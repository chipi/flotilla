package store

import (
	"context"
	"fmt"
	"os"
	"sort"
	"testing"
	"time"

	"github.com/chipi/flotilla/internal/cases"
)

const (
	actTasks     = "plumbing.create_tasks"
	actDecisions = "plumbing.post_decisions"
)

// meeting plans actTasks, has it approved, executed and verified (its check hands the case on, RFC-0013's then), then
// plans actDecisions, which ends as `end` says: "rejected", "failed" or "verified".
func meeting(t *testing.T, s *Store, key, end string) string {
	t.Helper()
	ctx := context.Background()
	id := plannedOn(t, s, key)
	plan := func(action string) {
		t.Helper()
		r := rec(id)
		r.Action, r.Outcome, r.Autonomy = action, "propose", "propose"
		if _, err := s.ApplyPlan(ctx, r, cases.AwaitingApproval, "", "host:backup-01", false); err != nil {
			t.Fatal(err)
		}
	}
	move := func(to, by, res string) {
		t.Helper()
		if err := s.MoveCase(ctx, id, to, by, res, "x", ""); err != nil {
			t.Fatal(to, err)
		}
	}
	plan(actTasks)
	move(cases.Executing, cases.ByOperator, "")
	move(cases.Checking, cases.ByAct, "")
	if ok, err := s.LeaseCheck(ctx, id, "ops/check", time.Now().Add(time.Hour)); err != nil || !ok {
		t.Fatalf("lease %v %v", ok, err)
	}
	if err := s.HandOnVerified(ctx, id, "decisions", "ops/check", "run r: tasks made", `{}`); err != nil {
		t.Fatal(err)
	}
	move(cases.Planned, cases.ByAct, "")
	plan(actDecisions)
	switch end {
	case "rejected":
		move(cases.Escalated, cases.ByOperator, "")
	case "failed":
		move(cases.Executing, cases.ByOperator, "")
		move(cases.Escalated, cases.ByAct, "")
	case "verified":
		move(cases.Executing, cases.ByOperator, "")
		move(cases.Checking, cases.ByAct, "")
		move(cases.Closed, cases.ByCheck, cases.Verified)
	}
	return id
}

func counts(t *testing.T, s *Store, action string) (ActionRecord, Streak) {
	t.Helper()
	r, err := s.ActionRecord(context.Background(), action)
	if err != nil {
		t.Fatal(err)
	}
	st, err := s.PromotionStreak(context.Background(), action)
	if err != nil {
		t.Fatal(err)
	}
	return r, st
}

// Each plan of a case is credited from its own outcome (RFC-0013, 2026-11-23): the second plan's rejection or failure is
// not the first's, and the first's verification is not the second's.
func TestEarnedCountsPerPlan(t *testing.T) {
	for _, c := range []struct {
		end                          string
		t1Ver, t1Fail, d2Ver, d2Fail int
		t1Clean, d2Clean             int
		t1Bad, d2Bad                 string
	}{
		{"rejected", 1, 0, 0, 0, 1, 0, "", "rejected"},
		{"failed", 1, 0, 0, 1, 1, 0, "", "failed"},
		{"verified", 1, 0, 1, 0, 1, 1, "", ""},
	} {
		t.Run(c.end, func(t *testing.T) {
			s := openTest(t)
			meeting(t, s, "#m", c.end)
			r1, s1 := counts(t, s, actTasks)
			r2, s2 := counts(t, s, actDecisions)
			if r1.Verified != c.t1Ver || r1.Failed != c.t1Fail || s1.Clean != c.t1Clean || s1.BadWhat != c.t1Bad {
				t.Fatalf("first plan: %+v %+v", r1, s1)
			}
			if r2.Verified != c.d2Ver || r2.Failed != c.d2Fail || s2.Clean != c.d2Clean || s2.BadWhat != c.d2Bad {
				t.Fatalf("second plan: %+v %+v", r2, s2)
			}
		})
	}
}

// oldCounts is the case-level reading this replaced: a case's resolution and any escalation of the case, for every action
// the case planned. A case with one plan has to read the same through both.
func oldCounts(t *testing.T, s *Store, action string) (verified, failed int, st Streak) {
	t.Helper()
	ctx := context.Background()
	on := `EXISTS (SELECT 1 FROM plans p WHERE p.case_id = c.id AND p.action = ?)`
	if err := s.rdb.QueryRowContext(ctx, `SELECT COUNT(*) FROM cases c WHERE c.resolution = ? AND `+on, cases.Verified, action).Scan(&verified); err != nil {
		t.Fatal(err)
	}
	if err := s.rdb.QueryRowContext(ctx, `SELECT COUNT(*) FROM case_events e JOIN cases c ON c.id = e.case_id
		WHERE e.kind = 'case.escalated' AND e.from_status = ? AND `+on, cases.Executing, action).Scan(&failed); err != nil {
		t.Fatal(err)
	}
	rows, err := s.rdb.QueryContext(ctx, `SELECT p.outcome, p.decided_at, c.resolution,
		EXISTS (SELECT 1 FROM case_feedback f WHERE f.case_id = p.case_id AND f.verdict = ?),
		EXISTS (SELECT 1 FROM case_feedback f WHERE f.case_id = p.case_id AND f.verdict = ?),
		EXISTS (SELECT 1 FROM case_events e WHERE e.case_id = p.case_id AND e.kind = 'case.escalated' AND e.from_status = ?),
		EXISTS (SELECT 1 FROM case_events e WHERE e.case_id = p.case_id AND e.kind = 'case.escalated' AND e.from_status = ?)
		FROM plans p JOIN cases c ON c.id = p.case_id WHERE p.action = ? ORDER BY p.id DESC LIMIT 500`,
		VerdictWrong, VerdictRight, cases.Executing, cases.AwaitingApproval, action)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var outcome, at, resolution string
		var wrong, right, fail, rej bool
		if err := rows.Scan(&outcome, &at, &resolution, &wrong, &right, &fail, &rej); err != nil {
			t.Fatal(err)
		}
		bad := ""
		switch {
		case wrong:
			bad = "wrong"
		case fail:
			bad = "failed"
		case rej:
			bad = "rejected"
		}
		if bad != "" {
			st.BadAt, st.BadWhat = at, bad
			break
		}
		if (outcome == "record" && right) || (outcome != "record" && resolution == cases.Verified) {
			st.Clean = min(st.Clean+1, PromotionNeed)
		}
	}
	return
}

// A case with one plan counts exactly as before, whatever became of it.
func TestEarnedCountsOnePlanUnchanged(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	n := 0
	one := func(outcome string, moves ...[3]string) {
		n++
		id := plannedOn(t, s, fmt.Sprintf("#o%d", n))
		r := rec(id)
		r.Outcome, r.Autonomy = outcome, outcome
		to, res := cases.Executing, ""
		switch outcome {
		case "propose":
			to = cases.AwaitingApproval
		case "record":
			to, res = cases.Closed, cases.PlanRecorded
		}
		if _, err := s.ApplyPlan(ctx, r, to, res, "host:backup-01", false); err != nil {
			t.Fatal(err)
		}
		for _, m := range moves {
			if err := s.MoveCase(ctx, id, m[0], m[1], m[2], "x", ""); err != nil {
				t.Fatal(m, err)
			}
		}
	}
	verify := [][3]string{{cases.Checking, cases.ByAct, ""}, {cases.Closed, cases.ByCheck, cases.Verified}}
	one("auto", verify...)
	one("auto", verify...)
	one("auto", [3]string{cases.Escalated, cases.ByAct, ""})
	one("propose", [3]string{cases.Executing, cases.ByOperator, ""}, [3]string{cases.Checking, cases.ByAct, ""}, [3]string{cases.Closed, cases.ByCheck, cases.Verified})
	one("propose", [3]string{cases.Escalated, cases.ByOperator, ""})
	one("propose", [3]string{cases.Executing, cases.ByOperator, ""}, [3]string{cases.Escalated, cases.ByAct, ""})
	one("record")
	one("auto") // still executing
	one("auto", [3]string{cases.Closed, cases.ByAct, cases.ResolvedItself})
	wantV, wantF, wantS := oldCounts(t, s, "backup.rerun")
	r, st := counts(t, s, "backup.rerun")
	if r.Verified != wantV || r.Failed != wantF || r.Verified != 3 || r.Failed != 2 {
		t.Fatalf("new verified %d failed %d, old %d %d", r.Verified, r.Failed, wantV, wantF)
	}
	if st.Clean != wantS.Clean || st.BadWhat != wantS.BadWhat || st.BadAt != wantS.BadAt {
		t.Fatalf("new streak %+v, old %+v", st, wantS)
	}
}

// Run it on a copy of a real store (.e2e/realdata/flotilla.db, gitignored; skipped without one): per action, the old and
// new counts, as numbers only. Every difference has to be a case with more than one plan.
func TestEarnedCountsRealDataCopy(t *testing.T) {
	const path = "../../.e2e/realdata/flotilla.db"
	if _, err := os.Stat(path); err != nil {
		t.Skip("no store copy")
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	rows, err := s.rdb.Query(`SELECT action FROM plans GROUP BY action`)
	if err != nil {
		t.Fatal(err)
	}
	var acts []string
	for rows.Next() {
		var a string
		_ = rows.Scan(&a)
		acts = append(acts, a)
	}
	rows.Close()
	sort.Strings(acts)
	var multi int
	_ = s.rdb.QueryRow(`SELECT COUNT(*) FROM (SELECT case_id FROM plans GROUP BY case_id HAVING COUNT(*) > 1)`).Scan(&multi)
	t.Logf("cases with more than one plan: %d", multi)
	for _, a := range acts {
		ov, of, os_ := oldCounts(t, s, a)
		nr, ns := counts(t, s, a)
		var m int
		_ = s.rdb.QueryRow(`SELECT COUNT(DISTINCT case_id) FROM plans WHERE action = ? AND case_id IN (SELECT case_id FROM plans GROUP BY case_id HAVING COUNT(*) > 1)`, a).Scan(&m)
		diff := ov != nr.Verified || of != nr.Failed || os_.Clean != ns.Clean || os_.BadWhat != ns.BadWhat
		t.Logf("%s: multi-plan cases %d; verified %d->%d failed %d->%d clean %d->%d bad %q->%q differs=%v", a, m, ov, nr.Verified, of, nr.Failed, os_.Clean, ns.Clean, os_.BadWhat, ns.BadWhat, diff)
		if diff && m == 0 {
			t.Errorf("%s differs with no multi-plan case", a)
		}
	}
}
