package dispatch

import (
	"bytes"
	"context"
	"encoding/json"
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
	d    *Dispatcher
	st   *store.Store
	w    *fakeWorker
	now  time.Time
	logs *bytes.Buffer
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
	mon := &spec.Routine{Fleet: "ops", Routine: "backup-monitor", Description: "d", Enabled: true, Worker: "ops", Autonomy: "shadow",
		Trigger: spec.Trigger{Every: "10m"}, Every: 10 * time.Minute, Detects: []string{"failed_backup"}, Run: spec.Run{Command: "true"}, Limit: time.Minute}
	w := &fakeWorker{status: "ok"}
	f := &fixture{st: st, w: w, now: time.Now(), logs: &bytes.Buffer{}}
	s := &sched.Scheduler{Store: st, Routines: []*spec.Routine{j, mon}, Workers: map[string]sched.Runner{"ops": w},
		Now: func() time.Time { return f.now }}
	log := slog.New(slog.NewTextHandler(f.logs, nil))
	f.d = &Dispatcher{API: &api.Server{Sched: s, Store: st, Registry: reg, Log: log}, Log: log}
	s.AfterRun = f.d.Detected
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

// monitor runs the backup monitor through the scheduler, as its time loop or "run now" does, with the
// worker answering report.
func (f *fixture) monitor(t *testing.T, status, report string) {
	t.Helper()
	f.w.mu.Lock()
	f.w.status, f.w.plan = status, report
	f.w.mu.Unlock()
	if _, err := f.d.API.Sched.RunNow(context.Background(), "ops/backup-monitor", "schedule", "flotillad"); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) open(t *testing.T) []store.Case {
	t.Helper()
	cs, err := f.st.Cases(context.Background(), store.CaseFilter{Open: true})
	if err != nil {
		t.Fatal(err)
	}
	return cs
}

// A monitor's detection opens a case, keyed by code from kind and target, and hands it over; the same
// detection again is one case.
func TestDetectionOpensAndHandsOver(t *testing.T) {
	f := setup(t)
	rep := `{"outcome": "detections", "detections": [{"kind": "failed_backup", "target": "host:mini", "detail": "age 20.4h"}]}`
	f.monitor(t, "ok", rep)
	cs := f.open(t)
	if len(cs) != 1 || cs[0].Status != cases.HandedOver || cs[0].Project != "ops" || cs[0].CanonicalKey != "target=failed_backup/host:mini" {
		t.Fatalf("cases %+v", cs)
	}
	ev, _ := f.st.CaseEvents(context.Background(), cs[0].ID)
	if ev[0].Actor != "ops/backup-monitor" || !strings.Contains(ev[0].Detail, "age 20.4h") || ev[1].By != cases.ByDetector {
		t.Fatalf("timeline %+v", ev)
	}
	f.monitor(t, "ok", rep)
	if cs := f.open(t); len(cs) != 1 {
		t.Fatalf("a repeated detection opened %d cases", len(cs))
	}
	if ev, _ := f.st.CaseEvents(context.Background(), cs[0].ID); ev[len(ev)-1].Kind != "signal.attached" {
		t.Fatalf("the repeat is not on the case: %+v", ev[len(ev)-1])
	}
}

func TestReportsRefused(t *testing.T) {
	many := `{"outcome": "detections", "detections": [` + strings.TrimSuffix(strings.Repeat(`{"kind": "failed_backup", "target": "host:mini"},`, 51), ",") + `]}`
	for name, c := range map[string]struct{ status, report, log string }{
		"an undeclared kind": {"ok", `{"outcome": "detections", "detections": [{"kind": "disk_full", "target": "host:mini"}]}`, "detection refused"},
		"a malformed target": {"ok", `{"outcome": "detections", "detections": [{"kind": "failed_backup", "target": "mini; rm -rf"}]}`, "detection refused"},
		"too many":           {"ok", many, "more than 50"},
		"another outcome":    {"ok", `{"outcome": "plan"}`, "monitor report refused"},
		"no report":          {"ok", "", "no plan file"},
		"a failed run":       {"error", `{"outcome": "detections", "detections": [{"kind": "failed_backup", "target": "host:mini"}]}`, ""},
	} {
		t.Run(name, func(t *testing.T) {
			f := setup(t)
			f.monitor(t, c.status, c.report)
			if cs := f.open(t); len(cs) != 0 {
				t.Fatalf("opened %+v", cs)
			}
			if c.log != "" && (!strings.Contains(f.logs.String(), c.log) || !strings.Contains(f.logs.String(), "level=ERROR")) {
				t.Fatalf("no ERROR %q in the log:\n%s", c.log, f.logs)
			}
		})
	}
}

// The whole chain without a host: the monitor detects, the dispatcher hands the case to the acting
// routine, its plan goes to policy, and the case closes with the shadow plan recorded.
func TestDetectionToShadowPlan(t *testing.T) {
	f := setup(t)
	f.monitor(t, "ok", `{"outcome": "detections", "detections": [{"kind": "failed_backup", "target": "host:mini", "detail": "age 20.4h"}]}`)
	id := f.open(t)[0].ID
	f.w.mu.Lock()
	f.w.plan = `{"outcome": "plan", "action": "backup.rerun_observability", "params": {"host": "mini"}}`
	f.w.mu.Unlock()
	f.pass(t)
	if c := f.cs(t, id); c.Status != cases.Closed || c.Resolution != cases.ShadowPlan {
		t.Fatalf("case %+v", c)
	}
}

const old = `{"outcome": "detections", "detections": [{"kind": "failed_backup", "target": "host:mini", "detail": "age 20.4h"}]}`
const fine = `{"outcome": "detections", "detections": []}`

func (f *fixture) all(t *testing.T) []store.Case {
	cs, err := f.st.Cases(context.Background(), store.CaseFilter{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	return cs
}

// One case per incident (operator, 2026-11-13): while the backup stays old, the monitor's detections go
// into the incident's case, closed as a shadow plan; once a run reports it clear, the next detection is
// a new case.
func TestOneCasePerIncident(t *testing.T) {
	f := setup(t)
	f.monitor(t, "ok", old)
	first := f.all(t)[0].ID
	f.w.mu.Lock()
	f.w.plan = `{"outcome": "plan", "action": "backup.rerun_observability", "params": {"host": "mini"}}`
	f.w.mu.Unlock()
	f.pass(t)
	for i := 0; i < 3; i++ {
		f.monitor(t, "ok", old)
	}
	if n := len(f.all(t)); n != 1 {
		t.Fatalf("the same incident made %d cases", n)
	}
	ev, _ := f.st.CaseEvents(context.Background(), first)
	if ev[len(ev)-1].Kind != "signal.absorbed" {
		t.Fatalf("the repeat is not on the incident's case: %+v", ev[len(ev)-1])
	}
	f.monitor(t, "error", fine) // a failed run knows nothing: it clears nothing
	f.monitor(t, "ok", old)
	if n := len(f.all(t)); n != 1 {
		t.Fatalf("a failed run cleared the incident: %d cases", n)
	}
	f.monitor(t, "ok", fine)
	ev, _ = f.st.CaseEvents(context.Background(), first)
	if ev[len(ev)-1].Kind != "incident.cleared" {
		t.Fatalf("a run reporting it clear did not clear it: %+v", ev[len(ev)-1])
	}
	f.monitor(t, "ok", old)
	cs := f.all(t)
	if len(cs) != 2 || cs[0].ID == first || cs[0].Status != cases.HandedOver {
		t.Fatalf("after the incident cleared, a new detection: %+v", cs)
	}
}

// Only a shadow close holds the incident: a case the acting routine closed as resolved ended it.
func TestResolvedEndsTheIncident(t *testing.T) {
	f := setup(t)
	f.monitor(t, "ok", old)
	f.w.mu.Lock()
	f.w.plan = `{"outcome": "resolved_itself", "detail": "fine on a fresh look"}`
	f.w.mu.Unlock()
	f.pass(t)
	f.monitor(t, "ok", old)
	if n := len(f.all(t)); n != 2 {
		t.Fatalf("a detection after a resolved case: %d cases, want a new one", n)
	}
}
