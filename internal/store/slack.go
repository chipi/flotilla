package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
)

// Slack asks (G9, ADR-0006 decided 4, security T16): each plan waiting for a person gets one ask with a
// random nonce, posted in Slack by the connector. A click names the nonce; the ask is used once.

type SlackAsk struct {
	Nonce     string `json:"nonce"`
	CaseID    string `json:"case_id"`
	PlanHash  string `json:"plan_hash"`
	AskedAt   string `json:"asked_at"`
	SentAt    string `json:"sent_at"`
	DecidedAt string `json:"decided_at"`
	DecidedBy string `json:"decided_by"`
	Outcome   string `json:"outcome"`
}

var ErrAskUsed = errors.New("this Slack message was already answered")

// AskInSlack is the ask for this case's plan, made when it has none.
func (s *Store) AskInSlack(ctx context.Context, caseID, planHash string) (SlackAsk, error) {
	a, err := s.slackAsk(ctx, `SELECT nonce, case_id, plan_hash, asked_at, sent_at, decided_at, decided_by, outcome FROM slack_asks
		WHERE case_id = ? AND plan_hash = ?`, caseID, planHash)
	if err == nil || !errors.Is(err, sql.ErrNoRows) {
		return a, err
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return a, err
	}
	a = SlackAsk{Nonce: hex.EncodeToString(b), CaseID: caseID, PlanHash: planHash, AskedAt: s.now().Format(ts)}
	_, err = s.db.ExecContext(ctx, `INSERT INTO slack_asks (nonce, case_id, plan_hash, asked_at) VALUES (?, ?, ?, ?)`,
		a.Nonce, a.CaseID, a.PlanHash, a.AskedAt)
	return a, err
}

// SlackAskByNonce is the ask a click names; sql.ErrNoRows for a nonce flotillad never made.
func (s *Store) SlackAskByNonce(ctx context.Context, nonce string) (SlackAsk, error) {
	return s.slackAsk(ctx, `SELECT nonce, case_id, plan_hash, asked_at, sent_at, decided_at, decided_by, outcome FROM slack_asks
		WHERE nonce = ?`, nonce)
}

func (s *Store) slackAsk(ctx context.Context, q string, args ...any) (SlackAsk, error) {
	var a SlackAsk
	err := s.rdb.QueryRowContext(ctx, q, args...).Scan(&a.Nonce, &a.CaseID, &a.PlanHash, &a.AskedAt, &a.SentAt, &a.DecidedAt, &a.DecidedBy, &a.Outcome)
	return a, err
}

// MarkAskSent records that the connector posted the ask.
func (s *Store) MarkAskSent(ctx context.Context, nonce string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE slack_asks SET sent_at = ? WHERE nonce = ?`, s.now().Format(ts), nonce)
	return err
}

// UseAsk spends an ask: the first answer wins, any later one gets ErrAskUsed.
func (s *Store) UseAsk(ctx context.Context, nonce, by, outcome string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE slack_asks SET decided_at = ?, decided_by = ?, outcome = ? WHERE nonce = ? AND decided_at = ''`,
		s.now().Format(ts), by, outcome, nonce)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrAskUsed
	}
	return nil
}

// ErrCapReached is UseAskCapped's refusal: the day's approvals from Slack are at the cap.
var ErrCapReached = errors.New("the daily cap of approvals from Slack is used")

// UseAskCapped spends an ask as an approval only while fewer than max approvals since the given time exist. The
// count and the mark are one statement, so two clicks at cap-1 cannot both pass.
func (s *Store) UseAskCapped(ctx context.Context, nonce, by, outcome, since string, max int) error {
	res, err := s.db.ExecContext(ctx, `UPDATE slack_asks SET decided_at = ?, decided_by = ?, outcome = ? WHERE nonce = ? AND decided_at = ''
		AND (SELECT COUNT(*) FROM slack_asks WHERE outcome = 'approved' AND decided_at >= ?) < ?`,
		s.now().Format(ts), by, outcome, nonce, since, max)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return nil
	}
	var decided string
	if err := s.db.QueryRowContext(ctx, `SELECT decided_at FROM slack_asks WHERE nonce = ?`, nonce).Scan(&decided); err != nil {
		return err
	}
	if decided != "" {
		return ErrAskUsed
	}
	return ErrCapReached
}

// SlackApprovalsSince counts the plans approved from Slack since a time, for the daily cap.
func (s *Store) SlackApprovalsSince(ctx context.Context, since string) (int, error) {
	var n int
	err := s.rdb.QueryRowContext(ctx, `SELECT COUNT(*) FROM slack_asks WHERE outcome = 'approved' AND decided_at >= ?`, since).Scan(&n)
	return n, err
}

// UnsentAsks are asks the connector has not posted yet, oldest first.
func (s *Store) UnsentAsks(ctx context.Context) ([]SlackAsk, error) {
	rows, err := s.rdb.QueryContext(ctx, `SELECT nonce, case_id, plan_hash, asked_at, sent_at, decided_at, decided_by, outcome FROM slack_asks
		WHERE sent_at = '' AND decided_at = '' ORDER BY asked_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SlackAsk
	for rows.Next() {
		var a SlackAsk
		if err := rows.Scan(&a.Nonce, &a.CaseID, &a.PlanHash, &a.AskedAt, &a.SentAt, &a.DecidedAt, &a.DecidedBy, &a.Outcome); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ReopenAsk undoes UseAsk when the answer could not be applied (frozen, held): the plan still waits,
// and the same message may answer it later.
func (s *Store) ReopenAsk(ctx context.Context, nonce string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE slack_asks SET decided_at = '', decided_by = '', outcome = '' WHERE nonce = ?`, nonce)
	return err
}
