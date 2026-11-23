package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/chipi/flotilla/internal/cases"
)

// HookCall is one verified hook call: the case it opens, or attaches to the open case with the same
// canonical key.
type HookCall struct {
	Hook     string
	Delivery string
	Case     NewCase
	Detail   string
	// Keep is how long a delivery id is remembered (twice the signature's window).
	Keep time.Duration
	// Close closes the case at once (the smoke hook): handed over, then closed as resolved_itself.
	Close bool
}

// HookResult says what a call did.
type HookResult struct {
	CaseID string
	// Replay: the delivery id was seen inside its window, and nothing happened.
	Replay bool
	// Opened: a new case; otherwise the call attached to the open case with the same key.
	Opened bool
}

// AcceptHookCall records a delivery id and opens (or attaches to) its case in one transaction, so a crash
// between the two never leaves a remembered id without its case or a case a retry would open again. A new
// case is handed over at once (the hook is its detector, its kind is known); a smoke hook's is then closed.
func (s *Store) AcceptHookCall(ctx context.Context, c HookCall) (HookResult, error) {
	if err := validNewCase(c.Case); err != nil {
		return HookResult{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return HookResult{}, err
	}
	defer tx.Rollback()
	now := s.now()
	at := now.Format(ts)
	actor := "hook:" + c.Hook
	if _, err := tx.ExecContext(ctx, `DELETE FROM hook_deliveries WHERE at < ?`, now.Add(-c.Keep).Format(ts)); err != nil {
		return HookResult{}, err
	}
	var seen string
	switch err := tx.QueryRowContext(ctx, `SELECT case_id FROM hook_deliveries WHERE hook = ? AND delivery = ?`, c.Hook, c.Delivery).Scan(&seen); {
	case err == nil:
		return HookResult{CaseID: seen, Replay: true}, tx.Commit()
	case !errors.Is(err, sql.ErrNoRows):
		return HookResult{}, err
	}
	var id string
	opened := false
	switch err := tx.QueryRowContext(ctx, `SELECT id FROM cases WHERE canonical_key = ? AND status <> ?`, c.Case.Canonical.String(), cases.Closed).Scan(&id); {
	case err == nil:
		if err := addSignal(ctx, tx, CaseEvent{CaseID: id, At: at, Kind: "signal.attached", Actor: actor, Detail: c.Detail, Data: c.Case.Data}); err != nil {
			return HookResult{}, err
		}
	case errors.Is(err, sql.ErrNoRows):
		if id, err = insertCase(ctx, tx, c.Case, now, actor, c.Detail); err != nil {
			return HookResult{}, err
		}
		opened = true
	default:
		return HookResult{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO hook_deliveries (hook, delivery, at, case_id) VALUES (?, ?, ?, ?)`, c.Hook, c.Delivery, at, id); err != nil {
		return HookResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return HookResult{}, err
	}
	if !opened {
		return HookResult{CaseID: id}, nil
	}
	if err := s.MoveCase(ctx, id, cases.HandedOver, cases.ByDetector, "", actor, "a hook with a known kind"); err != nil {
		return HookResult{CaseID: id, Opened: true}, err
	}
	if c.Close {
		if err := s.MoveCase(ctx, id, cases.Closed, cases.ByDetector, cases.ResolvedItself, actor, "the smoke hook closes its case on arrival"); err != nil {
			return HookResult{CaseID: id, Opened: true}, err
		}
	}
	return HookResult{CaseID: id, Opened: true}, nil
}

// HookActivity is one hook's calls since a time, from the events its handler writes (hook.accepted, hook.refused); a call cut off by
// the rate limit writes none, by design.
type HookActivity struct {
	Accepted, Refused int
	LastAt            string // its last call, either kind
	LastRefused       string // the reason of its last refusal
}

// HookActivities returns every hook's activity since a time, by hook name.
func (s *Store) HookActivities(ctx context.Context, since string) (map[string]HookActivity, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT actor, kind, at, detail FROM events
		WHERE kind IN ('hook.accepted', 'hook.refused') AND at >= ? ORDER BY seq`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]HookActivity{}
	for rows.Next() {
		var actor, kind, at, detail string
		if err := rows.Scan(&actor, &kind, &at, &detail); err != nil {
			return nil, err
		}
		name := strings.TrimPrefix(actor, "hook:")
		a := out[name]
		a.LastAt = at
		if kind == "hook.accepted" {
			a.Accepted++
		} else {
			a.Refused++
			a.LastRefused = detail
		}
		out[name] = a
	}
	return out, rows.Err()
}
