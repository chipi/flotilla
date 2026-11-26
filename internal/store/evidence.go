package store

import (
	"context"
	"time"
)

// The evidence a level needs (G13, the operator 2026-11-14): an action is ready for its next level
// after PromotionNeed clean plans in a row, with no wrong verdict and no failed execution in the last
// PromotionHold. A person still promotes; this only says how far the record has come.
const (
	PromotionNeed = 5
	PromotionHold = 7 * 24 * time.Hour
)

// Streak is an action's run of clean plans, newest first.
type Streak struct {
	Clean   int    `json:"clean"`    // clean plans since the last bad one, at most PromotionNeed
	BadAt   string `json:"bad_at"`   // when the newest bad plan was decided ("" for none)
	BadWhat string `json:"bad_what"` // what made it bad: wrong, failed or rejected
	Held    bool   `json:"held"`     // a bad plan inside PromotionHold
}

// Ready is whether the record is enough for the next level.
func (s Streak) Ready() bool { return s.Clean >= PromotionNeed && !s.Held }

// planWindows gives each plan the stretch of its case's timeline that is its own: from the event its decision
// wrote (ApplyPlan's detail names "plan <id> <action>:") up to the next plan's, so a rejection, a failed
// execution or a verification is credited to the plan the case was on when it happened (RFC-0013 amendment, 2026-11-23).
// A plan whose event cannot be found (a row written by hand) starts at the first event at or after it was
// decided. It takes the action once (?): only cases that planned it are read.
const planWindows = `WITH pstart AS (SELECT p.id, p.case_id, p.action, p.outcome, p.decided_at,
		COALESCE((SELECT MIN(e.seq) FROM case_events e WHERE e.case_id = p.case_id AND instr(e.detail, 'plan ' || p.id || ' ' || p.action || ':') > 0),
			(SELECT MIN(e.seq) FROM case_events e WHERE e.case_id = p.case_id AND e.at >= p.decided_at), 0) AS s0
		FROM plans p WHERE p.case_id IN (SELECT case_id FROM plans WHERE action = ?)),
	pwin AS (SELECT a.*, COALESCE((SELECT MIN(b.s0) FROM pstart b WHERE b.case_id = a.case_id AND b.id > a.id), 9223372036854775807) AS s1 FROM pstart a) `

// What happened to a plan, as events in its window (e is a case_events row, p a pwin row). A verified close
// is the case's, so its detail starts with the resolution; a check that hands the case on to its plan's then
// writes "verified:" too, with no resolution, which the case-level reading never counted.
const (
	inWindow      = `e.case_id = p.case_id AND e.seq >= p.s0 AND e.seq < p.s1`
	failedEvent   = `e.kind = 'case.escalated' AND e.from_status = 'executing'`
	rejectedEvent = `e.kind = 'case.escalated' AND e.from_status = 'awaiting_approval'`
	verifiedEvent = `e.detail LIKE 'verified:%' AND (e.kind = 'case.closed' OR (e.kind = 'case.handed_over' AND e.by_role = 'check'))`
)

// PromotionStreak reads an action's plans newest first. A plan is clean when a recorded plan was judged
// right, or a proposed or automatic one was approved as it stood and verified. It is bad when its
// diagnosis was judged wrong, its execution failed, or a person rejected it. Verified, failed and rejected are
// the plan's own (planWindows), not the case's: in a case of two plans (RFC-0013's then) the second plan's
// rejection does not count against the first. A verdict is on the case, so it still stands for each of its plans. A plan still open, or a
// recorded plan nobody judged, neither counts nor breaks the run.
func (s *Store) PromotionStreak(ctx context.Context, action string) (Streak, error) {
	var st Streak
	rows, err := s.rdb.QueryContext(ctx, planWindows+`SELECT p.outcome, p.decided_at,
		EXISTS (SELECT 1 FROM case_events e WHERE `+inWindow+` AND `+verifiedEvent+`),
		EXISTS (SELECT 1 FROM case_feedback f WHERE f.case_id = p.case_id AND f.verdict = ?),
		EXISTS (SELECT 1 FROM case_feedback f WHERE f.case_id = p.case_id AND f.verdict = ?),
		EXISTS (SELECT 1 FROM case_events e WHERE `+inWindow+` AND `+failedEvent+`),
		EXISTS (SELECT 1 FROM case_events e WHERE `+inWindow+` AND `+rejectedEvent+`)
		FROM pwin p WHERE p.action = ? ORDER BY p.id DESC LIMIT 500`,
		action, VerdictWrong, VerdictRight, action)
	if err != nil {
		return st, err
	}
	defer rows.Close()
	cutoff := s.now().Add(-PromotionHold)
	for rows.Next() {
		var outcome, at string
		var verified, wrong, right, failed, rejected bool
		if err := rows.Scan(&outcome, &at, &verified, &wrong, &right, &failed, &rejected); err != nil {
			return st, err
		}
		bad := ""
		switch {
		case wrong:
			bad = "wrong"
		case failed:
			bad = "failed"
		case rejected:
			bad = "rejected"
		}
		if bad != "" {
			st.BadAt, st.BadWhat = at, bad
			if t, err := time.Parse(ts, at); err == nil && t.After(cutoff) {
				st.Held = true
			}
			break
		}
		clean := (outcome == "record" && right) || (outcome != "record" && verified)
		if clean && st.Clean < PromotionNeed {
			st.Clean++
		}
	}
	return st, rows.Err()
}

// ActionPlanners is, for each action, the live routines whose runs have planned it, sorted: the routines whose own levels
// and caps a promotion of the action has to get past. A shadow's plan never reaches policy, so it names no planner.
func (s *Store) ActionPlanners(ctx context.Context) (map[string][]string, error) {
	rows, err := s.rdb.QueryContext(ctx, `SELECT DISTINCT p.action, r.routine FROM plans p JOIN runs r ON r.id = p.run_id WHERE r.`+liveRuns+`
		ORDER BY p.action, r.routine`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var action, routine string
		if err := rows.Scan(&action, &routine); err != nil {
			return nil, err
		}
		out[action] = append(out[action], routine)
	}
	return out, rows.Err()
}

// UnjudgedRecorded is the recorded plans (a case's latest plan, whose outcome is record) nobody has judged yet, newest first,
// at most limit, and how many there are in all: a record-level action earns nothing until a person says Right or Wrong. It
// reads the plan's outcome, not the case's resolution: a watched case (ADR-0023) is still watching, or closed as resolved
// itself, and counts as the plan it recorded.
func (s *Store) UnjudgedRecorded(ctx context.Context, limit int) ([]PlanRecord, int, error) {
	const q = `FROM plans p JOIN cases c ON c.id = p.case_id
		WHERE c.status IN ('closed', 'watching') AND p.outcome = 'record'
		AND p.id = (SELECT MAX(id) FROM plans WHERE case_id = p.case_id)
		AND NOT EXISTS (SELECT 1 FROM case_feedback f WHERE f.case_id = p.case_id)`
	var n int
	if err := s.rdb.QueryRowContext(ctx, `SELECT COUNT(*) `+q).Scan(&n); err != nil {
		return nil, 0, err
	}
	rows, err := s.rdb.QueryContext(ctx, `SELECT p.id, p.case_id, p.run_id, p.action, p.decided_at `+q+` ORDER BY p.decided_at DESC, p.id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []PlanRecord
	for rows.Next() {
		var p PlanRecord
		if err := rows.Scan(&p.ID, &p.CaseID, &p.RunID, &p.Action, &p.DecidedAt); err != nil {
			return nil, 0, err
		}
		out = append(out, p)
	}
	return out, n, rows.Err()
}
