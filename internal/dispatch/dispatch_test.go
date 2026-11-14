package dispatch

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
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
	chk := &spec.Routine{Fleet: "ops", Routine: "verify-backup", Description: "d", Enabled: true, Worker: "ops", Autonomy: "shadow",
		Trigger: spec.Trigger{Check: &spec.CheckTrigger{Verify: "backup_newer_than_execution"}}, Run: spec.Run{Command: "true"}, Limit: time.Minute}
	w := &fakeWorker{status: "ok"}
	f := &fixture{st: st, w: w, now: time.Now(), logs: &bytes.Buffer{}}
	s := &sched.Scheduler{Store: st, Routines: []*spec.Routine{j, mon, chk}, Workers: map[string]sched.Runner{"ops": w},
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

// An expired lease frees a handed-over case, never one with an execution in flight; one in checking is
// freed, so its check resumes.
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
	// c: in checking, its check run's lease run out (flotillad restarted mid-check)
	c := f.checking(t, "#c")
	if ok, _ := f.st.LeaseCheck(ctx, c, "ops/verify-backup", f.now.Add(-time.Minute)); !ok {
		t.Fatal("check lease")
	}
	if ok, _ := f.st.LeaseCheck(ctx, a, "ops/verify-backup", f.now.Add(time.Hour)); ok {
		t.Fatal("a check lease on a case that is not in checking")
	}
	n, _ := f.st.ExpireLeases(ctx, f.now)
	if n != 2 || f.cs(t, a).Holder != "" || f.cs(t, c).Holder != "" {
		t.Fatalf("expired leases kept: %d, holders %q %q", n, f.cs(t, a).Holder, f.cs(t, c).Holder)
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

// checking is a case whose backup.rerun_observability ran and came back done: in checking, the target
// locked (registry: check backup_newer_than_execution within 60m).
func (f *fixture) checking(t *testing.T, key string) string {
	t.Helper()
	return f.checkingAction(t, key, "backup.rerun_observability")
}

func (f *fixture) checkingAction(t *testing.T, key, action string) string {
	t.Helper()
	ctx := context.Background()
	id := f.handOver(t, key)
	if err := f.st.MoveCase(ctx, id, cases.Planned, cases.ByAct, "", "ops/rerun", "run r1"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.ApplyPlan(ctx, store.PlanRecord{CaseID: id, RunID: "r1", Action: action,
		Params: map[string]string{"host": "mini"}, PlanHash: "h-" + key, Outcome: "auto", Autonomy: "auto"},
		cases.Executing, "", "host:mini", true); err != nil {
		t.Fatal(err)
	}
	if err := f.st.MoveCase(ctx, id, cases.Checking, cases.ByAct, "", "flotillad", "on mini: done"); err != nil {
		t.Fatal(err)
	}
	return id
}

func (f *fixture) result(plan string) {
	f.w.mu.Lock()
	f.w.plan = plan
	f.w.mu.Unlock()
}

func (f *fixture) holder(t *testing.T) string {
	t.Helper()
	h, err := f.st.LockHolder(context.Background(), "host:mini")
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// The check step closes a verified case and releases its target; the run gets the case and what to
// verify, with the window's end.
func TestCheckVerifiedCloses(t *testing.T) {
	f := setup(t)
	id := f.checking(t, "#v")
	f.result(`{"outcome": "verified", "detail": "backup at 10:42, after 10:31"}`)
	f.pass(t)
	c := f.cs(t, id)
	if c.Status != cases.Closed || c.Resolution != cases.Verified || c.Holder != "" {
		t.Fatalf("case %+v", c)
	}
	if e := lastEvent(t, f, id); e.By != cases.ByCheck || e.Actor != "ops/verify-backup" || !strings.Contains(e.Detail, "after 10:31") {
		t.Fatalf("the close: %+v", e)
	}
	if h := f.holder(t); h != "" {
		t.Fatalf("a verified case still holds the target: %q", h)
	}
	if f.w.runs() != 1 {
		t.Fatalf("%d runs, want 1", f.w.runs())
	}
	var kj checkJSON
	if err := json.Unmarshal([]byte(f.w.got[0].Env["FLOTILLA_CHECK"]), &kj); err != nil || kj.Verify != "backup_newer_than_execution" ||
		kj.Action != "backup.rerun_observability" || kj.Params["host"] != "mini" || kj.Since != c.StatusAt && kj.Since == "" {
		t.Fatalf("FLOTILLA_CHECK %q", f.w.got[0].Env["FLOTILLA_CHECK"])
	}
	since, _ := time.Parse(time.RFC3339Nano, kj.Since)
	if dl, err := time.Parse(time.RFC3339, kj.Deadline); err != nil || dl.Sub(since) < 59*time.Minute || dl.Sub(since) > 61*time.Minute {
		t.Fatalf("deadline %q for since %q, want 60m after", kj.Deadline, kj.Since)
	}
	if !strings.Contains(f.w.got[0].Env["FLOTILLA_CASE"], id) {
		t.Fatalf("FLOTILLA_CASE %q", f.w.got[0].Env["FLOTILLA_CASE"])
	}
}

// "Not yet" keeps the case in checking, the target held, and it is looked at again after the backoff.
func TestCheckNotYetLooksAgain(t *testing.T) {
	f := setup(t)
	id := f.checking(t, "#n")
	f.result(`{"outcome": "not_yet", "detail": "newest backup 09:00"}`)
	f.pass(t)
	if c := f.cs(t, id); c.Status != cases.Checking || c.Holder != "" {
		t.Fatalf("after not yet: %+v", c)
	}
	if e := lastEvent(t, f, id); e.Kind != notYetEvent || !strings.Contains(e.Detail, "09:00") {
		t.Fatalf("event %+v", e)
	}
	if h := f.holder(t); h != id {
		t.Fatalf("target held by %q, want %s", h, id)
	}
	f.pass(t)
	if f.w.runs() != 1 {
		t.Fatalf("looked again inside the backoff: %d runs", f.w.runs())
	}
	f.now = f.now.Add(DefaultBackoff + time.Minute)
	f.result(`{"outcome": "verified", "detail": "backup at 10:42"}`)
	f.pass(t)
	if c := f.cs(t, id); c.Status != cases.Closed || c.Resolution != cases.Verified || f.w.runs() != 2 {
		t.Fatalf("after the backoff: %+v, %d runs", c, f.w.runs())
	}
}

// Past the window the case is not verified: the timer escalates it, the target stays held, and no check
// runs.
func TestCheckWindowEscalates(t *testing.T) {
	f := setup(t)
	id := f.checking(t, "#w")
	f.now = f.now.Add(61 * time.Minute)
	f.result(`{"outcome": "verified"}`)
	f.pass(t)
	c := f.cs(t, id)
	if c.Status != cases.Escalated || f.w.runs() != 0 {
		t.Fatalf("past the window: %+v, %d runs", c, f.w.runs())
	}
	if e := lastEvent(t, f, id); e.By != cases.ByTimer || !strings.Contains(e.Detail, "not verified: backup_newer_than_execution did not hold within 1h0m0s") {
		t.Fatalf("event %+v", e)
	}
	if h := f.holder(t); h != id {
		t.Fatalf("a not verified case's target: held by %q, want %s", h, id)
	}
}

// Without a check routine nothing runs; the window still ends it. Anything but verified or not_yet is no
// decision: the case stays in checking.
func TestCheckWithoutRoutineOrDecision(t *testing.T) {
	f := setup(t)
	f.d.API.Sched.Routines[2].Enabled = false
	id := f.checking(t, "#x")
	f.pass(t)
	if c := f.cs(t, id); c.Status != cases.Checking || f.w.runs() != 0 {
		t.Fatalf("no check routine: %+v, %d runs", c, f.w.runs())
	}
	f.now = f.now.Add(61 * time.Minute)
	f.pass(t)
	if c := f.cs(t, id); c.Status != cases.Escalated {
		t.Fatalf("no check routine, past the window: %+v", c)
	}

	g := setup(t)
	b := g.checking(t, "#y")
	g.result(`{"outcome": "resolved_itself"}`)
	g.pass(t)
	if c := g.cs(t, b); c.Status != cases.Checking {
		t.Fatalf("a check outcome that is not one: %+v", c)
	}
	if e := lastEvent(t, g, b); e.Kind != "run.no_decision" || !strings.Contains(e.Detail, `"resolved_itself" is not verified or not_yet`) {
		t.Fatalf("event %+v", e)
	}

	// a check run's "verified" for another case verifies nothing
	h := setup(t)
	x := h.checking(t, "#z")
	h.result(`{"outcome": "verified", "case_id": "case-20261114-someoneelse"}`)
	h.pass(t)
	if c := h.cs(t, x); c.Status != cases.Checking {
		t.Fatalf("verified by a result for another case: %+v", c)
	}
	if e := lastEvent(t, h, x); e.Kind != "run.no_decision" || !strings.Contains(e.Detail, "not this one") {
		t.Fatalf("event %+v", e)
	}
}

// An action the registry no longer declares has no check: nothing can verify the case, so it is
// escalated at once rather than left to wait.
func TestCheckActionGoneEscalates(t *testing.T) {
	f := setup(t)
	id := f.checkingAction(t, "#g", "gone.action")
	f.pass(t)
	if c := f.cs(t, id); c.Status != cases.Escalated || f.w.runs() != 0 {
		t.Fatalf("an action without a check: %+v, %d runs", c, f.w.runs())
	}
	if e := lastEvent(t, f, id); e.By != cases.ByTimer || !strings.Contains(e.Detail, `the registry declares no check for "gone.action"`) {
		t.Fatalf("event %+v", e)
	}
}

// executing is a case whose backup.rerun_observability policy let run: executing, the target locked,
// nothing sent yet. It returns the case and its plan's id.
func (f *fixture) executing(t *testing.T, key string) (string, int64) {
	t.Helper()
	ctx := context.Background()
	id := f.handOver(t, key)
	if err := f.st.MoveCase(ctx, id, cases.Planned, cases.ByAct, "", "ops/rerun", "run r1"); err != nil {
		t.Fatal(err)
	}
	pid, err := f.st.ApplyPlan(ctx, store.PlanRecord{CaseID: id, RunID: "r1", Action: "backup.rerun_observability",
		Params: map[string]string{"host": "mini"}, PlanHash: "h-" + key, Outcome: "auto", Autonomy: "auto"},
		cases.Executing, "", "host:mini"+key, true)
	if err != nil {
		t.Fatal(err)
	}
	return id, pid
}

// The deadline is the registry's timeout for the action, not the default: 5m there, so 20m in all.
func TestExecutingDeadlineFromTheRegistry(t *testing.T) {
	f := setup(t)
	raw, err := os.ReadFile(filepath.Join("..", "..", "host", "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	five := strings.Replace(string(raw), `"timeout": "30m"`, `"timeout": "5m"`, 1)
	if five == string(raw) {
		t.Fatal("the registry's timeout line moved: update this test")
	}
	p := filepath.Join(t.TempDir(), "registry.json")
	if err := os.WriteFile(p, []byte(five), 0o644); err != nil {
		t.Fatal(err)
	}
	if f.d.API.Registry, err = registry.Load(p); err != nil {
		t.Fatal(err)
	}
	id, _ := f.executing(t, "#t")
	f.now = f.now.Add(19 * time.Minute)
	f.pass(t)
	if c := f.cs(t, id); c.Status != cases.Executing {
		t.Fatalf("inside a 5m action's deadline: %+v", c)
	}
	f.now = f.now.Add(2 * time.Minute)
	f.pass(t)
	if c := f.cs(t, id); c.Status != cases.Escalated || !strings.Contains(lastEvent(t, f, id).Detail, "within 20m0s") {
		t.Fatalf("past a 5m action's deadline: %+v %+v", c, lastEvent(t, f, id))
	}
}

// The executing deadline (RFC-0004 §3.5): past the action's timeout (30m) and the grace (15m) with no
// execution in flight, a case is unknown: escalated by the timer, target held. Before it, or with its
// execution still in flight, it is left alone.
func TestExecutingDeadline(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	a, _ := f.executing(t, "#a")
	b, pb := f.executing(t, "#b")
	if err := f.st.StartHostExecution(ctx, store.HostExecution{ID: api.ExecutionID(b, pb), CaseID: b, PlanID: pb, Host: "mini",
		Action: "backup.rerun_observability", Approved: "auto", Envelope: []byte("{}")}); err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(44 * time.Minute)
	f.pass(t)
	if c := f.cs(t, a); c.Status != cases.Executing {
		t.Fatalf("inside the deadline: %+v", c)
	}
	f.now = f.now.Add(2 * time.Minute)
	f.pass(t)
	c := f.cs(t, a)
	if c.Status != cases.Escalated {
		t.Fatalf("past the deadline: %+v", c)
	}
	if e := lastEvent(t, f, a); e.By != cases.ByTimer || !strings.Contains(e.Detail, "unknown: no result for backup.rerun_observability within 45m0s") {
		t.Fatalf("event %+v", e)
	}
	if h, _ := f.st.LockHolder(ctx, "host:mini#a"); h != a {
		t.Fatalf("an unknown case's target: held by %q, want %s", h, a)
	}
	if c := f.cs(t, b); c.Status != cases.Executing {
		t.Fatalf("an execution in flight was escalated by the timer: %+v", c)
	}
	if f.w.runs() != 0 {
		t.Fatalf("%d runs: the deadline runs nothing", f.w.runs())
	}
}

// A reassigned case goes round again: its acting routine plans, policy proposes, as the first time.
func TestReassignedCasePlansAgain(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	op := api.Caller{Name: "operator", Role: api.RoleOperator}
	f.d.API.Sched.Routines[0].Autonomy = "propose"
	if err := f.d.API.SetActionAutonomy(ctx, op, "test", "backup.rerun_observability", "propose", "test"); err != nil {
		t.Fatal(err)
	}
	f.result(`{"outcome": "plan", "action": "backup.rerun_observability", "params": {"host": "mini"}, "detail": "age 30h"}`)
	id := f.handOver(t, "#r")
	f.pass(t)
	if c := f.cs(t, id); c.Status != cases.AwaitingApproval {
		t.Fatalf("the first plan: %+v; logs: %s", c, f.logs)
	}
	if _, err := f.d.API.ApproveCase(ctx, op, "test", id); err != nil {
		t.Fatal(err)
	}
	if err := f.st.MoveCase(ctx, id, cases.Escalated, cases.ByAct, "", "flotillad", "on mini: refused"); err != nil {
		t.Fatal(err)
	}
	if err := f.d.API.ReassignCase(ctx, op, "test", id, "try again"); err != nil {
		t.Fatal(err)
	}
	f.pass(t)
	if c := f.cs(t, id); c.Status != cases.AwaitingApproval {
		ev, _ := f.st.CaseEvents(ctx, id)
		t.Fatalf("the second plan: %+v\nevents: %+v\nlogs: %s", c, ev, f.logs)
	}
}
