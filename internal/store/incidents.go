package store

import (
	"context"
	"database/sql"
	"errors"

	"github.com/chipi/flotilla/internal/cases"
)

// One case per incident (operator, 2026-11-13). RFC-0004 §3.5 matches a detection only against open
// cases and dismissed ones, so while a fault lasts every monitor run after a shadow close would open a
// new case. Instead a case closed as a recorded shadow plan stays the incident's case until the monitor
// that opened it reports its key clear (an incident.cleared event); detections in between are absorbed
// into it. Any other close (resolved itself, verified, by an operator) ends the incident: the next
// detection is a new case, which is what the recurrence rule counts.

// How a detection landed.
const (
	Opened   = "opened"   // a new case
	Attached = "attached" // to the open case with the same key
	Absorbed = "absorbed" // into the incident's case, closed as a shadow plan and not cleared
)

const (
	incidentCleared = "incident.cleared"
	signalAbsorbed  = "signal.absorbed"
)

// DetectIncident is OpenCase for a monitor's detection, with the incident rule above.
func (s *Store) DetectIncident(ctx context.Context, n NewCase, actor, detail string) (string, string, error) {
	if err := validNewCase(n); err != nil {
		return "", "", err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", "", err
	}
	defer tx.Rollback()
	now := s.now()
	at := now.Format(ts)
	ck := n.Canonical.String()
	var id string
	err = tx.QueryRowContext(ctx, `SELECT id FROM cases WHERE canonical_key = ? AND status <> ?`, ck, cases.Closed).Scan(&id)
	switch {
	case err == nil:
		if err := addCaseEvent(ctx, tx, CaseEvent{CaseID: id, At: at, Kind: "signal.attached", Actor: actor, Detail: detail}); err != nil {
			return "", "", err
		}
		return id, Attached, tx.Commit()
	case !errors.Is(err, sql.ErrNoRows):
		return "", "", err
	}
	err = tx.QueryRowContext(ctx, `SELECT c.id FROM cases c WHERE c.canonical_key = ? AND c.resolution = ?
		AND NOT EXISTS (SELECT 1 FROM case_events e WHERE e.case_id = c.id AND e.kind = ?)
		ORDER BY c.opened_at DESC LIMIT 1`, ck, cases.ShadowPlan, incidentCleared).Scan(&id)
	switch {
	case err == nil:
		if err := addCaseEvent(ctx, tx, CaseEvent{CaseID: id, At: at, Kind: signalAbsorbed, Actor: actor, Detail: detail}); err != nil {
			return "", "", err
		}
		return id, Absorbed, tx.Commit()
	case !errors.Is(err, sql.ErrNoRows):
		return "", "", err
	}
	if id, err = insertCase(ctx, tx, n, now, actor, detail); err != nil {
		return "", "", err
	}
	return id, Opened, tx.Commit()
}

// ClearIncidents ends the incidents a monitor opened whose keys its latest successful run no longer
// reports: each case it opened, closed as a shadow plan and not cleared, whose key is not in present,
// gets an incident.cleared event (a kind the monitor stopped detecting is no longer reported, so its
// incidents clear too). It returns the cases it cleared.
func (s *Store) ClearIncidents(ctx context.Context, monitor string, present map[string]bool) ([]string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT c.id, c.canonical_key FROM cases c WHERE c.resolution = ?
		AND NOT EXISTS (SELECT 1 FROM case_events e WHERE e.case_id = c.id AND e.kind = ?)
		AND EXISTS (SELECT 1 FROM case_events d WHERE d.case_id = c.id AND d.kind = 'case.detected' AND d.actor = ?)`,
		cases.ShadowPlan, incidentCleared, monitor)
	if err != nil {
		return nil, err
	}
	type open struct{ id, key string }
	var found []open
	for rows.Next() {
		var o open
		if err := rows.Scan(&o.id, &o.key); err != nil {
			rows.Close()
			return nil, err
		}
		found = append(found, o)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	at := s.now().Format(ts)
	var cleared []string
	for _, o := range found {
		if present[o.key] {
			continue
		}
		if err := addCaseEvent(ctx, tx, CaseEvent{CaseID: o.id, At: at, Kind: incidentCleared, Actor: monitor,
			Detail: "the monitor no longer reports it"}); err != nil {
			return nil, err
		}
		cleared = append(cleared, o.id)
	}
	return cleared, tx.Commit()
}
