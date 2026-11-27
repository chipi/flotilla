package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Slack asks (G9, ADR-0006 decided 4, security T16; ADR-0023 §3): each plan waiting for a person, and each notice about
// a watched case, gets one ask with a random nonce, posted in Slack by the connector. A click names the nonce and an
// answer id; the ask keeps the ids it offered, and a click for any other is refused. An ask is spent by its first
// click. A notice's ask is also spent when a newer notice's ask replaces it, when the notice is acknowledged anywhere and
// when the case leaves watching: the message of a spent ask answers nothing.

type SlackAsk struct {
	Nonce     string `json:"nonce"`
	CaseID    string `json:"case_id"`
	PlanHash  string `json:"plan_hash"`
	AskedAt   string `json:"asked_at"`
	SentAt    string `json:"sent_at"`
	DecidedAt string `json:"decided_at"`
	DecidedBy string `json:"decided_by"`
	Outcome   string `json:"outcome"`
	Kind      string `json:"kind"`    // plan | notice
	Answers   string `json:"answers"` // JSON list of the answer ids the message offered
}

// Ask kinds, and the plan_hash prefix of a notice's ask.
const (
	AskPlan         = "plan"
	AskNotice       = "notice"
	askNoticePrefix = "notice:"
)

const askCols = `nonce, case_id, plan_hash, asked_at, sent_at, decided_at, decided_by, outcome, kind, answers`

func scanAsk(r interface{ Scan(...any) error }) (SlackAsk, error) {
	var a SlackAsk
	err := r.Scan(&a.Nonce, &a.CaseID, &a.PlanHash, &a.AskedAt, &a.SentAt, &a.DecidedAt, &a.DecidedBy, &a.Outcome, &a.Kind, &a.Answers)
	return a, err
}

// Offers is whether the ask offered this answer id.
func (a SlackAsk) Offers(id string) bool {
	var ids []string
	if json.Unmarshal([]byte(a.Answers), &ids) != nil {
		return false
	}
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

// NoticeID is the notice a notice's ask is about, 0 for a plan's.
func (a SlackAsk) NoticeID() int64 {
	if a.Kind != AskNotice {
		return 0
	}
	id, _ := strconv.ParseInt(strings.TrimPrefix(a.PlanHash, askNoticePrefix), 10, 64)
	return id
}

var ErrAskUsed = errors.New("this Slack message was already answered")

// AskInSlack is the ask for this case's plan, made when it has none.
func (s *Store) AskInSlack(ctx context.Context, caseID, planHash string) (SlackAsk, error) {
	return s.askInSlack(ctx, caseID, planHash, AskPlan, []string{"approve", "reject"})
}

// AskNoticeInSlack is the ask for one notice of a case, made when it has none; answers are the ids it offers.
func (s *Store) AskNoticeInSlack(ctx context.Context, caseID string, noticeID int64, answers []string) (SlackAsk, error) {
	return s.askInSlack(ctx, caseID, askNoticePrefix+strconv.FormatInt(noticeID, 10), AskNotice, answers)
}

func (s *Store) askInSlack(ctx context.Context, caseID, planHash, kind string, answers []string) (SlackAsk, error) {
	a, err := s.slackAsk(ctx, `SELECT `+askCols+` FROM slack_asks
		WHERE case_id = ? AND plan_hash = ?`, caseID, planHash)
	if err == nil || !errors.Is(err, sql.ErrNoRows) {
		return a, err
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return a, err
	}
	ids, err := json.Marshal(answers)
	if err != nil {
		return a, err
	}
	a = SlackAsk{Nonce: hex.EncodeToString(b), CaseID: caseID, PlanHash: planHash, AskedAt: s.now().Format(ts), Kind: kind, Answers: string(ids)}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return a, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `INSERT INTO slack_asks (nonce, case_id, plan_hash, asked_at, kind, answers) VALUES (?, ?, ?, ?, ?, ?)`,
		a.Nonce, a.CaseID, a.PlanHash, a.AskedAt, a.Kind, a.Answers); err != nil {
		return a, err
	}
	if kind == AskNotice { // a newer notice's message replaces the earlier one's: that nonce is spent
		if err := closeNoticeAsks(ctx, tx, caseID, a.Nonce, "flotillad", "superseded", a.AskedAt); err != nil {
			return a, err
		}
	}
	return a, tx.Commit()
}

// SlackAskByNonce is the ask a click names; sql.ErrNoRows for a nonce flotillad never made.
func (s *Store) SlackAskByNonce(ctx context.Context, nonce string) (SlackAsk, error) {
	return s.slackAsk(ctx, `SELECT `+askCols+` FROM slack_asks
		WHERE nonce = ?`, nonce)
}

func (s *Store) slackAsk(ctx context.Context, q string, args ...any) (SlackAsk, error) {
	return scanAsk(s.rdb.QueryRowContext(ctx, q, args...))
}

// MarkAskSent records that the connector posted the ask. A notice's ask also marks the notice sent, with a notice.sent
// event on its case, in the same transaction.
func (s *Store) MarkAskSent(ctx context.Context, nonce string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	at := s.now().Format(ts)
	if _, err := tx.ExecContext(ctx, `UPDATE slack_asks SET sent_at = ? WHERE nonce = ?`, at, nonce); err != nil {
		return err
	}
	a, err := scanAsk(tx.QueryRowContext(ctx, `SELECT `+askCols+` FROM slack_asks WHERE nonce = ?`, nonce))
	if errors.Is(err, sql.ErrNoRows) {
		return tx.Commit()
	}
	if err != nil {
		return err
	}
	if id := a.NoticeID(); id > 0 {
		var n, sent int
		if err := tx.QueryRowContext(ctx, `SELECT n, sent_at <> '' FROM notices WHERE id = ?`, id).Scan(&n, &sent); err != nil {
			return err
		}
		if sent == 0 {
			if _, err := tx.ExecContext(ctx, `UPDATE notices SET sent_at = ? WHERE id = ?`, at, id); err != nil {
				return err
			}
			if err := addCaseEvent(ctx, tx, CaseEvent{CaseID: a.CaseID, At: at, Kind: "notice.sent", Actor: "flotillad",
				Detail: fmt.Sprintf("notice %d offered to the Slack connector", n)}); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// closeNoticeAsks spends every open notice ask of a case but the one named, in the caller's transaction: its notice was
// acknowledged, a newer one replaced it, or the case left watching, so no message of it may answer anything.
func closeNoticeAsks(ctx context.Context, tx *sql.Tx, caseID, except, by, outcome, at string) error {
	_, err := tx.ExecContext(ctx, `UPDATE slack_asks SET decided_at = ?, decided_by = ?, outcome = ? WHERE case_id = ? AND kind = ? AND decided_at = '' AND nonce <> ?`,
		at, by, outcome, caseID, AskNotice, except)
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
	rows, err := s.rdb.QueryContext(ctx, `SELECT `+askCols+` FROM slack_asks
		WHERE sent_at = '' AND decided_at = '' ORDER BY asked_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SlackAsk
	for rows.Next() {
		a, err := scanAsk(rows)
		if err != nil {
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
