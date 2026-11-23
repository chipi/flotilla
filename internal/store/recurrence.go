package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/chipi/flotilla/internal/cases"
)

// The recurrence rule (G14, the operator 2026-11-14): a new case of the same kind on the same target,
// opened within RecurrenceWindow of an earlier one closing, is a recurrence of it. The new case links to
// the earlier one, both timelines say so, and the Autonomy page counts them per kind. A case closed as a
// recorded plan is still the incident's case (incidents.go), so it absorbs detections instead.
const RecurrenceWindow = 7 * 24 * time.Hour

func linkRecurrence(ctx context.Context, tx *sql.Tx, id string, n NewCase, now time.Time) error {
	var prev, closedAt, resolution string
	err := tx.QueryRowContext(ctx, `SELECT id, closed_at, resolution FROM cases WHERE kind = ? AND target = ? AND id <> ?
		AND status = ? AND closed_at >= ? ORDER BY closed_at DESC LIMIT 1`,
		n.Kind, n.Target, id, cases.Closed, now.Add(-RecurrenceWindow).Format(ts)).Scan(&prev, &closedAt, &resolution)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE cases SET recurrence_of = ? WHERE id = ?`, prev, id); err != nil {
		return err
	}
	at := now.Format(ts)
	if err := addCaseEvent(ctx, tx, CaseEvent{CaseID: id, At: at, Kind: "case.recurrence", Actor: "flotillad", By: cases.ByPolicy,
		Detail: fmt.Sprintf("recurs %s, closed %s as %s", prev, closedAt, resolution)}); err != nil {
		return err
	}
	return addCaseEvent(ctx, tx, CaseEvent{CaseID: prev, At: at, Kind: "case.recurred", Actor: "flotillad", By: cases.ByPolicy,
		Detail: "came back as " + id})
}

// Recurrences is how many cases of each kind came back within the window, since the given time.
func (s *Store) Recurrences(ctx context.Context, since string) (map[string]int, error) {
	rows, err := s.rdb.QueryContext(ctx, `SELECT kind, COUNT(*) FROM cases WHERE recurrence_of <> '' AND opened_at >= ? GROUP BY kind`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var k string
		var n int
		if err := rows.Scan(&k, &n); err != nil {
			return nil, err
		}
		out[k] = n
	}
	return out, rows.Err()
}
