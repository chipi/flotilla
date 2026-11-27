package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// NoticeWatching is the notice kind of a watched case (ADR-0023).
const NoticeWatching = "watching"

// Notice is one prompt to the operator about a case. Sending it is a connector's job; the store keeps when it fell due,
// when it was sent and who acknowledged it.
type Notice struct {
	ID      int64  `json:"id"`
	CaseID  string `json:"case_id"`
	Kind    string `json:"kind"`
	N       int    `json:"n"` // 1 for the first notice of a watching period
	DueAt   string `json:"due_at"`
	SentAt  string `json:"sent_at"`
	AckedBy string `json:"acked_by"`
	AckedAt string `json:"acked_at"`
}

// Notices is a case's notices, oldest first.
func (s *Store) Notices(ctx context.Context, caseID string) ([]Notice, error) {
	rows, err := s.rdb.QueryContext(ctx, `SELECT id, case_id, kind, n, due_at, sent_at, acked_by, acked_at FROM notices WHERE case_id = ? ORDER BY id`, caseID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Notice
	for rows.Next() {
		var n Notice
		if err := rows.Scan(&n.ID, &n.CaseID, &n.Kind, &n.N, &n.DueAt, &n.SentAt, &n.AckedBy, &n.AckedAt); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// DueNotice records that notice n of a case fell due at dueAt: the row and its notice.due event, in one transaction.
func (s *Store) DueNotice(ctx context.Context, caseID, kind string, n int, dueAt string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var found int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM cases WHERE id = ?`, caseID).Scan(&found); err != nil {
		return err
	}
	if found == 0 {
		return ErrNoSuchCase
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO notices (case_id, kind, n, due_at) VALUES (?, ?, ?, ?)`, caseID, kind, n, dueAt); err != nil {
		return err
	}
	if err := addCaseEvent(ctx, tx, CaseEvent{CaseID: caseID, At: s.now().Format(ts), Kind: "notice.due", Actor: "flotillad",
		Detail: fmt.Sprintf("%s notice %d is due", kind, n)}); err != nil {
		return err
	}
	return tx.Commit()
}

// ErrNoNotice and ErrNoticeAcked are AckNotice's refusals: nothing has fallen due yet, or the newest notice is answered.
var (
	ErrNoNotice    = errors.New("the case has no notice to acknowledge")
	ErrNoticeAcked = errors.New("the newest notice is already acknowledged")
	ErrNotWatching = errors.New("the case is not watching")
)

// AckNotice acknowledges the case's newest notice as actor: acked_by and acked_at on its row, and a notice.acked event,
// in one transaction. The case timer's snooze reads acked_at.
func (s *Store) AckNotice(ctx context.Context, caseID, actor string) (Notice, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Notice{}, err
	}
	defer tx.Rollback()
	var status, statusAt string
	err = tx.QueryRowContext(ctx, `SELECT status, status_at FROM cases WHERE id = ?`, caseID).Scan(&status, &statusAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Notice{}, ErrNoSuchCase
	}
	if err != nil {
		return Notice{}, err
	}
	if status != "watching" {
		return Notice{}, ErrNotWatching
	}
	start, err := time.Parse(time.RFC3339Nano, statusAt)
	if err != nil {
		return Notice{}, err
	}
	var n Notice
	err = tx.QueryRowContext(ctx, `SELECT id, case_id, kind, n, due_at, sent_at, acked_by, acked_at FROM notices WHERE case_id = ? AND kind = ? ORDER BY id DESC LIMIT 1`,
		caseID, NoticeWatching).Scan(&n.ID, &n.CaseID, &n.Kind, &n.N, &n.DueAt, &n.SentAt, &n.AckedBy, &n.AckedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Notice{}, ErrNoNotice
	}
	if err != nil {
		return Notice{}, err
	}
	// the casetimer's own scope: a notice of this watching period fell due at or after the case began to be watched
	if due, err := time.Parse(time.RFC3339Nano, n.DueAt); err != nil || due.Before(start) {
		return Notice{}, ErrNoNotice
	}
	if n.AckedAt != "" {
		return n, ErrNoticeAcked
	}
	at := s.now().Format(ts)
	if _, err := tx.ExecContext(ctx, `UPDATE notices SET acked_by = ?, acked_at = ? WHERE id = ?`, actor, at, n.ID); err != nil {
		return Notice{}, err
	}
	if err := addCaseEvent(ctx, tx, CaseEvent{CaseID: caseID, At: at, Kind: "notice.acked", Actor: actor,
		Detail: fmt.Sprintf("%s notice %d acknowledged", n.Kind, n.N)}); err != nil {
		return Notice{}, err
	}
	n.AckedBy, n.AckedAt = actor, at
	return n, tx.Commit()
}
