package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/chipi/flotilla/internal/cases"
)

var (
	ErrNoSuchCase    = errors.New("no such case")
	ErrBadTransition = errors.New("transition refused")
)

// Key is one of a case's keys (RFC-0004 §3.5): a (kind, value) pair computed by code.
type Key struct {
	Kind  string `json:"kind"`
	Value string `json:"value"`
}

func (k Key) String() string { return k.Kind + "=" + k.Value }

// NewCase is a detection: what opens a case, or attaches to the open one with the same canonical key.
type NewCase struct {
	Kind      string
	Target    string
	Project   string
	Urgency   int
	Keys      []Key
	Canonical Key // one of Keys: "one open case per canonical key"
}

type Case struct {
	ID           string  `json:"id"`
	Kind         string  `json:"kind"`
	Target       string  `json:"target"`
	CanonicalKey string  `json:"canonical_key"`
	Project      string  `json:"project"`
	Urgency      int     `json:"urgency"`
	Status       string  `json:"status"`
	Resolution   string  `json:"resolution"`
	Holder       string  `json:"holder"`
	OpenedAt     string  `json:"opened_at"`
	StatusAt     string  `json:"status_at"`
	ClosedAt     *string `json:"closed_at"`
	Keys         []Key   `json:"keys,omitempty"`
}

type CaseEvent struct {
	Seq    int64  `json:"seq"`
	CaseID string `json:"case_id"`
	At     string `json:"at"`
	Kind   string `json:"kind"`
	Actor  string `json:"actor"`
	By     string `json:"by,omitempty"`
	From   string `json:"from,omitempty"`
	To     string `json:"to,omitempty"`
	Detail string `json:"detail,omitempty"`
}

func newCaseID(day string) (string, error) {
	b := make([]byte, 5)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "case-" + day + "-" + hex.EncodeToString(b), nil
}

func validNewCase(n NewCase) error {
	if n.Kind == "" || n.Target == "" {
		return fmt.Errorf("a case needs a kind and a target")
	}
	if n.Canonical.Kind == "" || n.Canonical.Value == "" {
		return fmt.Errorf("a case needs a canonical key")
	}
	for _, k := range n.Keys {
		if k == n.Canonical {
			return nil
		}
	}
	return fmt.Errorf("the canonical key %s is not one of the case's keys", n.Canonical)
}

// OpenCase opens a case at detected, or, when a case with the same canonical key is open, attaches the
// detection to it as an event: a storm is one case. opened reports which.
func (s *Store) OpenCase(ctx context.Context, n NewCase, actor, detail string) (id string, opened bool, err error) {
	if err := validNewCase(n); err != nil {
		return "", false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", false, err
	}
	defer tx.Rollback()
	now := s.now()
	at := now.Format(ts)
	ck := n.Canonical.String()
	err = tx.QueryRowContext(ctx, `SELECT id FROM cases WHERE canonical_key = ? AND status <> ?`, ck, cases.Closed).Scan(&id)
	switch {
	case err == nil:
		if err := addCaseEvent(ctx, tx, CaseEvent{CaseID: id, At: at, Kind: "signal.attached", Actor: actor, Detail: detail}); err != nil {
			return "", false, err
		}
		return id, false, tx.Commit()
	case !errors.Is(err, sql.ErrNoRows):
		return "", false, err
	}
	if id, err = newCaseID(now.Format("20060102")); err != nil {
		return "", false, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO cases (id, kind, target, canonical_key, project, urgency, status, opened_at, status_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, id, n.Kind, n.Target, ck, n.Project, n.Urgency, cases.Detected, at, at); err != nil {
		return "", false, err
	}
	for _, k := range n.Keys {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO case_keys (case_id, kind, value) VALUES (?, ?, ?)`, id, k.Kind, k.Value); err != nil {
			return "", false, err
		}
	}
	if err := addCaseEvent(ctx, tx, CaseEvent{CaseID: id, At: at, Kind: "case.detected", Actor: actor, To: cases.Detected, Detail: detail}); err != nil {
		return "", false, err
	}
	return id, true, tx.Commit()
}

// MoveCase changes a case's status if the transition table allows it (cases.Check): the move, the role
// making it, and for a close its resolution. A refused move changes nothing and is recorded as an
// event, so a job trying what it may not shows on the case's timeline.
func (s *Store) MoveCase(ctx context.Context, id, to, by, resolution, actor, detail string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	moveErr := s.moveCase(ctx, tx, id, to, by, resolution, actor, detail)
	if moveErr != nil && !errors.Is(moveErr, ErrBadTransition) {
		return moveErr
	}
	if err := tx.Commit(); err != nil { // a refused move commits its transition.refused event
		return err
	}
	return moveErr
}

// moveCase is MoveCase inside the caller's transaction. On a refused move it writes the
// transition.refused event and returns ErrBadTransition; the caller decides whether to commit.
//
// The per-target lock (RFC-0004 §3.5) is released when the case closes, when a proposal is rejected or
// expires (awaiting_approval -> escalated: nothing ran) and when the operator reassigns an escalated
// case. An execution that failed or is unknown (executing or checking -> escalated) keeps it: the target
// stays held until the operator clears the case.
func (s *Store) moveCase(ctx context.Context, tx *sql.Tx, id, to, by, resolution, actor, detail string) error {
	var from string
	if err := tx.QueryRowContext(ctx, `SELECT status FROM cases WHERE id = ?`, id).Scan(&from); errors.Is(err, sql.ErrNoRows) {
		return ErrNoSuchCase
	} else if err != nil {
		return err
	}
	at := s.now().Format(ts)
	if cerr := cases.Check(from, to, by, resolution); cerr != nil {
		if err := addCaseEvent(ctx, tx, CaseEvent{CaseID: id, At: at, Kind: "transition.refused", Actor: actor, By: by,
			From: from, To: to, Detail: cerr.Error()}); err != nil {
			return err
		}
		return fmt.Errorf("%w: %v", ErrBadTransition, cerr)
	}
	var closedAt any
	if to == cases.Closed {
		closedAt = at
	}
	if _, err := tx.ExecContext(ctx, `UPDATE cases SET status = ?, resolution = ?, status_at = ?, closed_at = ? WHERE id = ?`,
		to, resolution, at, closedAt, id); err != nil {
		return err
	}
	d := detail
	if resolution != "" {
		d = strings.TrimSpace(resolution + ": " + detail)
	}
	if err := addCaseEvent(ctx, tx, CaseEvent{CaseID: id, At: at, Kind: "case." + to, Actor: actor, By: by, From: from, To: to, Detail: d}); err != nil {
		return err
	}
	if to == cases.Closed || (from == cases.AwaitingApproval && to == cases.Escalated) || (from == cases.Escalated && to == cases.HandedOver) {
		if _, err := tx.ExecContext(ctx, `DELETE FROM target_locks WHERE case_id = ?`, id); err != nil {
			return err
		}
	}
	return nil
}

// AddCaseEvent appends to a case's timeline, closed or not (an outcome check days later, a recurrence,
// the operator's feedback). It never changes the status.
func (s *Store) AddCaseEvent(ctx context.Context, id, kind, actor, detail string) error {
	if strings.HasPrefix(kind, "case.") || kind == "transition.refused" {
		return fmt.Errorf("%q is the store's own event kind", kind)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM cases WHERE id = ?`, id).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return ErrNoSuchCase
	}
	if err := addCaseEvent(ctx, tx, CaseEvent{CaseID: id, At: s.now().Format(ts), Kind: kind, Actor: actor, Detail: detail}); err != nil {
		return err
	}
	return tx.Commit()
}

func addCaseEvent(ctx context.Context, tx *sql.Tx, e CaseEvent) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO case_events (case_id, at, kind, actor, by_role, from_status, to_status, detail)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, e.CaseID, e.At, e.Kind, e.Actor, e.By, e.From, e.To, e.Detail)
	return err
}

const caseCols = `id, kind, target, canonical_key, project, urgency, status, resolution, holder, opened_at, status_at, closed_at`

func scanCase(sc interface{ Scan(...any) error }) (Case, error) {
	var c Case
	var closed sql.NullString
	err := sc.Scan(&c.ID, &c.Kind, &c.Target, &c.CanonicalKey, &c.Project, &c.Urgency, &c.Status, &c.Resolution, &c.Holder,
		&c.OpenedAt, &c.StatusAt, &closed)
	if closed.Valid {
		c.ClosedAt = &closed.String
	}
	return c, err
}

// Case returns one case with its keys.
func (s *Store) Case(ctx context.Context, id string) (Case, error) {
	c, err := scanCase(s.db.QueryRowContext(ctx, `SELECT `+caseCols+` FROM cases WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Case{}, ErrNoSuchCase
	}
	if err != nil {
		return Case{}, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT kind, value FROM case_keys WHERE case_id = ? ORDER BY kind, value`, id)
	if err != nil {
		return Case{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var k Key
		if err := rows.Scan(&k.Kind, &k.Value); err != nil {
			return Case{}, err
		}
		c.Keys = append(c.Keys, k)
	}
	return c, rows.Err()
}

// CaseFilter narrows Cases. Open means every status but closed; Status, when set, is one status.
type CaseFilter struct {
	Open   bool
	Status string
	Kind   string
	Limit  int
}

// Cases lists cases, the most recently changed first.
func (s *Store) Cases(ctx context.Context, f CaseFilter) ([]Case, error) {
	q := `SELECT ` + caseCols + ` FROM cases WHERE 1 = 1`
	var args []any
	if f.Open {
		q += ` AND status <> ?`
		args = append(args, cases.Closed)
	}
	if f.Status != "" {
		q += ` AND status = ?`
		args = append(args, f.Status)
	}
	if f.Kind != "" {
		q += ` AND kind = ?`
		args = append(args, f.Kind)
	}
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 100
	}
	q += ` ORDER BY status_at DESC, id LIMIT ?`
	args = append(args, f.Limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Case
	for rows.Next() {
		c, err := scanCase(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// CaseEvents is a case's timeline, oldest first.
func (s *Store) CaseEvents(ctx context.Context, id string) ([]CaseEvent, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT seq, case_id, at, kind, actor, by_role, from_status, to_status, detail
		FROM case_events WHERE case_id = ? ORDER BY seq`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CaseEvent
	for rows.Next() {
		var e CaseEvent
		if err := rows.Scan(&e.Seq, &e.CaseID, &e.At, &e.Kind, &e.Actor, &e.By, &e.From, &e.To, &e.Detail); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
