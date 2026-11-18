package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/chipi/flotilla/internal/cases"
)

// MaxStageChanges bounds how often an acting routine may hand a case on (RFC-0013 §2: fix -> clarify ->
// fix is two); the dispatcher escalates the third instead of moving the case.
const MaxStageChanges = 2

// ErrStageLimit: the case was handed on MaxStageChanges times already.
var ErrStageLimit = errors.New("the case has been handed on as often as it may be")

// StageCase moves a handed-over case to another stage (RFC-0013 §2): the routine for (kind, stage) acts
// on it next. Its status stays handed_over; a case.staged event records who, why and the data the next
// routine reads. Only a handed-over case held by `holder` moves, and at most MaxStageChanges times.
func (s *Store) StageCase(ctx context.Context, id, stage, holder, detail, data string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var status, from, held string
	var changes int
	err = tx.QueryRowContext(ctx, `SELECT status, stage, holder, stage_changes FROM cases WHERE id = ?`, id).Scan(&status, &from, &held, &changes)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNoSuchCase
	} else if err != nil {
		return err
	}
	switch {
	case status != cases.HandedOver:
		return fmt.Errorf("%w: case %s is %s, not handed_over", ErrBadTransition, id, status)
	case held != holder:
		return fmt.Errorf("%w: case %s is held by %q, not %q", ErrBadTransition, id, held, holder)
	case stage == from:
		return fmt.Errorf("%w: case %s is already at stage %q", ErrBadTransition, id, stage)
	case changes >= MaxStageChanges:
		return ErrStageLimit
	}
	if _, err := tx.ExecContext(ctx, `UPDATE cases SET stage = ?, stage_changes = stage_changes + 1 WHERE id = ?`, stage, id); err != nil {
		return err
	}
	ev := CaseEvent{CaseID: id, At: s.now().Format(ts), Kind: "case.staged", Actor: holder, By: cases.ByAct,
		From: stageName(from), To: stageName(stage), Detail: detail, Data: data}
	if err := addCaseEvent(ctx, tx, ev); err != nil {
		return err
	}
	return tx.Commit()
}

// stageName is how a stage reads on the timeline: the empty stage is the kind's first routine.
func stageName(stage string) string {
	if stage == "" {
		return "first"
	}
	return stage
}

// AddCaseSpend adds an acting run's spend and time to its case: the case's runs share one budget and one
// time limit (RFC-0013 §2).
func (s *Store) AddCaseSpend(ctx context.Context, id string, usd, seconds float64) error {
	if usd <= 0 && seconds <= 0 {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `UPDATE cases SET spent_usd = spent_usd + ?, spent_s = spent_s + ? WHERE id = ?`, max(usd, 0), max(seconds, 0), id)
	return err
}

// CaseOfRun is the case a run of a resumable routine was recorded on (its run.started event), "" when none:
// the store has no run-to-case link (ADR-0019).
func (s *Store) CaseOfRun(ctx context.Context, runID string) (string, error) {
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT case_id FROM case_events WHERE kind = 'run.started' AND detail = ? ORDER BY seq DESC LIMIT 1`, "run "+runID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// StageData is the data of the case's latest case.staged event: what the routine that handed it on left
// for the next one ("" when it was never handed on).
func (s *Store) StageData(ctx context.Context, id string) (string, error) {
	var data string
	err := s.db.QueryRowContext(ctx, `SELECT data FROM case_events WHERE case_id = ? AND kind = 'case.staged' ORDER BY seq DESC LIMIT 1`, id).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return data, err
}

// KeySpend is what the acting runs of every case with this canonical key, opened since `since`, have spent
// together (RFC-0013 §2): the case limit holds for the bug, not for one case, so closing a case and the
// next detection opening a new one does not start the limit over.
func (s *Store) KeySpend(ctx context.Context, key, since string) (usd, seconds float64, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(spent_usd), 0), COALESCE(SUM(spent_s), 0) FROM cases
		WHERE canonical_key = ? AND opened_at >= ?`, key, since).Scan(&usd, &seconds)
	return usd, seconds, err
}
