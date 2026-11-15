package api

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/chipi/flotilla/internal/cases"
	"github.com/chipi/flotilla/internal/policy"
	"github.com/chipi/flotilla/internal/spec"
	"github.com/chipi/flotilla/internal/store"
)

const repoAction = "repo.process_change"

func repoCase(t *testing.T, s *Server, key, runID string) string {
	t.Helper()
	ctx := context.Background()
	target := "repo:host/registry.json/backup.rerun_observability/pre_shadow" + key
	k := store.Key{Kind: "target", Value: "process_change/" + target}
	id, _, err := s.Store.OpenCase(ctx, store.NewCase{Kind: "process_change", Target: target, Keys: []store.Key{k}, Canonical: k}, "ops/case-review", "")
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Store.MoveCase(ctx, id, cases.HandedOver, cases.ByDetector, "", "ops/case-review", "")
	if err := s.Store.MoveCase(ctx, id, cases.Planned, cases.ByAct, "", "triage/cycle", "run "+runID); err != nil {
		t.Fatal(err)
	}
	_ = s.Store.StartRun(ctx, store.Run{ID: runID, Routine: "triage/cycle", SpecHash: "x", Trigger: "event", Actor: "flotillad",
		Autonomy: "propose", StartedAt: "2026-11-15T09:00:00.000000000Z", Status: store.StatusRunning})
	return id
}

func repoPlan(id, runID, change string) policy.Plan {
	return policy.Plan{CaseID: id, RunID: runID, Action: repoAction,
		Params: map[string]string{"file": "host/registry.json", "rule": "below_cycle", "change": change}}
}

// ADR-0016: a process change is a plan in Approvals, never above propose; a person approves it, nothing
// goes to a host, and recording the commit closes it as verified, counted on the action's record.
func TestProcessChangePlan(t *testing.T) {
	ctx := context.Background()
	s := newServer(t, func(*spec.Routine) {})
	op := Caller{Name: "user:ada", Role: RoleOperator, User: true}
	if err := s.SetActionAutonomy(ctx, op, "test", repoAction, "auto", "try"); err == nil {
		t.Fatal("a repo change was set to auto")
	}
	if err := s.SetActionAutonomy(ctx, op, "test", repoAction, "propose", "the operator, 2026-11-15"); err != nil {
		t.Fatal(err)
	}
	_ = s.Store.SetControl(ctx, "triage/cycle", "autonomy", "auto", "user:ada", "test")

	id := repoCase(t, s, "", "run-pc")
	d, _, err := s.DecidePlan(ctx, repoPlan(id, "run-pc", "- \"backup_age_above(20h)\"\n+ \"backup_age_above(25h)\""))
	if err != nil {
		t.Fatal(err)
	}
	if d.Outcome != policy.Propose || !strings.Contains(strings.Join(d.Reasons, "; "), "a change to the repo: at most propose") {
		t.Fatalf("with the routine at auto, a repo change proposes: %+v", d)
	}
	if c := caseStatus(t, s, id); c.Status != cases.AwaitingApproval {
		t.Fatalf("not in Approvals: %s", c.Status)
	}
	if err := s.RecordApplied(ctx, op, "test", id, "abc1234"); err == nil {
		t.Fatal("recorded as applied before it was approved")
	}
	if _, err := s.ApprovePlan(ctx, op, "test", id, "", ""); err != nil {
		t.Fatal(err)
	}
	if n, err := s.SendExecutions(ctx); err != nil || n != 0 {
		t.Fatalf("a repo change was sent to a host: %d %v", n, err)
	}
	for _, bad := range []string{"", "xyz", "HEAD", "abc"} {
		if err := s.RecordApplied(ctx, op, "test", id, bad); err == nil {
			t.Errorf("commit %q accepted", bad)
		}
	}
	if err := s.RecordApplied(ctx, agentCaller, "test", id, "abc1234"); err == nil {
		t.Fatal("an agent recorded a change as applied")
	}
	if err := s.RecordApplied(ctx, op, "test", id, "1dd224c"); err != nil {
		t.Fatal(err)
	}
	c := caseStatus(t, s, id)
	if c.Status != cases.Closed || c.Resolution != cases.Verified {
		t.Fatalf("after recording the commit: %s %s", c.Status, c.Resolution)
	}
	evs, _ := s.Store.CaseEvents(ctx, id)
	if last := evs[len(evs)-1]; !strings.Contains(last.Detail, "applied in commit 1dd224c") || last.Actor != "user:ada" {
		t.Fatalf("the close: %+v", last)
	}
	if st, _ := s.Store.PromotionStreak(ctx, repoAction); st.Clean != 1 {
		t.Fatalf("an applied change counts as clean: %+v", st)
	}
}

// A change over the param's max is refused; a host action cannot be recorded as applied by hand.
func TestProcessChangeLimits(t *testing.T) {
	ctx := context.Background()
	s := newServer(t, func(*spec.Routine) {})
	_ = s.SetActionAutonomy(ctx, Caller{Name: "user:ada", Role: RoleOperator, User: true}, "test", repoAction, "propose", "x")
	id := repoCase(t, s, "#big", "run-big")
	d, _, _ := s.DecidePlan(ctx, repoPlan(id, "run-big", strings.Repeat("x", 4001)))
	if d.Outcome != policy.Refuse || !strings.Contains(strings.Join(d.Reasons, "; "), "over 4000 characters") {
		t.Fatalf("an oversize change: %+v", d)
	}
	hid := proposedCase(t, s, "h")
	if _, err := s.ApprovePlan(ctx, opCaller, "test", hid, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordApplied(ctx, opCaller, "test", hid, "abc1234"); err == nil || !strings.Contains(err.Error(), "not a repo change") {
		t.Fatalf("a host action recorded as applied by hand: %v", err)
	}
}

// The worker listener (ADR-0016): a worker's token reads cases and proposes notes, nothing more.
func TestWorkerListenerReadsCases(t *testing.T) {
	s := newServer(t, func(*spec.Routine) {})
	id := proposedCase(t, s, "w")
	ts := httptest.NewServer(s.WorkerHandler())
	defer ts.Close()
	if code, out := call(t, ts, "GET", "/v1/cases?all=1", workerTok, ""); code != 200 || len(out["cases"].([]any)) != 1 {
		t.Fatalf("cases: %d %v", code, out)
	}
	if code, out := call(t, ts, "GET", "/v1/cases/"+id, workerTok, ""); code != 200 || out["plans"] == nil || out["events"] == nil {
		t.Fatalf("a case: %d %v", code, out)
	}
	if code, out := call(t, ts, "POST", "/v1/cases/"+id+"/notes", workerTok, `{"text":"(case-review: refused_plan) x"}`); code != 200 {
		t.Fatalf("a note: %d %v", code, out)
	}
	evs, _ := s.Store.CaseEvents(context.Background(), id)
	if last := evs[len(evs)-1]; last.Kind != "note.proposed" || !strings.Contains(last.Detail, "case-review: refused_plan") {
		t.Fatalf("the note on the timeline: %+v", last)
	}
	for _, p := range []string{"/v1/status", "/v1/runs"} {
		if code, _ := call(t, ts, "GET", p, workerTok, ""); code != 404 {
			t.Errorf("%s on the worker listener: %d", p, code)
		}
	}
	if code, _ := call(t, ts, "POST", "/v1/cases/"+id+"/approve", workerTok, `{}`); code != 404 {
		t.Errorf("approve on the worker listener: %d", code)
	}
	if code, _ := call(t, ts, "GET", "/v1/cases", agentTok, ""); code != 403 {
		t.Errorf("an agent token on the worker listener's cases: %d", code)
	}
}
