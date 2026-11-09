package sched

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

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

func (m *fakeMetrics) Run(job, status, stage string, d, s float64) { m.runs = append(m.runs, status) }
func (m *fakeMetrics) LastSuccess(job string, at time.Time)        { m.ok++ }

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
	j := &spec.Job{Fleet: "triage", Job: "cycle", Description: "d", Enabled: true, Worker: "triage",
		Run: spec.Run{Command: "python3 x.py", Workdir: "signal-fleet/mvp", SpendFile: "s"}, Stage: spec.StageShadow,
		Budget: spec.Budget{DayUSD: budget}, Every: 10 * time.Minute, Limit: 20 * time.Minute, Fresh: 35 * time.Minute, Hash: "abc"}
	return &Scheduler{Store: st, Jobs: []*spec.Job{j}, Workers: map[string]Runner{"triage": fr}, Metrics: fm}, fr, fm
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
	_ = s.Store.SetControl(context.Background(), "triage/cycle", KeyStage, "propose", "operator", "api")
	s.RunNow(context.Background(), "triage/cycle", "manual", "operator")
	if fr.got[0].Env["FLEETD_STAGE"] != "propose" {
		t.Fatalf("stage: %s", fr.got[0].Env["FLEETD_STAGE"])
	}
}

func TestControlAboveMaxStageNotObeyed(t *testing.T) {
	s, fr, _ := setup(t, 2)
	s.Jobs[0].MaxStage = spec.StageShadow
	_ = s.Store.SetControl(context.Background(), "triage/cycle", KeyStage, "live", "operator", "api")
	s.RunNow(context.Background(), "triage/cycle", "manual", "operator")
	if fr.got[0].Env["FLEETD_STAGE"] != "shadow" {
		t.Fatalf("a shadow-capped job ran at %s", fr.got[0].Env["FLEETD_STAGE"])
	}
}

func TestSkips(t *testing.T) {
	for name, c := range map[string]struct{ subject, key, want string }{
		"fleet stopped": {"triage", KeyStop, "STOPped"},
		"job paused":    {"triage/cycle", KeyPaused, "paused"},
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

func TestOneRunPerJob(t *testing.T) {
	s, fr, _ := setup(t, 2)
	fr.block = make(chan struct{})
	done := make(chan struct{})
	go func() { s.RunNow(context.Background(), "triage/cycle", "schedule", "flotillad"); close(done) }()
	for len(s.Running()) == 0 {
		time.Sleep(time.Millisecond)
	}
	if _, err := s.RunNow(context.Background(), "triage/cycle", "manual", "operator"); !errors.Is(err, ErrRunning) {
		t.Fatalf("a second start of a running job: %v", err)
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
		t.Fatal("unknown job")
	}
}
