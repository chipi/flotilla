package sched

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chipi/flotilla/internal/models"
	"github.com/chipi/flotilla/internal/spec"
	"github.com/chipi/flotilla/internal/store"
	"github.com/chipi/flotilla/internal/worker"
)

type fakeRunner struct {
	mu    sync.Mutex
	got   []worker.Request
	res   *worker.Result
	err   error
	block chan struct{}
}

func (f *fakeRunner) Run(ctx context.Context, r worker.Request) (*worker.Result, error) {
	f.mu.Lock()
	f.got = append(f.got, r)
	f.mu.Unlock()
	if f.block != nil {
		<-f.block
	}
	return f.res, f.err
}

type fakeMetrics struct {
	runs []string
	ok   int
}

func (m *fakeMetrics) Run(routine, status, autonomy string, d, s float64) {
	m.runs = append(m.runs, status)
}
func (m *fakeMetrics) LastSuccess(routine string, at time.Time) { m.ok++ }

func setup(t *testing.T, budget float64) (*Scheduler, *fakeRunner, *fakeMetrics) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	zero := 0
	fr := &fakeRunner{res: &worker.Result{Status: "ok", ExitCode: &zero, DurationS: 1.5, SpendUSD: 0.4}}
	fm := &fakeMetrics{}
	j := &spec.Routine{Fleet: "triage", Routine: "cycle", Description: "d", Enabled: true, Worker: "triage",
		Run: spec.Run{Command: "python3 x.py", Workdir: "signal-fleet/mvp", SpendFile: "s"}, Autonomy: spec.AutonomyRecord,
		Budget: spec.Budget{DayUSD: budget}, Every: 10 * time.Minute, Limit: 20 * time.Minute, Fresh: 35 * time.Minute, Hash: "abc"}
	return &Scheduler{Store: st, Routines: []*spec.Routine{j}, Workers: map[string]Runner{"triage": fr}, Metrics: fm}, fr, fm
}

func TestRunPassesTheContract(t *testing.T) {
	s, fr, fm := setup(t, 2)
	r, err := s.RunNow(context.Background(), "triage/cycle", "manual", "operator")
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != store.StatusOK || r.SpendUSD != 0.4 {
		t.Fatalf("got %+v", r)
	}
	env := fr.got[0].Env
	if env["FLEETD_STAGE"] != "shadow" || env["FLEETD_CYCLE_ID"] != r.ID || env["FLEETD_BUDGET_LEFT"] != "2.0000" {
		t.Fatalf("fleetd's cycle contract: %v", env)
	}
	if fr.got[0].TimeoutS != 1200 || fr.got[0].Workdir != "signal-fleet/mvp" || fr.got[0].SpendFile != "s" {
		t.Fatalf("request: %+v", fr.got[0])
	}
	runs, _ := s.Store.Runs(context.Background(), "", 5)
	if len(runs) != 1 || runs[0].Actor != "operator" || runs[0].Trigger != "manual" || runs[0].SpecHash != "abc" {
		t.Fatalf("recorded: %+v", runs)
	}
	if fm.ok != 1 || fm.runs[0] != "ok" {
		t.Fatalf("metrics: %+v", fm)
	}
}

func TestStageControlOverridesSpec(t *testing.T) {
	s, fr, _ := setup(t, 2)
	_ = s.Store.SetControl(context.Background(), "triage/cycle", KeyAutonomy, "propose", "operator", "api")
	s.RunNow(context.Background(), "triage/cycle", "manual", "operator")
	if fr.got[0].Env["FLEETD_STAGE"] != "propose" {
		t.Fatalf("autonomy: %s", fr.got[0].Env["FLEETD_STAGE"])
	}
}

// ADR-0014 decision 8: frozen, a run starts at record; the routine's own control is untouched, so
// lifting the freeze restores it; a run already in flight keeps what it started with.
func TestFreeze(t *testing.T) {
	ctx := context.Background()
	s, fr, _ := setup(t, 2)
	_ = s.Store.SetControl(ctx, "triage/cycle", KeyAutonomy, "auto", "operator", "api")
	fr.block = make(chan struct{})
	done := make(chan *store.Run)
	go func() { r, _ := s.RunNow(ctx, "triage/cycle", "manual", "operator"); done <- r }()
	for started := false; !started; time.Sleep(time.Millisecond) { // the worker has it: its autonomy is read
		fr.mu.Lock()
		started = len(fr.got) == 1
		fr.mu.Unlock()
	}
	_ = s.Store.SetControl(ctx, FreezeSubject, KeyFrozen, "1", "claude-example", "api")
	close(fr.block)
	if r := <-done; r.Autonomy != "auto" || fr.got[0].Env["FLEETD_STAGE"] != spec.FleetdStage("auto") {
		t.Fatalf("a run in flight when the freeze came: %s / %s", r.Autonomy, fr.got[0].Env["FLEETD_STAGE"])
	}
	fr.block = nil
	r, _ := s.RunNow(ctx, "triage/cycle", "manual", "operator")
	if r.Autonomy != "record" || fr.got[1].Env["FLEETD_STAGE"] != "shadow" {
		t.Fatalf("frozen, a run started at %s / %s", r.Autonomy, fr.got[1].Env["FLEETD_STAGE"])
	}
	if v, _ := s.Store.Control(ctx, "triage/cycle", KeyAutonomy); v != "auto" {
		t.Fatalf("the freeze overwrote the routine's control: %q", v)
	}
	if set, _ := s.Configured(ctx, s.Routines[0]); set != "auto" {
		t.Fatalf("configured while frozen: %s", set)
	}
	_ = s.Store.SetControl(ctx, FreezeSubject, KeyFrozen, "0", "operator", "api")
	r, _ = s.RunNow(ctx, "triage/cycle", "manual", "operator")
	if r.Autonomy != "auto" {
		t.Fatalf("after the freeze, a run started at %s", r.Autonomy)
	}
}

func TestControlAboveMaxAutonomyNotObeyed(t *testing.T) {
	s, fr, _ := setup(t, 2)
	s.Routines[0].MaxAutonomy = spec.AutonomyRecord
	_ = s.Store.SetControl(context.Background(), "triage/cycle", KeyAutonomy, "auto", "operator", "api")
	s.RunNow(context.Background(), "triage/cycle", "manual", "operator")
	if fr.got[0].Env["FLEETD_STAGE"] != "shadow" {
		t.Fatalf("a record-capped routine ran at %s", fr.got[0].Env["FLEETD_STAGE"])
	}
}

func TestRunGetsTheModelsSection(t *testing.T) {
	s, fr, _ := setup(t, 2)
	s.RunNow(context.Background(), "triage/cycle", "manual", "operator")
	if _, ok := fr.got[0].Env["FLOTILLA_MODELS"]; ok {
		t.Fatal("FLOTILLA_MODELS set without a models section")
	}
	// security T4: a run gets only the aliases its spec names, with their endpoints; none named, none
	s.Models = &models.Config{Endpoints: map[string]models.Endpoint{"gw": {BaseURL: "http://gw/v1"}, "model-host": {BaseURL: "http://model-host/v1"}},
		Aliases: map[string]models.Alias{"flash": {Endpoint: "gw", Model: "f"}, "local": {Endpoint: "model-host", Model: "l"}}}
	s.OTLPTracesURL, s.Environment = "http://victoriatraces:10428/insert/opentelemetry/v1/traces", "operations"
	s.RunNow(context.Background(), "triage/cycle", "manual", "operator")
	e := fr.got[1].Env
	if _, ok := e["FLOTILLA_MODELS"]; ok || e["FLOTILLA_OTLP_TRACES_URL"] != s.OTLPTracesURL || e["FLOTILLA_ENVIRONMENT"] != "operations" {
		t.Fatalf("a routine that names no model got the models section, or the telemetry env is wrong: %v", e)
	}
	s.Routines[0].Models = []string{"flash"}
	s.RunNow(context.Background(), "triage/cycle", "manual", "operator")
	if got := fr.got[2].Env["FLOTILLA_MODELS"]; got != `{"endpoints":{"gw":{"base_url":"http://gw/v1","defaults":{}}},"aliases":{"flash":{"endpoint":"gw","model":"f"}}}` {
		t.Fatalf("the routine's models: %s", got)
	}
}

func TestRunGetsItsSkillRules(t *testing.T) {
	s, fr, _ := setup(t, 2)
	s.RunNow(context.Background(), "triage/cycle", "manual", "operator")
	if _, ok := fr.got[0].Env["FLOTILLA_SKILLS"]; ok {
		t.Fatal("FLOTILLA_SKILLS set for a routine without rules")
	}
	s.Routines[0].Skills.Rule = []spec.SkillRule{{When: map[string]string{"class": "a"}, Load: []string{"hint-a"}}}
	s.RunNow(context.Background(), "triage/cycle", "manual", "operator")
	if got := fr.got[1].Env["FLOTILLA_SKILLS"]; got != `{"rule":[{"when":{"class":"a"},"load":["hint-a"]}]}` {
		t.Fatalf("FLOTILLA_SKILLS: %q", got)
	}
}

func TestRunGetsItsHarness(t *testing.T) {
	s, fr, _ := setup(t, 2)
	s.RunNow(context.Background(), "triage/cycle", "manual", "operator")
	if _, ok := fr.got[0].Env["FLOTILLA_HARNESS"]; ok {
		t.Fatal("FLOTILLA_HARNESS set for a routine without a harness")
	}
	s.Routines[0].Harness = &spec.Harness{Protocol: "json", Tools: []string{"t"}, Gates: []string{"g"},
		Limits: spec.HarnessLimits{MaxToolCalls: 3, MaxSteps: 4, ToolCallsPerTurn: 1}}
	s.RunNow(context.Background(), "triage/cycle", "manual", "operator")
	if got := fr.got[1].Env["FLOTILLA_HARNESS"]; !strings.Contains(got, `"protocol":"json","tools":["t"],"gates":["g"]`) {
		t.Fatalf("FLOTILLA_HARNESS: %q", got)
	}
}

func TestRunGetsItsPromptsAndRecordsThem(t *testing.T) {
	s, fr, _ := setup(t, 2)
	s.RunNow(context.Background(), "triage/cycle", "manual", "operator")
	if _, ok := fr.got[0].Env["FLOTILLA_PROMPTS"]; ok {
		t.Fatal("FLOTILLA_PROMPTS set for a routine without prompts")
	}
	s.Routines[0].Prompts = map[string]string{"system": "playbooks/triage.md"}
	s.Routines[0].PromptSHA = map[string]string{"system": "ab12"}
	r, _ := s.RunNow(context.Background(), "triage/cycle", "manual", "operator")
	want := `{"system":{"path":"playbooks/triage.md","sha256":"ab12"}}`
	if got := fr.got[1].Env["FLOTILLA_PROMPTS"]; got != want {
		t.Fatalf("FLOTILLA_PROMPTS: %q", got)
	}
	ev, err := s.Store.LatestEvent(context.Background(), "run.prompts", r.ID)
	if err != nil || ev == nil || ev.Detail != want {
		t.Fatalf("the run's prompts event: %+v, %v", ev, err)
	}
}

func TestSkips(t *testing.T) {
	for name, c := range map[string]struct{ subject, key, want string }{
		"fleet stopped":  {"triage", KeyStop, "STOPped"},
		"routine paused": {"triage/cycle", KeyPaused, "paused"},
	} {
		t.Run(name, func(t *testing.T) {
			s, fr, fm := setup(t, 2)
			_ = s.Store.SetControl(context.Background(), c.subject, c.key, "1", "agent", "api")
			r, err := s.RunNow(context.Background(), "triage/cycle", "schedule", "flotillad")
			if err != nil || r.Status != store.StatusSkipped || !strings.Contains(r.Detail, c.want) {
				t.Fatalf("got %+v %v", r, err)
			}
			if len(fr.got) != 0 {
				t.Fatal("a skipped run must not reach the worker")
			}
			if fm.runs[0] != "skipped" {
				t.Fatal("a skip is counted")
			}
			_ = s.Store.SetControl(context.Background(), c.subject, c.key, "0", "operator", "api")
			if r, _ := s.RunNow(context.Background(), "triage/cycle", "schedule", "flotillad"); r.Status != store.StatusOK {
				t.Fatalf("after clearing: %s", r.Status)
			}
		})
	}
}

func TestBudgetStopsTheDay(t *testing.T) {
	s, fr, _ := setup(t, 0.5)
	s.RunNow(context.Background(), "triage/cycle", "schedule", "flotillad") // spends 0.4
	s.RunNow(context.Background(), "triage/cycle", "schedule", "flotillad") // 0.8 >= 0.5 afterwards
	r, _ := s.RunNow(context.Background(), "triage/cycle", "schedule", "flotillad")
	if r.Status != store.StatusSkipped || !strings.Contains(r.Detail, "daily budget") {
		t.Fatalf("third run: %+v", r)
	}
	if len(fr.got) != 2 {
		t.Fatalf("worker called %d times", len(fr.got))
	}
}

func TestZeroBudgetMeansNoCap(t *testing.T) {
	s, fr, _ := setup(t, 0)
	for i := 0; i < 3; i++ {
		s.RunNow(context.Background(), "triage/cycle", "schedule", "flotillad")
	}
	if len(fr.got) != 3 {
		t.Fatalf("budget 0 is no cap (the heartbeat spends nothing): %d runs", len(fr.got))
	}
}

func TestWorkerUnreachableIsAnErrorRun(t *testing.T) {
	s, fr, _ := setup(t, 2)
	fr.res, fr.err = nil, errors.New("connection refused")
	r, err := s.RunNow(context.Background(), "triage/cycle", "schedule", "flotillad")
	if err != nil || r.Status != store.StatusError || !strings.Contains(r.Detail, "connection refused") {
		t.Fatalf("got %+v %v", r, err)
	}
}

// A request that never reached the worker (a deploy recreating it, 2026-11-15) is a skipped run.
func TestWorkerNotReachedIsSkipped(t *testing.T) {
	s, fr, _ := setup(t, 2)
	fr.res, fr.err = nil, fmt.Errorf("worker http://worker-ops-read:8081: %w: lookup worker-ops-read: server misbehaving", worker.ErrUnreachable)
	r, err := s.RunNow(context.Background(), "triage/cycle", "schedule", "flotillad")
	if err != nil || r.Status != store.StatusSkipped || !strings.Contains(r.Detail, "server misbehaving") {
		t.Fatalf("got %+v %v", r, err)
	}
}

// A restart starts a routine when it is due, not at once (2026-11-16): from its last run that was not
// skipped, plus its interval.
func TestFirstWait(t *testing.T) {
	s, _, _ := setup(t, 2)
	ctx := context.Background()
	j := s.Routines[0]
	// in the past: the store stamps a skipped run with its own (real) clock, so the skip below is the newest run
	now := time.Date(2020, 1, 1, 10, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return now }
	if d := s.FirstWait(ctx, j); d != 0 {
		t.Fatalf("never ran: %s, want 0", d)
	}
	s.Now = func() time.Time { return now.Add(-2 * time.Minute) }
	if _, err := s.RunNow(ctx, j.ID(), "schedule", "flotillad"); err != nil {
		t.Fatal(err)
	}
	s.Now = func() time.Time { return now }
	if d := s.FirstWait(ctx, j); d != 8*time.Minute {
		t.Fatalf("ran 2m ago, every 10m: %s, want 8m", d)
	}
	// a skipped run since does not count: it did not do the work
	if err := s.Store.SetControl(ctx, j.ID(), KeyPaused, "1", "t", "test"); err != nil {
		t.Fatal(err)
	}
	s.Now = func() time.Time { return now.Add(-time.Minute) }
	if r, _ := s.RunNow(ctx, j.ID(), "schedule", "flotillad"); r.Status != store.StatusSkipped {
		t.Fatalf("paused run: %s", r.Status)
	}
	s.Now = func() time.Time { return now }
	if d := s.FirstWait(ctx, j); d != 8*time.Minute {
		t.Fatalf("after a skip: %s, want 8m (from the run before it)", d)
	}
	s.Now = func() time.Time { return now.Add(20 * time.Minute) }
	if d := s.FirstWait(ctx, j); d != 0 {
		t.Fatalf("overdue: %s, want 0", d)
	}
}

type readyRunner struct {
	fakeRunner
	mu    sync.Mutex
	calls int
	after int
}

func (r *readyRunner) Ready(context.Context) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	return r.calls > r.after
}

// The first run waits for its worker to answer, at most ReadyWait.
func TestAwaitWorker(t *testing.T) {
	s, _, _ := setup(t, 2)
	rr := &readyRunner{after: 1}
	s.Workers["triage"] = rr
	s.ReadyWait = 10 * time.Second
	s.AwaitWorker(context.Background(), s.Routines[0])
	if rr.calls != 2 {
		t.Fatalf("asked %d times, want 2 (no, then yes)", rr.calls)
	}
	never := &readyRunner{after: 1 << 30}
	s.Workers["triage"] = never
	s.ReadyWait = 50 * time.Millisecond
	start := time.Now()
	s.AwaitWorker(context.Background(), s.Routines[0])
	if el := time.Since(start); el > 5*time.Second {
		t.Fatalf("did not give up: %s", el)
	}
}

func TestOneRunPerJob(t *testing.T) {
	s, fr, _ := setup(t, 2)
	fr.block = make(chan struct{})
	done := make(chan struct{})
	go func() { s.RunNow(context.Background(), "triage/cycle", "schedule", "flotillad"); close(done) }()
	for len(s.Running()) == 0 {
		time.Sleep(time.Millisecond)
	}
	if _, err := s.RunNow(context.Background(), "triage/cycle", "manual", "operator"); !errors.Is(err, ErrRunning) {
		t.Fatalf("a second start of a running routine: %v", err)
	}
	close(fr.block)
	<-done
}

func TestFailedRunKeepsOutputTail(t *testing.T) {
	s, fr, fm := setup(t, 2)
	one := 1
	fr.res = &worker.Result{Status: "error", ExitCode: &one, OutputTail: strings.Repeat("x", 2000) + "Traceback: boom"}
	r, _ := s.RunNow(context.Background(), "triage/cycle", "schedule", "flotillad")
	if r.Status != "error" || !strings.HasSuffix(r.Detail, "Traceback: boom") || len(r.Detail) > 800 {
		t.Fatalf("detail %d chars", len(r.Detail))
	}
	if fm.ok != 0 {
		t.Fatal("a failed run is not a success")
	}
}

func TestUnknownJob(t *testing.T) {
	s, _, _ := setup(t, 2)
	if _, err := s.RunNow(context.Background(), "nope/x", "manual", "op"); err == nil {
		t.Fatal("unknown routine")
	}
}

// flotillad's heartbeat reads the clock before Loop has started: Clock must not depend on Loop.
func TestClockBeforeLoop(t *testing.T) {
	s := &Scheduler{}
	if s.Clock().IsZero() || s.Location == nil {
		t.Fatal("Clock on a scheduler nothing has started")
	}
}

// A routine on cases is never run on time (its Every is zero: a ticker would panic).
func TestLoopLeavesCaseRoutinesAlone(t *testing.T) {
	s, fr, _ := setup(t, 2)
	s.Routines[0].Trigger.Case, s.Routines[0].Every = &spec.CaseTrigger{Kind: "failed_backup"}, 0
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	s.Loop(ctx)
	if len(fr.got) != 0 {
		t.Fatalf("a routine on cases ran %d times on time", len(fr.got))
	}
}

// A run on a case gets the case in its environment and a plan file of its own, and returns the plan.
func TestRunWithACase(t *testing.T) {
	ctx := context.Background()
	s, fr, _ := setup(t, 2)
	fr.res.Plan = []byte(`{"outcome":"plan","action":"backup.rerun"}`)
	r, out, err := s.RunWith(ctx, "triage/cycle", "case", "flotillad", map[string]string{"FLOTILLA_CASE": `{"id":"case-1"}`})
	if err != nil || out == nil || string(out.Plan) != `{"outcome":"plan","action":"backup.rerun"}` {
		t.Fatalf("run %+v outcome %+v err %v", r, out, err)
	}
	req := fr.got[0]
	if req.Env["FLOTILLA_CASE"] != `{"id":"case-1"}` || !strings.Contains(req.PlanFile, r.ID) {
		t.Fatalf("the request: env %v plan_file %q", req.Env["FLOTILLA_CASE"], req.PlanFile)
	}
	if _, err := s.RunNow(ctx, "triage/cycle", "manual", "op"); err != nil || fr.got[1].PlanFile != "" {
		t.Fatalf("a run on time asked for a plan file: %q", fr.got[1].PlanFile)
	}
}

// A run's model calls, as its worker returned them, are kept with the run (G1).
func TestRunRecordsItsModelCalls(t *testing.T) {
	s, fr, _ := setup(t, 2)
	in, cost, no, step := int64(120), 0.002, false, int64(2)
	fr.res.Calls = []worker.Call{{At: "2026-11-14T10:00:01Z", Alias: "triage-pro", Model: "m", TokensIn: &in, CostUSD: &cost,
		FinishReason: "length", Step: &step, DecisionKey: "fp-1",
		Role: "triage", Episode: "fp-1", EpisodeSeq: &step, Tools: json.RawMessage(`["bash","read"]`)},
		{At: "not a time", Alias: "triage-flash", Error: "Transient", Tools: json.RawMessage(`[{"name":"get_metric","arg":"up","ok":true}]`)},
		{At: "2026-11-14T10:00:02Z", Alias: "triage-flash", Check: "tools", CheckOK: &no, Detail: "no get_metric call"}}
	fr.res.Loaded = []worker.Loaded{{At: "2026-11-14T10:00:00Z", Kind: "playbook", Trust: "source", Title: "triage playbook", Source: "p @ x", SHA256: "ab"},
		{Kind: "input", Trust: "untrusted", Title: "glitchtip: X", Source: "s"}}
	r, err := s.RunNow(context.Background(), "triage/cycle", "manual", "operator")
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Store.RunCalls(context.Background(), []string{r.ID})
	if err != nil {
		t.Fatal(err)
	}
	cs := got[r.ID]
	if len(cs) != 3 || cs[0].Routine != "triage/cycle" || *cs[0].TokensIn != 120 || cs[0].At != "2026-11-14T10:00:01.000000000Z" {
		t.Fatalf("the calls: %+v", cs)
	}
	if cs[0].FinishReason != "length" || cs[0].Step == nil || *cs[0].Step != 2 || cs[0].DecisionKey != "fp-1" || cs[1].FinishReason != "" || cs[1].Step != nil {
		t.Fatalf("a call's finish reason, step and decision key reach the store: %+v / %+v", cs[0], cs[1])
	}
	if cs[0].Role != "triage" || cs[0].Episode != "fp-1" || cs[0].EpisodeSeq == nil || *cs[0].EpisodeSeq != 2 || cs[0].Tools != `[{"name":"bash"},{"name":"read"}]` || cs[1].Tools != `[{"name":"get_metric","arg":"up","ok":true}]` || cs[1].EpisodeSeq != nil {
		t.Fatalf("a call's role, episode and tools reach the store: %+v / %+v", cs[0], cs[1])
	}
	if cs[1].Error != "Transient" || cs[1].At == "" {
		t.Fatalf("a call with a bad time is kept, at the run's time: %+v", cs[1])
	}
	if cs[0].Check != "" || cs[0].CheckOK != nil || cs[2].Check != "tools" || cs[2].CheckOK == nil || *cs[2].CheckOK || cs[2].Detail != "no get_metric call" {
		t.Fatalf("a contract check's result is kept on its row, an ordinary call has none (G15): %+v / %+v", cs[0], cs[2])
	}
	ld, err := s.Store.RunLoaded(context.Background(), []string{r.ID})
	if err != nil {
		t.Fatal(err)
	}
	if l := ld[r.ID]; len(l) != 2 || l[0].Trust != "source" || l[0].At != "2026-11-14T10:00:00.000000000Z" || l[1].Trust != "untrusted" || l[1].At == "" {
		t.Fatalf("what the run read (G7): %+v", l)
	}
}

type offline struct{ fakeRunner }

func (*offline) Available() bool { return false }

// RFC-0011: a routine whose remote worker is not connected is skipped, with the reason.
func TestRemoteWorkerNotConnectedSkips(t *testing.T) {
	s, _, _ := setup(t, 0)
	s.Workers["triage"] = &offline{}
	why, err := s.SkipReason(context.Background(), s.Routines[0])
	if err != nil || why != "worker triage is not connected" {
		t.Fatalf("skip reason %q %v", why, err)
	}
}

// A run whose calls file outgrew the worker's cap keeps the lines it got, and one step row says how many were not; its
// harness steps keep their kind and role.
func TestRunRecordsDroppedCallsAndSteps(t *testing.T) {
	s, fr, _ := setup(t, 2)
	fr.res.Calls = []worker.Call{{Alias: "a", Role: "coder"}, {Kind: "step", Role: "gates", Detail: "round 1: gates clean"}}
	fr.res.CallsDropped = 41
	r, err := s.RunNow(context.Background(), "triage/cycle", "manual", "operator")
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Store.RunCalls(context.Background(), []string{r.ID})
	if err != nil {
		t.Fatal(err)
	}
	cs := got[r.ID]
	if len(cs) != 3 || cs[0].Kind != "call" || cs[1].Kind != "step" || cs[1].Role != "gates" || cs[1].Detail != "round 1: gates clean" {
		t.Fatalf("the rows: %+v", cs)
	}
	if last := cs[2]; last.Kind != "step" || last.Role != "harness" || !strings.Contains(last.Detail, "41 more model calls") || !strings.Contains(last.Detail, "first 2 lines") {
		t.Fatalf("the dropped-calls row: %+v", last)
	}
	fr.res.CallsDropped = 0
	r2, _ := s.RunNow(context.Background(), "triage/cycle", "manual", "operator")
	got, _ = s.Store.RunCalls(context.Background(), []string{r2.ID})
	if len(got[r2.ID]) != 2 {
		t.Fatalf("a run within the cap has no dropped-calls row: %+v", got[r2.ID])
	}
}
