package store

import (
	"context"
	"time"

	"github.com/chipi/flotilla/internal/cases"
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

// PromotionStreak reads an action's plans newest first. A plan is clean when a shadow plan was judged
// right, or a proposed or automatic one was approved as it stood and verified. It is bad when its
// diagnosis was judged wrong, its execution failed, or a person rejected it. A plan still open, or a
// shadow plan nobody judged, neither counts nor breaks the run.
func (s *Store) PromotionStreak(ctx context.Context, action string) (Streak, error) {
	var st Streak
	rows, err := s.db.QueryContext(ctx, `SELECT p.outcome, p.decided_at, c.resolution,
		EXISTS (SELECT 1 FROM case_feedback f WHERE f.case_id = p.case_id AND f.verdict = ?),
		EXISTS (SELECT 1 FROM case_feedback f WHERE f.case_id = p.case_id AND f.verdict = ?),
		EXISTS (SELECT 1 FROM case_events e WHERE e.case_id = p.case_id AND e.kind = 'case.escalated' AND e.from_status = ?),
		EXISTS (SELECT 1 FROM case_events e WHERE e.case_id = p.case_id AND e.kind = 'case.escalated' AND e.from_status = ?)
		FROM plans p JOIN cases c ON c.id = p.case_id WHERE p.action = ? ORDER BY p.id DESC LIMIT 500`,
		VerdictWrong, VerdictRight, cases.Executing, cases.AwaitingApproval, action)
	if err != nil {
		return st, err
	}
	defer rows.Close()
	cutoff := s.now().Add(-PromotionHold)
	for rows.Next() {
		var outcome, at, resolution string
		var wrong, right, failed, rejected bool
		if err := rows.Scan(&outcome, &at, &resolution, &wrong, &right, &failed, &rejected); err != nil {
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
		clean := (outcome == "shadow" && right) || (outcome != "shadow" && resolution == cases.Verified)
		if clean && st.Clean < PromotionNeed {
			st.Clean++
		}
	}
	return st, rows.Err()
}
