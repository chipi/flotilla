package dispatch

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chipi/flotilla/internal/api"
	"github.com/chipi/flotilla/internal/cases"
	"github.com/chipi/flotilla/internal/registry"
	"github.com/chipi/flotilla/internal/sched"
	"github.com/chipi/flotilla/internal/spec"
	"github.com/chipi/flotilla/internal/store"
	"github.com/chipi/flotilla/internal/worker"
)

// fakeWorker answers every run with status and plan; with block set, a run waits for it.
type fakeWorker struct {
	mu     sync.Mutex
	got    []worker.Request
	status string
	plan   string
	block  chan struct{}
}

func (f *fakeWorker) Run(ctx context.Context, r worker.Request) (*worker.Result, error) {
	f.mu.Lock()
	f.got = append(f.got, r)
	status, plan, block := f.status, f.plan, f.block
	f.mu.Unlock()
	if block != nil {
		<-block
	}
	z := 0
	res := &worker.Result{Status: status, ExitCode: &z}
	if plan != "" {
		res.Plan = json.RawMessage(plan)
	}
	return res, nil
}

func (f *fakeWorker) runs() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.got) }

type fixture struct {
	d   *Dispatcher
	st  *store.Store
	w   *fakeWorker
	now time.Time
}

func setup(t *testing.T) *fixture {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "d.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	reg, err := registry.Load(filepath.Join("..", "..", "host", "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	j := &spec.Routine{Fleet: "ops", Routine: "rerun", Description: "d", Enabled: true, Worker: "ops", Autonomy: "shadow",
		Trigger: spec.Trigger{Case: &spec.CaseTrigger{Kind: "failed_backup"}}, Run: spec.Run{Command: "true"}, Limit: time.Minute}
	w := &fakeWorker{status: "ok"}
	f := &fixture{st: st, w: w, now: time.Now()}
	s := &sched.Scheduler{Store: st, Routines: []*spec.Routine{j}, Workers: map[string]sched.Runner{"ops": w},
		Now: func() time.Time { return f.now }}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	f.d = &Dispatcher{API: &api.Server{Sched: s, Store: st, Registry: reg, Log: log}, Log: log}
	return f
}

func (f *fixture) handOver(t *testing.T, key string) string {
	t.Helper()
	ctx := context.Background()
	k := store.Key{Kind: "target", Value: "failed_backup/host:mini" + key}
	id, _, err := f.st.OpenCase(ctx, store.NewCase{Kind: "failed_backup", Target: "host:mini", Keys: []store.Key{k}, Canonical: k}, "ops/backup-monitor", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.st.MoveCase(ctx, id, cases.HandedOver, cases.ByDetector, "", "ops/backup-monitor", ""); err != nil {
		t.Fatal(err)
	}
	return id
}

func (f *fixture) pass(t *testing.T) {
	t.Helper()
	if err := f.d.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.d.Wait()
}

func (f *fixture) cs(t *testing.T, id string) store.Case {
	t.Helper()
	c, err := f.st.Case(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func lastEvent(t *testing.T, f *fixture, id string) store.CaseEvent {
	ev, _ := f.st.CaseEvents(context.Background(), id)
	return ev[len(ev)-1]
}

// v1's chain from hand-over to the recorded shadow plan, with the case in the run's environment.
func TestAPlanGoesToPolicy(t *testing.T) {
	f := setup(t)
	f.w.plan = `{"outcome": "plan", "action": "backup.rerun_observability", "params": {"host": "mini"}}`
	id := f.handOver(t, "")
	f.pass(t)
	c := f.cs(t, id)
	if c.Status != cases.Closed || c.Resolution != cases.ShadowPlan || c.Holder != "" {
		t.Fatalf("case %+v", c)
	}
	plans, _ := f.st.Plans(context.Background(), id)
	if len(plans) != 1 || plans[0].Outcome != "shadow" {
		t.Fatalf("plans %+v", plans)
	}
	var seen caseJSON
	if err := json.Unmarshal([]byte(f.w.got[0].Env["FLOTILLA_CASE"]), &seen); err != nil || seen.ID != id || seen.Target != "host:mini" || len(seen.Keys) != 1 {
		t.Fatalf("FLOTILLA_CASE %q", f.w.got[0].Env["FLOTILLA_CASE"])
	}
	if f.w.got[0].PlanFile == "" {
		t.Fatal("no plan file asked for")
	}
}

func TestResolvedAndEscalated(t *testing.T) {
	f := setup(t)
	f.w.plan = `{"outcome": "resolved_itself", "detail": "backup 2h old"}`
	a := f.handOver(t, "#a")
	f.pass(t)
	if c := f.cs(t, a); c.Status != cases.Closed || c.Resolution != cases.ResolvedItself {
		t.Fatalf("resolved: %+v", c)
	}
	f.w.plan = `{"outcome": "escalate", "detail": "the metric is missing"}`
	b := f.handOver(t, "#b")
	f.pass(t)
	if c := f.cs(t, b); c.Status != cases.Escalated || !strings.Contains(lastEvent(t, f, b).Detail, "the metric is missing") {
		t.Fatalf("escalated: %+v", c)
	}
}

// Anything that is not a decision leaves the case handed over, says why, and waits out the backoff.
func TestNoDecisionBacksOff(t *testing.T) {
	for name, c := range map[string]struct{ status, plan, want string }{
		"no plan file":   {"ok", "", "wrote no plan file"},
		"a failed run":   {"error", `{"outcome": "plan"}`, "ended error"},
		"another case":   {"ok", `{"outcome": "resolved_itself", "case_id": "case-other"}`, "not this one"},
		"a word unknown": {"ok", `{"outcome": "fix_it"}`, "is not plan"},
	} {
		t.Run(name, func(t *testing.T) {
			f := setup(t)
			f.w.status, f.w.plan = c.status, c.plan
			id := f.handOver(t, "")
			f.pass(t)
			e := lastEvent(t, f, id)
			if got := f.cs(t, id); got.Status != cases.HandedOver || got.Holder != "" || e.Kind != "run.no_decision" || !strings.Contains(e.Detail, c.want) {
				t.Fatalf("case %s holder %q, last event %+v", got.Status, got.Holder, e)
			}
			f.pass(t)
			if f.w.runs() != 1 {
				t.Fatalf("dispatched again inside the backoff: %d runs", f.w.runs())
			}
			f.now = f.now.Add(DefaultBackoff + time.Second)
			f.pass(t)
			if f.w.runs() != 2 {
				t.Fatalf("not dispatched after the backoff: %d runs", f.w.runs())
			}
		})
	}
}

// A leased case is not dispatched twice while its run is going.
func TestOneRunPerCase(t *testing.T) {
	f := setup(t)
	f.w.block, f.w.plan = make(chan struct{}), `{"outcome": "resolved_itself"}`
	id := f.handOver(t, "")
	if err := f.d.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	for f.w.runs() == 0 {
		time.Sleep(time.Millisecond)
	}
	if c := f.cs(t, id); c.Holder != "ops/rerun" {
		t.Fatalf("holder %q while running", c.Holder)
	}
	_ = f.d.Once(context.Background())
	close(f.w.block)
	f.d.Wait()
	if f.w.runs() != 1 {
		t.Fatalf("%d runs for one case", f.w.runs())
	}
}

// An expired lease frees a handed-over case, never one with an execution open.
func TestExpiredLeases(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	a := f.handOver(t, "#a")
	ok, _ := f.st.LeaseCase(ctx, a, "ops/rerun", f.now.Add(-time.Minute))
	if !ok {
		t.Fatal("lease")
	}
	if ok, _ := f.st.LeaseCase(ctx, a, "ops/other", f.now.Add(time.Hour)); ok {
		t.Fatal("a held case leased twice")
	}
	// b: leased, then planned and executing (an execution open): its lease must outlive its expiry.
	b := f.handOver(t, "#b")
	_, _ = f.st.LeaseCase(ctx, b, "ops/rerun", f.now.Add(-time.Minute))
	_ = f.st.MoveCase(ctx, b, cases.Planned, cases.ByAct, "", "ops/rerun", "")
	if _, err := f.st.ApplyPlan(ctx, store.PlanRecord{CaseID: b, RunID: "r", Action: "backup.rerun_observability",
		Params: map[string]string{"host": "mini"}, Outcome: "auto", Autonomy: "auto"}, cases.Executing, "", "host:mini#b", true); err != nil {
		t.Fatal(err)
	}
	n, _ := f.st.ExpireLeases(ctx, f.now)
	if n != 1 || f.cs(t, a).Holder != "" {
		t.Fatalf("expired lease kept: %d, holder %q", n, f.cs(t, a).Holder)
	}
	if f.cs(t, b).Holder != "ops/rerun" {
		t.Fatal("an executing case lost its holder: the action could run again")
	}
}

func TestNoActingRoutineNoRun(t *testing.T) {
	f := setup(t)
	f.d.API.Sched.Routines[0].Enabled = false
	f.handOver(t, "")
	f.pass(t)
	if f.w.runs() != 0 {
		t.Fatal("a disabled routine ran")
	}
}
