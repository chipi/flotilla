package api

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chipi/flotilla/internal/cases"
	"github.com/chipi/flotilla/internal/policy"
	"github.com/chipi/flotilla/internal/sched"
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
	s, _ := versionedIn(t, extra)
	return s
}

// versionedIn is versioned with the fleets folder it loaded (srv.SpecsDir is set to it).
func versionedIn(t *testing.T, extra string) (*Server, string) {
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
	srv.SpecsDir = d
	return srv, d
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

// Re-recording is the operator's controlled way past the freeze: it records the folder as it is now as the live
// or previous version's hash, in one transaction with an event; a shadow and a retired version have nothing to
// re-record, the same hash is refused, and a reason is required.
func TestRerecordVersion(t *testing.T) {
	ctx := context.Background()
	s, d := versionedIn(t, "")
	was, _, _ := s.Store.LiveVersion(ctx, "triage/cycle")
	rec := func(c Caller, routine string, n int, why string) (RerecordResult, error) {
		return s.RerecordVersion(ctx, c, "test", routine, n, why)
	}
	for name, c := range map[string]struct {
		c       Caller
		routine string
		n       int
		why     string
		code    int
		msg     string
	}{
		"an agent":        {agentCaller, "triage/cycle", 1, "x", 403, ""},
		"a worker":        {Caller{Name: "w", Role: RoleWorker}, "triage/cycle", 1, "x", 403, ""},
		"no reason":       {opCaller, "triage/cycle", 1, " ", 400, "needs a reason"},
		"unknown routine": {opCaller, "triage/nothing", 1, "x", 404, "not in the fleets folder"},
		"unknown version": {opCaller, "triage/cycle", 9, "x", 404, "no version v9"},
		"nothing changed": {opCaller, "triage/cycle", 1, "x", 409, "nothing to re-record"},
		"a shadow":        {opCaller, "triage/cycle", 2, "x", 409, "is a shadow"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := rec(c.c, c.routine, c.n, c.why)
			if p := AsProblem(err); p == nil || p.Code != c.code || !strings.Contains(p.Msg, c.msg) {
				t.Fatalf("want %d with %q, got %v", c.code, c.msg, err)
			}
		})
	}
	if e, _ := s.Store.LatestEvent(ctx, "version.rerecord", "triage/cycle@v1"); e != nil {
		t.Fatalf("a refused re-record wrote an event: %+v", e)
	}

	verWrite(t, d, "triage/cycle/v1/spec.yaml", "run: {command: \"echo v1 edited\", timeout: 20m}\n")
	res, err := rec(opCaller, "triage/cycle", 1, "a deliberate change to v1")
	if err != nil || res.OldHash != was.Hash || res.NewHash == was.Hash || len(res.Changed) != 1 || res.Changed[0] != "spec.yaml (changed)" || res.By != "operator" {
		t.Fatalf("re-record: %v %+v", err, res)
	}
	now, _, _ := s.Store.LiveVersion(ctx, "triage/cycle")
	if now.Hash != res.NewHash || now.State != store.VersionLive || now.LiveAt != was.LiveAt {
		t.Fatalf("the row: %+v", now)
	}
	e, _ := s.Store.LatestEvent(ctx, "version.rerecord", "triage/cycle@v1")
	if e == nil || e.Actor != "operator" || e.Via != "test" || !strings.Contains(e.Detail, res.OldHash+" -> "+res.NewHash) || !strings.Contains(e.Detail, "spec.yaml (changed)") || !strings.Contains(e.Detail, "a deliberate change to v1") {
		t.Fatalf("event %+v", e)
	}
	// the start now passes over the edited folder: v1 stays live, nothing is refused
	rs, _ := spec.LoadVersions(d)
	stored, _ := s.Store.AllVersions(ctx)
	if _, err := versions.Check(rs, stored); err != nil {
		t.Fatalf("after the re-record: %v", err)
	}
	// retired versions are history
	if err := s.RetireVersion(ctx, opCaller, "test", "triage/cycle", 3, "not needed"); err != nil {
		t.Fatal(err)
	}
	if _, err := rec(opCaller, "triage/cycle", 3, "x"); AsProblem(err) == nil || AsProblem(err).Code != 409 || !strings.Contains(err.Error(), "is retired") {
		t.Fatalf("a retired version: %v", err)
	}
	// a folder that does not load is refused
	verWrite(t, d, "triage/cycle/v1/spec.yaml", "run: {command: x, timeout: 1s}\n")
	if _, err := rec(opCaller, "triage/cycle", 1, "x"); AsProblem(err) == nil || AsProblem(err).Code != 409 || !strings.Contains(err.Error(), "does not load") {
		t.Fatalf("a version that does not load: %v", err)
	}
}

// Over HTTP: the route is the operator's and the body is a reason.
func TestRerecordRoute(t *testing.T) {
	srv, d := versionedIn(t, "")
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	verWrite(t, d, "triage/cycle/v1/spec.yaml", "run: {command: \"echo v1 edited\", timeout: 20m}\n")
	p := "/v1/routines/triage/cycle/versions/1/rerecord"
	for _, tok := range []string{agentTok, workerTok, ""} {
		if c, _ := call(t, ts, "POST", p, tok, `{"reason":"x"}`); c != 403 && c != 401 {
			t.Errorf("token %q: %d, want 403 or 401", tok, c)
		}
	}
	if c, _ := call(t, ts, "POST", p, opTok, `{}`); c != 400 {
		t.Fatalf("no reason: %d", c)
	}
	c, out := call(t, ts, "POST", p, opTok, `{"reason":"deliberate"}`)
	if c != 200 || out["routine"] != "triage/cycle" || out["version"] != float64(1) || out["by"] != "operator" || out["old_hash"] == out["new_hash"] ||
		out["changed"].([]any)[0] != "spec.yaml (changed)" {
		t.Fatalf("%d %v", c, out)
	}
}

// A start barrier: every goroutine waits on it, so the calls overlap as much as the scheduler lets them.
func together(n int, f func(i int)) {
	var ready, wg sync.WaitGroup
	start := make(chan struct{})
	ready.Add(n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			ready.Done()
			<-start
			f(i)
		}(i)
	}
	ready.Wait()
	close(start)
	wg.Wait()
}

// versionInvariants: one live version, at most one previous, and the scheduler runs the version the store says is live.
func versionInvariants(t *testing.T, s *Server) string {
	t.Helper()
	rows, err := s.Store.Versions(context.Background(), "triage/cycle")
	if err != nil {
		t.Fatal(err)
	}
	live, prev := 0, 0
	liveN := 0
	for _, r := range rows {
		switch r.State {
		case store.VersionLive:
			live, liveN = live+1, r.Version
		case store.VersionPrevious:
			prev++
		}
	}
	got := states(t, s)
	if live != 1 || prev > 1 {
		t.Fatalf("%d live and %d previous: %s", live, prev, got)
	}
	if bound := s.Sched.Routine("triage/cycle").Version; bound != liveN {
		t.Fatalf("the store says v%d is live and the scheduler runs v%d: %s", liveN, bound, got)
	}
	return got
}

// Overlapping state changes, under -race: the store's own rules decide, one change wins where only one can, and
// what is left is always one live version, at most one previous, with the scheduler bound to the live one. A
// refused change is a 409, never a 500 or a locked database.
func TestVersionStateChangesRace(t *testing.T) {
	ctx := context.Background()
	ok := PromoteRequest{Autonomy: "record", Reason: "no pairs yet; the diff was read"}
	okCode := func(err error) int {
		if err == nil {
			return 200
		}
		if p := AsProblem(err); p != nil {
			return p.Code
		}
		return 500
	}
	t.Run("two operators promote the same shadow", func(t *testing.T) {
		for round := 0; round < 5; round++ {
			s := versioned(t, "")
			codes := make([]int, 2)
			together(2, func(i int) { codes[i] = okCode(s.PromoteVersion(ctx, opCaller, "test", "triage/cycle", 2, ok)) })
			if !(codes[0] == 200 && codes[1] == 409 || codes[0] == 409 && codes[1] == 200) {
				t.Fatalf("one wins and the other is refused: %v", codes)
			}
			if got := versionInvariants(t, s); got != "v1=previous v2=live v3=shadow" {
				t.Fatalf("states %s", got)
			}
		}
	})
	t.Run("v2 and v3 promoted together", func(t *testing.T) {
		for round := 0; round < 5; round++ {
			s := versioned(t, "")
			codes := make([]int, 2)
			together(2, func(i int) { codes[i] = okCode(s.PromoteVersion(ctx, opCaller, "test", "triage/cycle", i+2, ok)) })
			if codes[0] != 200 || codes[1] != 200 {
				t.Fatalf("both are legal in either order: %v", codes)
			}
			got := versionInvariants(t, s)
			if got != "v1=retired v2=live v3=previous" && got != "v1=retired v2=previous v3=live" {
				t.Fatalf("states %s", got)
			}
		}
	})
	t.Run("a promote races a rollback", func(t *testing.T) {
		for round := 0; round < 5; round++ {
			s := versioned(t, "")
			if err := s.PromoteVersion(ctx, opCaller, "test", "triage/cycle", 2, ok); err != nil {
				t.Fatal(err)
			}
			var perr, rerr error
			together(2, func(i int) {
				if i == 0 {
					perr = s.PromoteVersion(ctx, opCaller, "test", "triage/cycle", 3, ok)
				} else {
					rerr = mustRollback(s, RollbackRequest{Autonomy: "record", Reason: "racing"})
				}
			})
			for _, c := range []int{okCode(perr), okCode(rerr)} {
				if c != 200 && c != 409 {
					t.Fatalf("a refused change is a 409: promote %v, rollback %v", perr, rerr)
				}
			}
			versionInvariants(t, s)
		}
	})
	t.Run("a retire races a promote of the same shadow", func(t *testing.T) {
		for round := 0; round < 5; round++ {
			s := versioned(t, "")
			var perr, rerr error
			together(2, func(i int) {
				if i == 0 {
					perr = s.PromoteVersion(ctx, opCaller, "test", "triage/cycle", 2, ok)
				} else {
					rerr = s.RetireVersion(ctx, opCaller, "test", "triage/cycle", 2, "racing")
				}
			})
			if okCode(perr)+okCode(rerr) != 200+409 {
				t.Fatalf("exactly one of promote and retire wins: promote %v, retire %v", perr, rerr)
			}
			versionInvariants(t, s)
		}
	})
}

// The HTTP layer of the version routes with real tokens: the happy paths (status, JSON shape, the states after),
// what a bad request is, and what the mux says for a wrong method.
func TestVersionRoutes(t *testing.T) {
	srv := versioned(t, "")
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	const base = "/v1/routines/triage/cycle"

	t.Run("refusals change nothing", func(t *testing.T) {
		for name, c := range map[string]struct {
			method, path, body string
			want               int
		}{
			"malformed body":          {"POST", base + "/versions/2/promote", `{`, 400},
			"an unknown field":        {"POST", base + "/versions/2/promote", `{"autonomy":"record","reason":"x","bogus":1}`, 400},
			"an unknown rollback key": {"POST", base + "/rollback", `{"autonomy":"record","reason":"x","force":true}`, 400},
			"a malformed rollback":    {"POST", base + "/rollback", `nope`, 400},
			"unknown routine":         {"POST", "/v1/routines/triage/nothing/versions/2/promote", `{"autonomy":"record","reason":"x"}`, 404},
			"unknown version":         {"POST", base + "/versions/9/promote", `{"autonomy":"record","reason":"x"}`, 404},
			"retire an unknown one":   {"POST", base + "/versions/9/retire", `{"reason":"x"}`, 404},
			"version abc":             {"POST", base + "/versions/abc/promote", `{"autonomy":"record","reason":"x"}`, 400},
			"version 0":               {"POST", base + "/versions/0/promote", `{"autonomy":"record","reason":"x"}`, 400},
			"version -1":              {"POST", base + "/versions/-1/retire", `{"reason":"x"}`, 400},
			"GET on promote":          {"GET", base + "/versions/2/promote", ``, 405},
			"GET on rollback":         {"GET", base + "/rollback", ``, 405},
			"DELETE on a version":     {"DELETE", base + "/versions/2/retire", ``, 405},
			"POST on the list":        {"POST", base + "/versions", `{}`, 405},
			"rollback, no previous":   {"POST", base + "/rollback", `{"autonomy":"record","reason":"x"}`, 409},
		} {
			if code, out := call(t, ts, c.method, c.path, opTok, c.body); code != c.want {
				t.Errorf("%s: %d %v, want %d", name, code, out, c.want)
			}
		}
		if got := states(t, srv); got != "v1=live v2=shadow v3=shadow" {
			t.Fatalf("a refused request changed the states: %s", got)
		}
	})

	t.Run("promote, retire and roll back", func(t *testing.T) {
		code, out := call(t, ts, "POST", base+"/versions/2/promote", opTok, `{"autonomy":"propose","reason":"no pairs yet; the diff was read","without_pairs":true}`)
		if code != 200 || out["routine"] != "triage/cycle" || out["version"] != "2" || out["state"] != "live" || out["autonomy"] != "propose" || out["by"] != "operator" {
			t.Fatalf("promote: %d %v", code, out)
		}
		if got := states(t, srv); got != "v1=previous v2=live v3=shadow" {
			t.Fatalf("states %s", got)
		}
		code, out = call(t, ts, "POST", base+"/versions/3/retire", opTok, `{"reason":"not needed"}`)
		if code != 200 || out["version"] != "3" || out["state"] != "retired" || out["by"] != "operator" {
			t.Fatalf("retire: %d %v", code, out)
		}
		code, out = call(t, ts, "POST", base+"/rollback", opTok, `{"autonomy":"record","reason":"v2 files too much"}`)
		if code != 200 || out["rolled_back"] != float64(2) || out["live"] != float64(1) {
			t.Fatalf("rollback: %d %v", code, out)
		}
		if got := states(t, srv); got != "v1=live v2=retired v3=retired" {
			t.Fatalf("states %s", got)
		}
		code, out = call(t, ts, "GET", base+"/versions", agentTok, "")
		if code != 200 || out["live"] != float64(1) {
			t.Fatalf("list: %d %v", code, out)
		}
	})
}

// Authentication at HTTP on every version route: a viewer, a revoked user, the retired callers-file operator token
// and no token at all are refused, and nothing moves.
func TestVersionRoutesRefuseWhoIsNotAnOperator(t *testing.T) {
	srv := versioned(t, "")
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	_, out := call(t, ts, "POST", "/v1/users", opTok, `{"name":"ada","role":"admin"}`)
	admin := out["token"].(string)
	_, out = call(t, ts, "POST", "/v1/users", admin, `{"name":"vic","role":"viewer"}`)
	viewer := out["token"].(string)
	_, out = call(t, ts, "POST", "/v1/users", admin, `{"name":"gone","role":"operator"}`)
	revoked := out["token"].(string)
	if c, _ := call(t, ts, "POST", "/v1/users/gone/revoke", admin, ""); c != 200 {
		t.Fatalf("revoke: %d", c)
	}
	routes := []struct{ method, path, body string }{
		{"POST", "/v1/routines/triage/cycle/versions/2/promote", `{"autonomy":"record","reason":"x"}`},
		{"POST", "/v1/routines/triage/cycle/versions/3/retire", `{"reason":"x"}`},
		{"POST", "/v1/routines/triage/cycle/rollback", `{"autonomy":"record","reason":"x"}`},
		{"POST", "/v1/routines/triage/cycle/versions/1/rerecord", `{"reason":"x"}`},
	}
	for who, tok := range map[string]string{"a viewer": viewer, "a revoked operator": revoked, "the retired callers-file operator token": opTok, "no token": "", "a guess": "flt_" + strings.Repeat("0", 64)} {
		for _, r := range routes {
			if c, _ := call(t, ts, r.method, r.path, tok, r.body); c != 401 && c != 403 {
				t.Errorf("%s on %s: %d, want 401 or 403", who, r.path, c)
			}
		}
	}
	if got := states(t, srv); got != "v1=live v2=shadow v3=shadow" {
		t.Fatalf("a refused request changed the states: %s", got)
	}
	// the admin may
	if c, _ := call(t, ts, "POST", routes[0].path, admin, routes[0].body); c != 200 {
		t.Fatalf("an admin promoting: %d", c)
	}
}

// The previous version was live when it made its plans: after a promote, approving one of them still works (a plan
// of the version that was rolled back is the one that cannot be approved).
func TestPreviousVersionsPlanStaysApprovableAfterAPromote(t *testing.T) {
	ctx := context.Background()
	s := versioned(t, "")
	if err := s.Store.StartRun(ctx, store.Run{ID: "run-v1", Routine: "triage/cycle", Version: 1, SpecHash: "x", Trigger: "case", Actor: "flotillad", Autonomy: "propose",
		StartedAt: "2026-11-18T10:00:00Z", Status: store.StatusOK}); err != nil {
		t.Fatal(err)
	}
	id := plannedCase(t, s, "prev", "")
	if _, err := s.Store.ApplyPlan(ctx, store.PlanRecord{CaseID: id, RunID: "run-v1", Action: rerun, Params: map[string]string{"host": "backup-01"},
		PlanHash: "hash-prev-0123456789", Outcome: policy.Propose, Autonomy: policy.Propose}, cases.AwaitingApproval, "", "host:backup-01", true); err != nil {
		t.Fatal(err)
	}
	if err := s.PromoteVersion(ctx, opCaller, "test", "triage/cycle", 2, PromoteRequest{Autonomy: "propose", Reason: "no pairs yet; the diff was read"}); err != nil {
		t.Fatal(err)
	}
	if got := states(t, s); got != "v1=previous v2=live v3=shadow" {
		t.Fatalf("states %s", got)
	}
	if hash, err := s.ApprovePlan(ctx, opCaller, "test", id, "", ""); err != nil || hash != "hash-prev-0123456789" {
		t.Fatalf("approving the previous version's plan: %q %v", hash, err)
	}
	if c := caseStatus(t, s, id); c.Status != cases.Executing {
		t.Fatalf("the case: %s", c.Status)
	}
}

// A version whose folder is deleted or broken after flotillad loaded the routines (it restarted without it, or the
// routines were reloaded): promote and roll back refuse it by status and change nothing.
func TestVersionActionsOnAFolderThatWentAway(t *testing.T) {
	ctx := context.Background()
	s, d := versionedIn(t, "")
	reload := func() {
		rs, err := spec.LoadVersions(d)
		if err != nil {
			t.Fatal(err)
		}
		s.Sched.SetRoutines(rs)
	}
	verWrite(t, d, "triage/cycle/v3/spec.yaml", "run: {command: x, timeout: 1s}\n") // broken: the timeout is under the minimum
	if err := os.RemoveAll(filepath.Join(d, "triage/cycle/v2")); err != nil {
		t.Fatal(err)
	}
	reload()
	ok := PromoteRequest{Autonomy: "record", Reason: "no pairs yet; the diff was read"}
	if p := AsProblem(s.PromoteVersion(ctx, opCaller, "test", "triage/cycle", 2, ok)); p == nil || p.Code != 404 || !strings.Contains(p.Msg, "no version v2 in the fleets folder") {
		t.Fatalf("a deleted shadow: %v", p)
	}
	if p := AsProblem(s.PromoteVersion(ctx, opCaller, "test", "triage/cycle", 3, ok)); p == nil || p.Code != 409 || !strings.Contains(p.Msg, "does not load") {
		t.Fatalf("a broken shadow: %v", p)
	}
	if got := states(t, s); got != "v1=live v2=shadow v3=shadow" {
		t.Fatalf("states %s", got)
	}

	// roll back with the previous version's folder gone: refused, v2 stays live
	s2, d2 := versionedIn(t, "")
	if err := s2.PromoteVersion(ctx, opCaller, "test", "triage/cycle", 2, ok); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(d2, "triage/cycle/v1")); err != nil {
		t.Fatal(err)
	}
	rs, err := spec.LoadVersions(d2)
	if err != nil {
		t.Fatal(err)
	}
	s2.Sched.SetRoutines(rs)
	if p := AsProblem(mustRollback(s2, RollbackRequest{Autonomy: "record", Reason: "x"})); p == nil || p.Code != 409 || !strings.Contains(p.Msg, "is not in the fleets folder or does not load") {
		t.Fatalf("a missing previous: %v", p)
	}
	if got := states(t, s2); got != "v1=previous v2=live v3=shadow" {
		t.Fatalf("a refused rollback changed the states: %s", got)
	}
}

// A running row the scheduler does not have (a worker's run flotillad is not tracking) is not cancelled by the
// rollback and is not an error: it is reported to the caller by the store only, and the API's Cancelled stays empty.
func TestRollbackWithARunningRowTheSchedulerDoesNotHave(t *testing.T) {
	ctx := context.Background()
	s := versioned(t, "")
	if err := s.PromoteVersion(ctx, opCaller, "test", "triage/cycle", 2, PromoteRequest{Autonomy: "record", Reason: "no pairs yet; the diff was read"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Store.StartRun(ctx, store.Run{ID: "ghost", Routine: "triage/cycle", Version: 2, SpecHash: "x", Trigger: "schedule", Actor: "flotillad", Autonomy: "record",
		StartedAt: "2026-11-18T10:00:00Z", Status: store.StatusRunning}); err != nil {
		t.Fatal(err)
	}
	res, err := s.RollbackVersion(ctx, opCaller, "test", "triage/cycle", RollbackRequest{Autonomy: "record", Reason: "x"})
	if err != nil || res.Live != 1 || len(res.Cancelled) != 0 {
		t.Fatalf("rollback: %v %+v", err, res)
	}
	if got := states(t, s); got != "v1=live v2=retired v3=shadow" {
		t.Fatalf("states %s", got)
	}
}

// A promote is the operator's control and is not held by the installation's other controls: while it is frozen, its
// fleet is STOPped or the routine is paused it still records the version and the autonomy chosen (the effective
// level stays what the freeze makes it), and the event says who.
func TestPromoteWhileHeld(t *testing.T) {
	ctx := context.Background()
	for name, hold := range map[string]func(s *Server) error{
		"frozen": func(s *Server) error { return s.SetFrozen(ctx, opCaller, "test", true, "") },
		"stopped": func(s *Server) error {
			return s.SetStopped(ctx, opCaller, "test", "triage", true, "")
		},
		"paused": func(s *Server) error {
			return s.Store.SetControl(ctx, "triage/cycle", sched.KeyPaused, "1", "operator", "test")
		},
	} {
		t.Run(name, func(t *testing.T) {
			s := versioned(t, "")
			if err := hold(s); err != nil {
				t.Fatal(err)
			}
			if err := s.PromoteVersion(ctx, opCaller, "ui", "triage/cycle", 2, PromoteRequest{Autonomy: "propose", Reason: "no pairs yet; the diff was read"}); err != nil {
				t.Fatalf("promote while %s: %v", name, err)
			}
			if got := states(t, s); got != "v1=previous v2=live v3=shadow" {
				t.Fatalf("states %s", got)
			}
			if v, _ := s.Store.Control(ctx, "triage/cycle", "autonomy"); v != "propose" {
				t.Fatalf("the autonomy chosen is recorded: %q", v)
			}
			if j := s.Sched.Routine("triage/cycle"); j.Version != 2 {
				t.Fatalf("bound to v%d", j.Version)
			}
			e, _ := s.Store.LatestEvent(ctx, "version.promote", "triage/cycle@v2")
			if e == nil || e.Actor != "operator" || !strings.Contains(e.Detail, "autonomy propose") {
				t.Fatalf("event %+v", e)
			}
			if name == "frozen" {
				st, _, _ := s.Status(ctx)
				if st[0].AutonomySet != "propose" || st[0].Autonomy != "record" {
					t.Fatalf("the freeze lowers what runs, not what is set: set %q, runs %q", st[0].AutonomySet, st[0].Autonomy)
				}
			}
		})
	}
}

// The auto path too: a rollback that keeps autonomy auto must not let a run of the rolled-back version that ended
// ok after the rollback have its plan decided and executed. The case is escalated with the withdrawn reason.
func TestPlanFiledAfterTheRollbackIsEscalatedAtAuto(t *testing.T) {
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
	if _, err := s.RollbackVersion(ctx, opCaller, "test", "triage/cycle", RollbackRequest{Autonomy: "auto", Reason: "v2 files too much"}); err != nil {
		t.Fatal(err)
	}
	<-done
	// A routine action (crm.update_field, executor routine) on a non-host target: with routine and action at auto and the
	// installation's routine_actions.max_autonomy at auto nothing else caps it, so without the guard this plan would be
	// decided Auto and the case would execute.
	s.RoutineActionMax = spec.AutonomyAuto
	if err := s.Store.SetControl(ctx, actionSubject("crm.update_field"), "autonomy", policy.Auto, "operator", "test"); err != nil {
		t.Fatal(err)
	}
	k := store.Key{Kind: "target", Value: "crm_fix/crm:contact-1"}
	id, _, err := s.Store.OpenCase(ctx, store.NewCase{Kind: "crm_fix", Target: "crm:contact-1", Keys: []store.Key{k}, Canonical: k}, "ops/crm-monitor", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Store.MoveCase(ctx, id, cases.HandedOver, cases.ByDetector, "", "ops/crm-monitor", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.Store.MoveCase(ctx, id, cases.Planned, cases.ByAct, "", "triage/cycle", "run "+runID); err != nil {
		t.Fatal(err)
	}
	plan := policy.Plan{CaseID: id, RunID: runID, Action: "crm.update_field",
		Params: map[string]string{"contact": "c1", "field": "email", "from": "a@example.org", "to": "b@example.org"}}
	d, _, err := s.DecidePlan(ctx, plan)
	if err != nil || d.Outcome != policy.Refuse || !strings.Contains(strings.Join(d.Reasons, ";"), "plan from a rolled-back version (v2 rolled back)") {
		t.Fatalf("a withdrawn plan at auto: %+v %v", d, err)
	}
	if c := caseStatus(t, s, id); c.Status != cases.Escalated {
		t.Fatalf("the case is %s, want escalated", c.Status)
	}
	if h, _ := s.Store.LockHolder(ctx, "crm:contact-1"); h != "" {
		t.Fatalf("a withdrawn plan took the target's lock: %q", h)
	}
}

// The re-record body is read strictly, like promote and rollback: a misspelt field is a 400, not a silent no-op.
func TestRerecordRouteRefusesUnknownFields(t *testing.T) {
	srv, d := versionedIn(t, "")
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	verWrite(t, d, "triage/cycle/v1/spec.yaml", "run: {command: \"echo v1 edited\", timeout: 20m}\n")
	p := "/v1/routines/triage/cycle/versions/1/rerecord"
	if c, _ := call(t, ts, "POST", p, opTok, `{"reason":"deliberate","resaon":"typo"}`); c != 400 {
		t.Fatalf("unknown field: %d, want 400", c)
	}
	if c, _ := call(t, ts, "POST", p, opTok, `{"reason":"deliberate"}`); c != 200 {
		t.Fatalf("the strict body still works: %d", c)
	}
}
