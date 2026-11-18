package api

import (
	"context"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/chipi/flotilla/internal/spec"
	"github.com/chipi/flotilla/internal/store"
	"github.com/chipi/flotilla/internal/worker"
)

// blockRunner is a worker whose run lasts until it is cancelled (answering "cancelled" with spend, as the
// worker does) or finish is closed (answering ok).
type blockRunner struct {
	mu      sync.Mutex
	started chan string
	finish  chan struct{}
	stopped chan struct{}
	cancels []string
}

func newBlockRunner() *blockRunner {
	return &blockRunner{started: make(chan string, 4), finish: make(chan struct{}), stopped: make(chan struct{})}
}

func (b *blockRunner) Run(ctx context.Context, r worker.Request) (*worker.Result, error) {
	b.started <- r.RunID
	z := 0
	select {
	case <-b.stopped:
		return &worker.Result{Status: "cancelled", ExitCode: &z, DurationS: 2, SpendUSD: 0.3, OutputTail: "terminated"}, nil
	case <-b.finish:
		return &worker.Result{Status: "ok", ExitCode: &z}, nil
	}
}

func (b *blockRunner) Cancel(ctx context.Context, runID string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.cancels = append(b.cancels, runID)
	if len(b.cancels) == 1 {
		close(b.stopped)
	}
	return nil
}

func (b *blockRunner) cancelled() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.cancels...)
}

// inFlight serves the API over a worker that blocks, and starts the routine's run through the API.
func inFlight(t *testing.T) (*httptest.Server, *store.Store, *blockRunner, string) {
	t.Helper()
	srv := newServer(t, func(*spec.Routine) {})
	br := newBlockRunner()
	srv.Sched.Workers["triage"] = br
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	if c, _ := call(t, ts, "POST", "/v1/routines/triage/cycle/run", agentTok, ""); c != 202 {
		t.Fatalf("run: %d", c)
	}
	var runID string
	select {
	case runID = <-br.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the run never reached the worker")
	}
	return ts, srv.Store, br, runID
}

func waitRun(t *testing.T, st *store.Store, runID string) store.Run {
	t.Helper()
	for i := 0; i < 500; i++ {
		runs, _ := st.Runs(context.Background(), "triage/cycle", 5)
		for _, r := range runs {
			if r.ID == runID && r.Status != store.StatusRunning {
				return r
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("run %s never ended", runID)
	return store.Run{}
}

// STOP is taken over spend: it ends the fleet's run in flight, not only the starts after it.
func TestStopCancelsTheFleetsRunInFlight(t *testing.T) {
	ts, st, br, runID := inFlight(t)
	if c, _ := call(t, ts, "POST", "/v1/fleets/triage/stop", agentTok, ""); c != 200 {
		t.Fatalf("stop: %d", c)
	}
	r := waitRun(t, st, runID)
	if r.Status != store.StatusCancelled || r.Detail != "cancelled: claude-example, fleet triage STOPped" || r.SpendUSD != 0.3 {
		t.Fatalf("the run: status %q detail %q spend %v", r.Status, r.Detail, r.SpendUSD)
	}
	if got := br.cancelled(); len(got) != 1 || got[0] != runID {
		t.Fatalf("the worker was asked to cancel %v, want [%s]", got, runID)
	}
	spent, _ := st.Spend(context.Background(), time.Now().In(time.Local).Format("2006-01-02"), "triage/cycle")
	if spent != 0.3 {
		t.Fatalf("a cancelled run's spend is the day's spend: %v", spent)
	}
	if last, _ := st.LastSuccess(context.Background(), "triage/cycle"); !last.IsZero() {
		t.Fatalf("a cancelled run is not a success: %v", last)
	}
	ev, _ := st.Events(context.Background(), 10)
	found := false
	for _, e := range ev {
		if e.Kind == "run.cancel" && e.Actor == "claude-example" && e.Subject == "triage/cycle" && e.Via == "api" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the event log has no run.cancel by the agent: %+v", ev)
	}
}

// A pause lets the run in flight finish; only STOP and an explicit cancel end it.
func TestPauseDoesNotCancel(t *testing.T) {
	ts, st, br, runID := inFlight(t)
	if c, _ := call(t, ts, "POST", "/v1/routines/triage/cycle/pause", agentTok, ""); c != 200 {
		t.Fatalf("pause: %d", c)
	}
	time.Sleep(300 * time.Millisecond)
	if got := br.cancelled(); len(got) != 0 {
		t.Fatalf("a pause cancelled %v", got)
	}
	close(br.finish)
	if r := waitRun(t, st, runID); r.Status != store.StatusOK {
		t.Fatalf("the paused routine's run in flight ended %q, want ok", r.Status)
	}
}

// Starting a fleet again cancels nothing, and a STOP of another fleet leaves this run alone.
func TestStopOfAnotherFleetLeavesTheRun(t *testing.T) {
	srv := newServer(t, func(j *spec.Routine) {})
	other := *srv.Sched.Routines()[0]
	other.Fleet, other.Worker = "ops", "triage"
	srv.Sched.SetRoutines(append(srv.Sched.Routines(), &other))
	br := newBlockRunner()
	srv.Sched.Workers["triage"] = br
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	if c, _ := call(t, ts, "POST", "/v1/routines/triage/cycle/run", agentTok, ""); c != 202 {
		t.Fatalf("run: %d", c)
	}
	runID := <-br.started
	if c, _ := call(t, ts, "POST", "/v1/fleets/ops/stop", agentTok, ""); c != 200 {
		t.Fatalf("stop: %d", c)
	}
	if c, _ := call(t, ts, "POST", "/v1/fleets/ops/start", opTok, `{"reason":"t"}`); c != 200 {
		t.Fatalf("start: %d", c)
	}
	time.Sleep(300 * time.Millisecond)
	if got := br.cancelled(); len(got) != 0 {
		t.Fatalf("a STOP of the ops fleet cancelled triage's run: %v", got)
	}
	close(br.finish)
	if r := waitRun(t, srv.Store, runID); r.Status != store.StatusOK {
		t.Fatalf("ended %q", r.Status)
	}
}

// `flotilla cancel`: an operator's, with a reason; who and why are the run's detail.
func TestCancelOneRun(t *testing.T) {
	ts, st, br, runID := inFlight(t)
	path := "/v1/runs/" + runID + "/cancel"
	if c, _ := call(t, ts, "POST", path, agentTok, `{"reason":"x"}`); c != 403 {
		t.Fatalf("an agent cancelling: %d, want 403", c)
	}
	if c, _ := call(t, ts, "POST", path, opTok, ""); c != 400 {
		t.Fatalf("no reason: %d, want 400", c)
	}
	if c, _ := call(t, ts, "POST", "/v1/runs/nope/cancel", opTok, `{"reason":"x"}`); c != 404 {
		t.Fatalf("an unknown run: %d, want 404", c)
	}
	if got := br.cancelled(); len(got) != 0 {
		t.Fatalf("a refused cancel reached the worker: %v", got)
	}
	c, out := call(t, ts, "POST", path, opTok, `{"reason":"bad prompt, burning spend"}`)
	if c != 200 || out["cancelled"] != runID || out["by"] != "operator" {
		t.Fatalf("cancel: %d %v", c, out)
	}
	r := waitRun(t, st, runID)
	if r.Status != store.StatusCancelled || r.Detail != "cancelled: operator, bad prompt, burning spend" {
		t.Fatalf("the run: status %q detail %q", r.Status, r.Detail)
	}
	if c, _ := call(t, ts, "POST", path, opTok, `{"reason":"again"}`); c != 404 {
		t.Fatalf("a run that has ended: %d, want 404", c)
	}
}
