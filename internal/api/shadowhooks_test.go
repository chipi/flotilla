package api

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/chipi/flotilla/internal/registry"
	"github.com/chipi/flotilla/internal/sched"
	"github.com/chipi/flotilla/internal/spec"
	"github.com/chipi/flotilla/internal/store"
	"github.com/chipi/flotilla/internal/versions"
	"github.com/chipi/flotilla/internal/worker"
)

// hookWorker is a live worker that keeps snapshots and a shadow worker that holds its run until it is cancelled.
type hookWorker struct {
	mu       sync.Mutex
	shadow   bool
	started  chan string
	stop     chan struct{}
	once     sync.Once
	cancels  []string
	deleted  []string
	snapshot bool
}

func (h *hookWorker) Run(ctx context.Context, r worker.Request) (*worker.Result, error) {
	z := 0
	if h.shadow {
		h.started <- r.RunID
		select {
		case <-h.stop:
			return &worker.Result{Status: "cancelled", ExitCode: &z}, nil
		case <-time.After(20 * time.Second):
		}
	}
	return &worker.Result{Status: "ok", ExitCode: &z}, nil
}
func (h *hookWorker) Health(ctx context.Context) (*worker.Health, error) {
	m := "live"
	if h.shadow {
		m = "shadow"
	}
	return &worker.Health{OK: true, Mode: m}, nil
}
func (h *hookWorker) Cancel(ctx context.Context, runID string) error {
	h.mu.Lock()
	h.cancels = append(h.cancels, runID)
	h.mu.Unlock()
	h.once.Do(func() { close(h.stop) })
	return nil
}
func (h *hookWorker) TakeSnapshot(ctx context.Context, runID string, paths []string) (*worker.Snapshot, error) {
	return &worker.Snapshot{RunID: runID}, nil
}
func (h *hookWorker) DeleteSnapshot(ctx context.Context, runID string) (bool, error) {
	h.mu.Lock()
	h.deleted = append(h.deleted, runID)
	h.mu.Unlock()
	return true, nil
}

type noRecords struct{}

func (noRecords) ShadowStarted(context.Context, store.Run, string, store.Input) error { return nil }
func (noRecords) ShadowDone(context.Context, *spec.Routine, store.Run, string, store.Input, *sched.CaseOutcome) error {
	return nil
}

// hookWorld is an API over a routine folder (v1 live, v2 shadow), with shadows on.
func hookWorld(t *testing.T, extra string) (*Server, *hookWorker, *hookWorker) {
	t.Helper()
	d := t.TempDir()
	put := func(rel, body string) {
		p := filepath.Join(d, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	put("triage/cycle/routine.yaml", "fleet: triage\nroutine: cycle\ndescription: d\nenabled: true\nworker: triage\ntrigger: {every: 10m}\nautonomy: record\nbudget: {day_usd: 2}\nfreshness: 35m\n"+extra)
	put("triage/cycle/v1/spec.yaml", "run: {command: \"echo v1\", timeout: 5m}\n")
	put("triage/cycle/v2/spec.yaml", "run: {command: \"echo v2\", timeout: 5m}\n")
	rs, err := spec.LoadVersions(d)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "h.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	bound, _, err := versions.Reconcile(context.Background(), st, rs)
	if err != nil {
		t.Fatal(err)
	}
	live := &hookWorker{stop: make(chan struct{})}
	shadow := &hookWorker{shadow: true, started: make(chan string, 4), stop: make(chan struct{})}
	s := &sched.Scheduler{Store: st, Workers: map[string]sched.Runner{"triage": live, "triage-shadow": shadow}, Recorder: noRecords{},
		Shadows: sched.ShadowConfig{Enabled: true}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	s.SetRoutines(bound)
	reg, err := registry.Load(filepath.Join("..", "..", "host", "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{Sched: s, Store: st, Workers: map[string]*worker.Client{}, Version: "test", Registry: reg, SpecsDir: d,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Callers: []Caller{{Name: "operator", Role: RoleOperator, TokenSHA256: HashToken(opTok)}}}
	return srv, live, shadow
}

var operator = Caller{Name: "operator", Role: RoleOperator}

func TestRetiringAShadowVersionCancelsItsRunInFlight(t *testing.T) {
	srv, _, shadow := hookWorld(t, "")
	ctx := context.Background()
	if _, err := srv.Sched.RunNow(ctx, "triage/cycle", "manual", "operator"); err != nil {
		t.Fatal(err)
	}
	<-shadow.started
	if err := srv.RetireVersion(ctx, operator, "test", "triage/cycle", 2, "not needed"); err != nil {
		t.Fatal(err)
	}
	srv.Sched.ShadowWait()
	if len(shadow.cancels) != 1 {
		t.Fatalf("the shadow's run was cancelled on the shadow worker: %v", shadow.cancels)
	}
	rows, _ := srv.Store.ShadowRuns(ctx, store.ShadowRunFilter{})
	if len(rows) != 1 || rows[0].Status != store.ShadowCancelled {
		t.Fatalf("%+v", rows)
	}
}

func TestAStopCancelsTheFleetsShadowRunsToo(t *testing.T) {
	srv, _, shadow := hookWorld(t, "")
	ctx := context.Background()
	srv.Sched.RunNow(ctx, "triage/cycle", "manual", "operator")
	<-shadow.started
	if err := srv.SetStopped(ctx, operator, "test", "triage", true, ""); err != nil {
		t.Fatal(err)
	}
	srv.Sched.ShadowWait()
	if len(shadow.cancels) != 1 {
		t.Fatalf("a STOP cancels the shadow runs of the fleet: %v", shadow.cancels)
	}
}

func TestUnpinningDeletesASnapshotNothingWaitsFor(t *testing.T) {
	srv, live, _ := hookWorld(t, "state: [st]\n")
	ctx := context.Background()
	srv.Store.NewInput(ctx, store.Input{Key: "live-1", Routine: "triage/cycle", Kind: store.InputTick, StartedAt: "2026-11-19T10:00:00.000000000Z", SnapshotPaths: []string{"st"}, SnapshotState: store.SnapshotHeld})
	if err := srv.PinInput(ctx, operator, "test", "live-1", true); err != nil {
		t.Fatal(err)
	}
	if len(live.deleted) != 0 {
		t.Fatalf("a pin keeps it: %v", live.deleted)
	}
	if err := srv.PinInput(ctx, operator, "test", "live-1", false); err != nil {
		t.Fatal(err)
	}
	in, _ := srv.Store.Input(ctx, "live-1")
	if len(live.deleted) != 1 || in.SnapshotState != store.SnapshotDeleted {
		t.Fatalf("an unpin releases it: %v %s", live.deleted, in.SnapshotState)
	}
}
