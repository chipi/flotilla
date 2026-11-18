package api

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chipi/flotilla/internal/cases"
	"github.com/chipi/flotilla/internal/policy"
	"github.com/chipi/flotilla/internal/spec"
	"github.com/chipi/flotilla/internal/store"
	"github.com/chipi/flotilla/internal/versions"
)

const verRoutine = `fleet: triage
routine: cycle
description: one pass
enabled: true
worker: triage
trigger: {every: 10m}
autonomy: record
budget: {day_usd: 2}
freshness: 35m
`

func verWrite(t *testing.T, dir, rel, body string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// versioned is an API server over triage/cycle loaded from a routine folder with versions v1..v3 (extra is
// appended to routine.yaml), reconciled the way flotillad's start does: v1 live, v2 and v3 shadows.
func versioned(t *testing.T, extra string) *Server {
	t.Helper()
	d := t.TempDir()
	verWrite(t, d, "triage/cycle/routine.yaml", verRoutine+extra)
	for _, v := range []string{"v1", "v2", "v3"} {
		verWrite(t, d, "triage/cycle/"+v+"/spec.yaml", "run: {command: \"echo "+v+"\", timeout: 20m}\n")
	}
	rs, err := spec.LoadVersions(d)
	if err != nil {
		t.Fatal(err)
	}
	srv := newServer(t, func(*spec.Routine) {})
	bound, _, err := versions.Reconcile(context.Background(), srv.Store, rs)
	if err != nil {
		t.Fatal(err)
	}
	srv.Sched.SetRoutines(bound)
	return srv
}

func states(t *testing.T, s *Server) string {
	t.Helper()
	v, err := s.RoutineVersions(context.Background(), "triage/cycle")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, x := range v.Versions {
		out = append(out, "v"+string(rune('0'+x.Version))+"="+x.State)
	}
	return strings.Join(out, " ")
}

func TestVersionsAreTheOperatorsOnly(t *testing.T) {
	for _, r := range []string{RoleAgent, RoleViewer, RoleWorker, "retired-operator-token", ""} {
		if Authorize(r, ActVersion) {
			t.Errorf("%q may promote, roll back or retire", r)
		}
	}
	if !Authorize(RoleOperator, ActVersion) || !Authorize(RoleAdmin, ActVersion) {
		t.Fatal("an operator and an admin may")
	}
	srv := versioned(t, "")
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	for _, p := range []string{"/v1/routines/triage/cycle/versions/2/promote", "/v1/routines/triage/cycle/versions/3/retire", "/v1/routines/triage/cycle/rollback"} {
		for _, tok := range []string{agentTok, workerTok, ""} {
			if c, _ := call(t, ts, "POST", p, tok, `{"autonomy":"record","reason":"x"}`); c != 403 && c != 401 {
				t.Errorf("%s with token %q: %d, want 403 or 401", p, tok, c)
			}
		}
	}
	if got := states(t, srv); got != "v1=live v2=shadow v3=shadow" {
		t.Fatalf("a refused call changed the states: %s", got)
	}
	if c, out := call(t, ts, "GET", "/v1/routines/triage/cycle/versions", agentTok, ""); c != 200 || out["live"] != float64(1) || len(out["versions"].([]any)) != 3 ||
		out["acceptance"].([]any)[0] != "pairs" {
		t.Fatalf("an agent may read the versions: %d %v", c, out)
	}
	if c, _ := call(t, ts, "GET", "/v1/routines/triage/nothing/versions", agentTok, ""); c != 404 {
		t.Fatalf("an unknown routine: %d", c)
	}
}

func TestPromoteVersion(t *testing.T) {
	ctx := context.Background()
	for name, c := range map[string]struct {
		extra string
		edit  func(s *Server, n *int, r *PromoteRequest)
		code  int
		msg   string
	}{
		"no autonomy":      {"", func(_ *Server, _ *int, r *PromoteRequest) { r.Autonomy = "" }, 400, "needs the routine's autonomy"},
		"a bad autonomy":   {"", func(_ *Server, _ *int, r *PromoteRequest) { r.Autonomy = "yolo" }, 400, "needs the routine's autonomy"},
		"above the cap":    {"max_autonomy: record\n", func(_ *Server, _ *int, r *PromoteRequest) { r.Autonomy = "propose" }, 400, "capped at record"},
		"a raise, no why":  {"", func(_ *Server, _ *int, r *PromoteRequest) { r.Autonomy, r.Reason = "auto", "" }, 400, "raising the autonomy to auto needs a reason"},
		"no pairs, no why": {"", func(_ *Server, _ *int, r *PromoteRequest) { r.Reason = "" }, 400, "lacks the evidence its acceptance names (pairs)"},
		"the live one":     {"", func(_ *Server, n *int, _ *PromoteRequest) { *n = 1 }, 409, "is live, not a shadow"},
		"no such version":  {"", func(_ *Server, n *int, _ *PromoteRequest) { *n = 9 }, 404, "no version v9"},
		"a disabled routine": {"", func(s *Server, _ *int, _ *PromoteRequest) {
			j := *s.Sched.Routine("triage/cycle")
			j.Enabled = false
			s.Sched.SetRoutines([]*spec.Routine{&j})
		}, 409, "is disabled"},
		"an unknown worker": {"", func(s *Server, _ *int, _ *PromoteRequest) { delete(s.Sched.Workers, "triage") }, 409, "does not know"},
		"a failed gate": {"acceptance: [replay]\n", func(_ *Server, _ *int, r *PromoteRequest) {
			r.Reason, r.Gates = "", []store.Gate{{Name: "replay", Result: "fail"}}
		}, 400, "failed result (gate replay)"},
		"a missing replay": {"acceptance: [replay]\n", func(_ *Server, _ *int, r *PromoteRequest) { r.Reason = "" }, 400, "(replay)"},
	} {
		t.Run(name, func(t *testing.T) {
			s := versioned(t, c.extra)
			n, r := 2, PromoteRequest{Autonomy: "record", Reason: "no pairs yet; the diff was read"}
			c.edit(s, &n, &r)
			err := s.PromoteVersion(ctx, opCaller, "test", "triage/cycle", n, r)
			if p := AsProblem(err); p == nil || p.Code != c.code || !strings.Contains(p.Msg, c.msg) {
				t.Fatalf("want %d with %q, got %v", c.code, c.msg, err)
			}
			if got := states(t, s); got != "v1=live v2=shadow v3=shadow" {
				t.Fatalf("a refused promote changed the states: %s", got)
			}
			if v, _ := s.Store.Control(ctx, "triage/cycle", "autonomy"); v != "" {
				t.Fatalf("a refused promote set the autonomy: %q", v)
			}
		})
	}

	s := versioned(t, "acceptance: [pairs, replay]\n")
	if err := s.PromoteVersion(ctx, agentCaller, "test", "triage/cycle", 2, PromoteRequest{Autonomy: "record", Reason: "x"}); AsProblem(err) == nil || AsProblem(err).Code != 403 {
		t.Fatalf("an agent promoted: %v", err)
	}
	err := s.PromoteVersion(ctx, opCaller, "ui", "triage/cycle", 2, PromoteRequest{Autonomy: "propose", Reason: "the corpus passes; no pairs yet", WithoutPairs: true,
		Gates: []store.Gate{{Name: "replay-corpus", Result: "pass", Ref: "ci/77"}}})
	if err != nil {
		t.Fatal(err)
	}
	if got := states(t, s); got != "v1=previous v2=live v3=shadow" {
		t.Fatalf("states %s", got)
	}
	if j := s.Sched.Routine("triage/cycle"); j.Version != 2 || j.Run.Command != "echo v2" {
		t.Fatalf("the scheduler runs version %d: %+v", j.Version, j.Run)
	}
	if v, _ := s.Store.Control(ctx, "triage/cycle", "autonomy"); v != "propose" {
		t.Fatalf("autonomy %q", v)
	}
	live, _, _ := s.Store.LiveVersion(ctx, "triage/cycle")
	if live.StateBy != "operator" || live.Promotion == nil || strings.Join(live.Promotion.Acceptance, ",") != "pairs,replay" ||
		live.Promotion.Gates[0].Ref != "ci/77" || !live.Promotion.WithoutPairs || live.Promotion.Missing[0] != "pairs" {
		t.Fatalf("what the promote recorded: %+v", live)
	}
	e, _ := s.Store.LatestEvent(ctx, "version.promote", "triage/cycle@v2")
	if e == nil || e.Actor != "operator" || e.Via != "ui" || !strings.Contains(e.Detail, "gate replay-corpus=pass") || !strings.Contains(e.Detail, "missing pairs") {
		t.Fatalf("event %+v", e)
	}
	// status says which version is bound
	st, _, _ := s.Status(ctx)
	if st[0].Version != 2 || st[0].VersionHash != live.Hash {
		t.Fatalf("status: %+v", st[0])
	}
}

func TestRetireVersion(t *testing.T) {
	ctx := context.Background()
	s := versioned(t, "")
	for _, c := range []struct {
		n    int
		why  string
		code int
	}{{1, "x", 409}, {3, "", 400}, {9, "x", 404}} {
		if p := AsProblem(s.RetireVersion(ctx, opCaller, "test", "triage/cycle", c.n, c.why)); p == nil || p.Code != c.code {
			t.Errorf("retire v%d %q: %v, want %d", c.n, c.why, p, c.code)
		}
	}
	if p := AsProblem(s.RetireVersion(ctx, agentCaller, "test", "triage/cycle", 3, "x")); p == nil || p.Code != 403 {
		t.Fatalf("an agent retired: %v", p)
	}
	if err := s.RetireVersion(ctx, opCaller, "test", "triage/cycle", 3, "not needed"); err != nil {
		t.Fatal(err)
	}
	if got := states(t, s); got != "v1=live v2=shadow v3=retired" {
		t.Fatalf("states %s", got)
	}
}

// Rolling back cancels the rolled-back version's run in flight, withdraws its plans awaiting approval (the case
// goes back to the acting routine) and binds the previous version again; a plan from it cannot be approved.
func TestRollbackVersion(t *testing.T) {
	ctx := context.Background()
	s := versioned(t, "")
	br := newBlockRunner()
	s.Sched.Workers["triage"] = br
	ok := PromoteRequest{Autonomy: "propose", Reason: "no pairs yet; the diff was read"}
	if p := AsProblem(mustRollback(s, RollbackRequest{Autonomy: "record", Reason: "x"})); p == nil || p.Code != 409 || !strings.Contains(p.Msg, "no previous") {
		t.Fatalf("rollback with no previous: %v", p)
	}
	if err := s.PromoteVersion(ctx, opCaller, "test", "triage/cycle", 2, ok); err != nil {
		t.Fatal(err)
	}
	// a v2 run in flight and a v2 plan awaiting approval
	done := make(chan error, 1)
	go func() {
		_, _, err := s.Sched.RunWith(ctx, "triage/cycle", "manual", "operator", nil)
		done <- err
	}()
	var runID string
	select {
	case runID = <-br.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the run never reached the worker")
	}
	id := plannedCase(t, s, "rb", "")
	if _, err := s.Store.ApplyPlan(ctx, store.PlanRecord{CaseID: id, RunID: runID, Action: rerun, Params: map[string]string{"host": "backup-01"},
		PlanHash: "hash-rb-0123456789", Outcome: policy.Propose, Autonomy: policy.Propose}, cases.AwaitingApproval, "", "host:backup-01", true); err != nil {
		t.Fatal(err)
	}

	for name, r := range map[string]RollbackRequest{
		"no reason":   {Autonomy: "record"},
		"no autonomy": {Reason: "v2 files too much"},
	} {
		if p := AsProblem(mustRollback(s, r)); p == nil || p.Code != 400 {
			t.Fatalf("%s: %v", name, p)
		}
	}
	if p := AsProblem(rollbackAs(s, agentCaller, RollbackRequest{Autonomy: "record", Reason: "x"})); p == nil || p.Code != 403 {
		t.Fatalf("an agent rolled back: %v", p)
	}
	res, err := s.RollbackVersion(ctx, opCaller, "ui", "triage/cycle", RollbackRequest{Autonomy: "record", Reason: "v2 files too much"})
	if err != nil {
		t.Fatal(err)
	}
	if res.RolledBack != 2 || res.Live != 1 || len(res.Cases) != 1 || res.Cases[0] != id || len(res.Cancelled) != 1 || res.Cancelled[0] != runID {
		t.Fatalf("result %+v (run %s, case %s)", res, runID, id)
	}
	if got := states(t, s); got != "v1=live v2=retired v3=shadow" {
		t.Fatalf("states %s", got)
	}
	if j := s.Sched.Routine("triage/cycle"); j.Version != 1 || j.Run.Command != "echo v1" {
		t.Fatalf("the scheduler runs version %d", j.Version)
	}
	if v, _ := s.Store.Control(ctx, "triage/cycle", "autonomy"); v != "record" {
		t.Fatalf("autonomy after the rollback is the one the operator confirmed: %q", v)
	}
	r := waitRun(t, s.Store, runID)
	if r.Status != store.StatusCancelled || r.Version != 2 || !strings.Contains(r.Detail, "v2 rolled back") {
		t.Fatalf("the run in flight: %q %q version %d", r.Status, r.Detail, r.Version)
	}
	if got := br.cancelled(); len(got) != 1 || got[0] != runID {
		t.Fatalf("the worker was asked to cancel %v", got)
	}
	<-done
	if c := caseStatus(t, s, id); c.Status != cases.HandedOver {
		t.Fatalf("the rolled-back plan's case: %s", c.Status)
	}
	if _, err := s.ApprovePlan(ctx, opCaller, "test", id, "", ""); AsProblem(err) == nil || AsProblem(err).Code != 409 {
		t.Fatalf("approving the withdrawn plan: %v", err)
	}
	// even when the case is awaiting approval again with that plan still its newest, neither approve nor reject
	for _, to := range []struct{ status, by string }{{cases.Planned, cases.ByAct}, {cases.AwaitingApproval, cases.ByPolicy}} {
		if err := s.Store.MoveCase(ctx, id, to.status, to.by, "", "triage/cycle", ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.ApprovePlan(ctx, opCaller, "test", id, "", ""); AsProblem(err) == nil || AsProblem(err).Code != 409 || !strings.Contains(err.Error(), "plan from a rolled-back version") {
		t.Fatalf("approving: %v", err)
	}
	if err := s.RejectCase(ctx, opCaller, "test", id, "no"); AsProblem(err) == nil || AsProblem(err).Code != 409 || !strings.Contains(err.Error(), "plan from a rolled-back version") {
		t.Fatalf("rejecting: %v", err)
	}
	e, _ := s.Store.LatestEvent(ctx, "version.rollback", "triage/cycle@v2")
	if e == nil || e.Actor != "operator" || e.Via != "ui" || !strings.Contains(e.Detail, "v2 files too much") {
		t.Fatalf("event %+v", e)
	}
}

func mustRollback(s *Server, r RollbackRequest) error { return rollbackAs(s, opCaller, r) }

func rollbackAs(s *Server, c Caller, r RollbackRequest) error {
	_, err := s.RollbackVersion(context.Background(), c, "test", "triage/cycle", r)
	return err
}

// A run in flight when a version is promoted finishes as the version it started with.
func TestRunInFlightKeepsItsVersionAtPromote(t *testing.T) {
	ctx := context.Background()
	s := versioned(t, "")
	br := newBlockRunner()
	s.Sched.Workers["triage"] = br
	done := make(chan error, 1)
	go func() {
		_, _, err := s.Sched.RunWith(ctx, "triage/cycle", "manual", "operator", nil)
		done <- err
	}()
	var runID string
	select {
	case runID = <-br.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the run never reached the worker")
	}
	if err := s.PromoteVersion(ctx, opCaller, "test", "triage/cycle", 2, PromoteRequest{Autonomy: "record", Reason: "x"}); err != nil {
		t.Fatal(err)
	}
	close(br.finish)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if r := waitRun(t, s.Store, runID); r.Version != 1 || r.Status != store.StatusOK {
		t.Fatalf("the run: version %d status %s", r.Version, r.Status)
	}
}

// Integration, API to store to scheduler: a promote binds the new version, so the next run records it (and its
// run_calls and the version of what it wrote), and a rollback binds the previous one again.
func TestPromoteBindsTheNextRun(t *testing.T) {
	ctx := context.Background()
	s := versioned(t, "")
	run := func() *store.Run {
		t.Helper()
		r, _, err := s.Sched.RunWith(ctx, "triage/cycle", "manual", "operator", nil)
		if err != nil || r == nil {
			t.Fatalf("run: %v %v", r, err)
		}
		runs, err := s.Store.Runs(ctx, "triage/cycle", 1)
		if err != nil || len(runs) != 1 || runs[0].ID != r.ID {
			t.Fatalf("the run just made is not the newest: %v %v", runs, err)
		}
		return &runs[0]
	}
	if r := run(); r.Version != 1 {
		t.Fatalf("before the promote: version %d", r.Version)
	}
	if err := s.PromoteVersion(ctx, opCaller, "test", "triage/cycle", 2, PromoteRequest{Autonomy: "record", Reason: "x"}); err != nil {
		t.Fatal(err)
	}
	if r := run(); r.Version != 2 {
		t.Fatalf("the next run after the promote records version %d, want 2", r.Version)
	}
	if _, err := s.RollbackVersion(ctx, opCaller, "test", "triage/cycle", RollbackRequest{Autonomy: "record", Reason: "x"}); err != nil {
		t.Fatal(err)
	}
	if r := run(); r.Version != 1 {
		t.Fatalf("the next run after the rollback records version %d, want 1", r.Version)
	}
}

// A run of the rolled-back version that ends ok before the cancel lands can still file a plan after the
// rollback marked the plans that existed: that plan is the rolled-back version's too and cannot be approved or
// rejected, though nothing marked it.
func TestPlanFiledAfterTheRollbackCannotBeApproved(t *testing.T) {
	ctx := context.Background()
	s := versioned(t, "")
	br := newBlockRunner()
	s.Sched.Workers["triage"] = br
	if err := s.PromoteVersion(ctx, opCaller, "test", "triage/cycle", 2, PromoteRequest{Autonomy: "propose", Reason: "no pairs yet; the diff was read"}); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, _, err := s.Sched.RunWith(ctx, "triage/cycle", "manual", "operator", nil)
		done <- err
	}()
	var runID string
	select {
	case runID = <-br.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the run never reached the worker")
	}
	if _, err := s.RollbackVersion(ctx, opCaller, "test", "triage/cycle", RollbackRequest{Autonomy: "record", Reason: "v2 files too much"}); err != nil {
		t.Fatal(err)
	}
	<-done
	// the run's plan arrives after the rollback, so no rolled_back_at was set on it
	id := plannedCase(t, s, "late", "")
	if _, err := s.Store.ApplyPlan(ctx, store.PlanRecord{CaseID: id, RunID: runID, Action: rerun, Params: map[string]string{"host": "backup-01"},
		PlanHash: "hash-late-0123456789", Outcome: policy.Propose, Autonomy: policy.Propose}, cases.AwaitingApproval, "", "host:backup-01", true); err != nil {
		t.Fatal(err)
	}
	plans, _ := s.Store.Plans(ctx, id)
	if len(plans) != 1 || plans[0].RolledBackAt != "" || plans[0].Version != 2 {
		t.Fatalf("the late plan: %+v", plans)
	}
	if _, err := s.ApprovePlan(ctx, opCaller, "test", id, "", ""); AsProblem(err) == nil || AsProblem(err).Code != 409 || !strings.Contains(err.Error(), "plan from a rolled-back version") {
		t.Fatalf("approving a late plan of a rolled-back version: %v", err)
	}
	if err := s.RejectCase(ctx, opCaller, "test", id, "no"); AsProblem(err) == nil || AsProblem(err).Code != 409 || !strings.Contains(err.Error(), "plan from a rolled-back version") {
		t.Fatalf("rejecting it: %v", err)
	}
	if c := caseStatus(t, s, id); c.Status != cases.AwaitingApproval {
		t.Fatalf("a refused approve or reject moved the case: %s", c.Status)
	}
}
