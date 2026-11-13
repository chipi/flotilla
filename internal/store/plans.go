package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/chipi/flotilla/internal/cases"
)

var (
	ErrLocked     = errors.New("the target is locked by another case")
	ErrNotPlanned = errors.New("the case is not planned: policy decides a planned case only")
	ErrNoSuchHold = errors.New("no such hold, or it is already lifted")
)

// PlanRecord is a plan with policy's decision on it (RFC-0004 §5.1).
type PlanRecord struct {
	ID        int64             `json:"id"`
	CaseID    string            `json:"case_id"`
	RunID     string            `json:"run_id"`
	Action    string            `json:"action"`
	Params    map[string]string `json:"params"`
	PlanHash  string            `json:"plan_hash"`
	Outcome   string            `json:"outcome"`
	Autonomy  string            `json:"autonomy"`
	Would     string            `json:"would,omitempty"`
	Reasons   []string          `json:"reasons"`
	DecidedAt string            `json:"decided_at"`
}

// ApplyPlan records a decision and moves the planned case with it, in one transaction: to (with
// resolution, for a close), by policy. With lockTarget it takes the target's lock first; a lock held by
// another case refuses the whole thing (ErrLocked), so two plans can never both hold a target.
func (s *Store) ApplyPlan(ctx context.Context, p PlanRecord, to, resolution, target string, lockTarget bool) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var status string
	if err := tx.QueryRowContext(ctx, `SELECT status FROM cases WHERE id = ?`, p.CaseID).Scan(&status); errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNoSuchCase
	} else if err != nil {
		return 0, err
	}
	if status != cases.Planned {
		return 0, fmt.Errorf("%w (it is %s)", ErrNotPlanned, status)
	}
	at := s.now().Format(ts)
	if lockTarget {
		var holder string
		err := tx.QueryRowContext(ctx, `SELECT case_id FROM target_locks WHERE target = ?`, target).Scan(&holder)
		switch {
		case err == nil && holder != p.CaseID:
			return 0, fmt.Errorf("%w (%s)", ErrLocked, holder)
		case errors.Is(err, sql.ErrNoRows):
			if _, err := tx.ExecContext(ctx, `INSERT INTO target_locks (target, case_id, taken_at) VALUES (?, ?, ?)`, target, p.CaseID, at); err != nil {
				return 0, err
			}
		case err != nil:
			return 0, err
		}
	}
	params, _ := json.Marshal(p.Params)
	reasons, _ := json.Marshal(p.Reasons)
	res, err := tx.ExecContext(ctx, `INSERT INTO plans (case_id, run_id, action, params, plan_hash, outcome, autonomy, would, reasons, decided_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, p.CaseID, p.RunID, p.Action, string(params), p.PlanHash, p.Outcome, p.Autonomy, p.Would, string(reasons), at)
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	detail := fmt.Sprintf("plan %d %s: %s at %s", id, p.Action, p.Outcome, p.Autonomy)
	if p.Would != "" {
		detail += ", would " + p.Would
	}
	if err := s.moveCase(ctx, tx, p.CaseID, to, cases.ByPolicy, resolution, "flotillad", detail+"; "+strings.Join(p.Reasons, "; ")); err != nil {
		return 0, err // a refused move rolls the plan and the lock back with it
	}
	return id, tx.Commit()
}

// Plans is a case's plans, oldest first.
func (s *Store) Plans(ctx context.Context, caseID string) ([]PlanRecord, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, case_id, run_id, action, params, plan_hash, outcome, autonomy, would, reasons, decided_at
		FROM plans WHERE case_id = ? ORDER BY id`, caseID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PlanRecord
	for rows.Next() {
		var p PlanRecord
		var params, reasons string
		if err := rows.Scan(&p.ID, &p.CaseID, &p.RunID, &p.Action, &params, &p.PlanHash, &p.Outcome, &p.Autonomy, &p.Would, &reasons, &p.DecidedAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(params), &p.Params)
		_ = json.Unmarshal([]byte(reasons), &p.Reasons)
		out = append(out, p)
	}
	return out, rows.Err()
}

// LockHolder is the case holding the target's lock, "" when free.
func (s *Store) LockHolder(ctx context.Context, target string) (string, error) {
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT case_id FROM target_locks WHERE target = ?`, target).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// Hold stops automatic and proposed actions on a host ("host:mini") or a target until a time
// (RFC-0004 §5.1 step 4, `flotilla hold`).
type Hold struct {
	ID        int64   `json:"id"`
	Scope     string  `json:"scope"`
	Until     string  `json:"until"`
	Actor     string  `json:"actor"`
	Reason    string  `json:"reason"`
	CreatedAt string  `json:"created_at"`
	LiftedAt  *string `json:"lifted_at"`
	LiftedBy  *string `json:"lifted_by"`
}

func (h Hold) String() string {
	return fmt.Sprintf("%s until %s by %s (%s)", h.Scope, h.Until, h.Actor, h.Reason)
}

// AddHold records a hold and an event.
func (s *Store) AddHold(ctx context.Context, scope string, until time.Time, actor, via, reason string) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	at := s.now().Format(ts)
	res, err := tx.ExecContext(ctx, `INSERT INTO holds (scope, until, actor, reason, created_at) VALUES (?, ?, ?, ?, ?)`,
		scope, until.UTC().Format(ts), actor, reason, at)
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	if _, err := tx.ExecContext(ctx, `INSERT INTO events (at, actor, via, kind, subject, detail) VALUES (?, ?, ?, 'hold.added', ?, ?)`,
		at, actor, via, scope, fmt.Sprintf("%d until %s: %s", id, until.UTC().Format(time.RFC3339), reason)); err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

// LiftHold ends a hold early; lifting loosens, so the caller requires a reason (ADR-0014).
func (s *Store) LiftHold(ctx context.Context, id int64, actor, via, reason string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	at := s.now().Format(ts)
	var scope string
	if err := tx.QueryRowContext(ctx, `SELECT scope FROM holds WHERE id = ? AND lifted_at IS NULL`, id).Scan(&scope); errors.Is(err, sql.ErrNoRows) {
		return ErrNoSuchHold
	} else if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE holds SET lifted_at = ?, lifted_by = ? WHERE id = ?`, at, actor, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO events (at, actor, via, kind, subject, detail) VALUES (?, ?, ?, 'hold.lifted', ?, ?)`,
		at, actor, via, scope, fmt.Sprintf("%d: %s", id, reason)); err != nil {
		return err
	}
	return tx.Commit()
}

// Holds lists holds: the active ones at now, or every one with all.
func (s *Store) Holds(ctx context.Context, now time.Time, all bool) ([]Hold, error) {
	q := `SELECT id, scope, until, actor, reason, created_at, lifted_at, lifted_by FROM holds`
	var args []any
	if !all {
		q += ` WHERE lifted_at IS NULL AND until > ?`
		args = append(args, now.UTC().Format(ts))
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY id DESC LIMIT 200`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Hold
	for rows.Next() {
		var h Hold
		var la, lb sql.NullString
		if err := rows.Scan(&h.ID, &h.Scope, &h.Until, &h.Actor, &h.Reason, &h.CreatedAt, &la, &lb); err != nil {
			return nil, err
		}
		if la.Valid {
			h.LiftedAt, h.LiftedBy = &la.String, &lb.String
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// ExecutionCounts are the numbers policy's limits read (RFC-0004 §5.3), from the cases' timelines: an
// execution is a case moved to executing; a failed or unknown one, a case moved from executing to
// escalated. v1 targets are hosts ("host:<name>"), so a host's executions are its target's.
type ExecutionCounts struct {
	TargetToday, HostHour, PlatformHour, FailedHour, HostNewCases int
}

func (s *Store) Counts(ctx context.Context, action, target, host string, dayStart, now time.Time, incidentWindow time.Duration) (ExecutionCounts, error) {
	var c ExecutionCounts
	hour := now.Add(-time.Hour).UTC().Format(ts)
	q := func(dst *int, query string, args ...any) error {
		return s.db.QueryRowContext(ctx, query, args...).Scan(dst)
	}
	hostTarget := "host:" + host
	if err := q(&c.TargetToday, `SELECT COUNT(*) FROM case_events e JOIN cases c ON c.id = e.case_id
		WHERE e.kind = 'case.executing' AND c.target = ? AND e.at >= ?
		AND EXISTS (SELECT 1 FROM plans p WHERE p.case_id = c.id AND p.action = ?)`, target, dayStart.UTC().Format(ts), action); err != nil {
		return c, err
	}
	if err := q(&c.HostHour, `SELECT COUNT(*) FROM case_events e JOIN cases c ON c.id = e.case_id
		WHERE e.kind = 'case.executing' AND c.target = ? AND e.at >= ?`, hostTarget, hour); err != nil {
		return c, err
	}
	if err := q(&c.PlatformHour, `SELECT COUNT(*) FROM case_events WHERE kind = 'case.executing' AND at >= ?`, hour); err != nil {
		return c, err
	}
	if err := q(&c.FailedHour, `SELECT COUNT(*) FROM case_events WHERE kind = 'case.escalated' AND from_status = ? AND at >= ?`,
		cases.Executing, hour); err != nil {
		return c, err
	}
	if err := q(&c.HostNewCases, `SELECT COUNT(*) FROM cases WHERE target = ? AND opened_at >= ?`,
		hostTarget, now.Add(-incidentWindow).UTC().Format(ts)); err != nil {
		return c, err
	}
	return c, nil
}

// ActionRecord is what an action has done, the evidence a promotion is decided on (RFC-0004 §5.2):
// its plans by policy's outcome and, in shadow, by what they would have been; its executions verified
// and failed or unknown.
type ActionRecord struct {
	Plans    int            `json:"plans"`
	Outcomes map[string]int `json:"outcomes"`
	Would    map[string]int `json:"would"`
	Verified int            `json:"verified"`
	Failed   int            `json:"failed"`
}

func (s *Store) ActionRecord(ctx context.Context, action string) (ActionRecord, error) {
	r := ActionRecord{Outcomes: map[string]int{}, Would: map[string]int{}}
	rows, err := s.db.QueryContext(ctx, `SELECT outcome, would, COUNT(*) FROM plans WHERE action = ? GROUP BY outcome, would`, action)
	if err != nil {
		return r, err
	}
	defer rows.Close()
	for rows.Next() {
		var outcome, would string
		var n int
		if err := rows.Scan(&outcome, &would, &n); err != nil {
			return r, err
		}
		r.Plans += n
		r.Outcomes[outcome] += n
		if would != "" {
			r.Would[would] += n
		}
	}
	if err := rows.Err(); err != nil {
		return r, err
	}
	onAction := `EXISTS (SELECT 1 FROM plans p WHERE p.case_id = c.id AND p.action = ?)`
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM cases c WHERE c.resolution = ? AND `+onAction,
		cases.Verified, action).Scan(&r.Verified); err != nil {
		return r, err
	}
	err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM case_events e JOIN cases c ON c.id = e.case_id
		WHERE e.kind = 'case.escalated' AND e.from_status = ? AND `+onAction, cases.Executing, action).Scan(&r.Failed)
	return r, err
}
