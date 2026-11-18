package hostagent

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chipi/flotilla/internal/hostlink"
	"github.com/chipi/flotilla/internal/worker"
)

// fakeLocal is the worker beside the link: it answers each run with its run id; block holds runs.
type fakeLocal struct {
	mu        sync.Mutex
	got       []worker.Request
	block     chan struct{}
	stop      chan struct{} // closed by Cancel: the worker stops the run and answers it "cancelled"
	stops     []string
	cancelled int
}

func (f *fakeLocal) Cancel(ctx context.Context, runID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stops = append(f.stops, runID)
	if f.stop != nil && len(f.stops) == 1 {
		close(f.stop)
	}
	return nil
}

func (f *fakeLocal) Run(ctx context.Context, r worker.Request) (*worker.Result, error) {
	f.mu.Lock()
	f.got = append(f.got, r)
	b, stop := f.block, f.stop
	f.mu.Unlock()
	if b != nil {
		select {
		case <-stop:
			return &worker.Result{Status: "cancelled", SpendUSD: 0.05, OutputTail: "stopped " + r.RunID}, nil
		case <-b:
		case <-ctx.Done():
			f.mu.Lock()
			f.cancelled++
			f.mu.Unlock()
			return nil, ctx.Err()
		}
	}
	z := 0
	return &worker.Result{Status: "ok", ExitCode: &z, OutputTail: "ran " + r.RunID}, nil
}

func (f *fakeLocal) runs() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.got) }

// link enrols a worker identity and returns a Link for it, not yet running.
func (w world) workerLink(t *testing.T, identity, name, version string, local LocalWorker) *Link {
	t.Helper()
	tok, _, err := w.api.MintEnrolment(context.Background(), operator, "test", identity)
	if err != nil {
		t.Fatal(err)
	}
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	chain, err := Enrol(context.Background(), w.addr, "flotilla", w.root.Cert, identity, tok, key)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	return &Link{Addr: w.addr, ServerName: "flotilla", Root: w.root.Cert, Identity: identity, Worker: name, Version: version, Key: key,
		LoadChain: func() ([]byte, error) { mu.Lock(); defer mu.Unlock(); return chain, nil },
		SaveChain: func(b []byte) error { mu.Lock(); defer mu.Unlock(); chain = b; return nil },
		Local:     local, Heartbeat: 200 * time.Millisecond, Log: quiet}
}

func runLink(t *testing.T, l *Link) context.CancelFunc {
	ctx, cancel := context.WithCancel(context.Background())
	go l.Run(ctx)
	t.Cleanup(cancel)
	return cancel
}

func remoteWorld(t *testing.T) world {
	return newWorld(t, func(s *hostlink.Server) {
		s.Workers = map[string]string{"w-bugfix-worker-host": "bugfix"}
		s.Version = "v1"
	})
}

// A run goes to the remote worker and its result comes back; the runner is available only while linked.
func TestRemoteRun(t *testing.T) {
	w := remoteWorld(t)
	r := hostlink.RemoteRunner{S: w.link, Worker: "bugfix"}
	if r.Available() {
		t.Fatal("available before any link")
	}
	if _, err := r.Run(context.Background(), worker.Request{RunID: "r0", TimeoutS: 5}); !errors.Is(err, hostlink.ErrWorkerNotConnected) {
		t.Fatalf("not connected: %v", err)
	}
	local := &fakeLocal{}
	runLink(t, w.workerLink(t, "w-bugfix-worker-host", "bugfix", "v1", local))
	waitFor(t, "the link", r.Available)
	res, err := r.Run(context.Background(), worker.Request{RunID: "r1", Routine: "bugfix/fix", TimeoutS: 5, Env: map[string]string{"A": "b"}})
	if err != nil || res.Status != "ok" || res.OutputTail != "ran r1" {
		t.Fatalf("run: %+v %v", res, err)
	}
	if local.got[0].Routine != "bugfix/fix" || local.got[0].Env["A"] != "b" {
		t.Fatalf("the local request: %+v", local.got[0])
	}
	st := w.link.WorkerStates()
	if len(st) != 1 || !st[0].Connected || st[0].Version != "v1" || st[0].Identity != "w-bugfix-worker-host" {
		t.Fatalf("states: %+v", st)
	}
}

// A link at another version is refused and the refusal is kept for the Workers view; a host identity cannot
// take runs, and a worker identity cannot speak as a host.
func TestRemoteIdentityAndVersion(t *testing.T) {
	w := remoteWorld(t)
	runLink(t, w.workerLink(t, "w-bugfix-worker-host", "bugfix", "v0", &fakeLocal{}))
	waitFor(t, "the refusal", func() bool {
		st := w.link.WorkerStates()
		return len(st) == 1 && strings.Contains(st[0].Refused, "version v0")
	})
	if w.link.WorkerConnected("bugfix") {
		t.Fatal("a link at another version is connected")
	}

	// a host agent's identity sending worker_hello: its first frame must be hello
	g := remoteWorld(t)
	a := g.agent(t, "host-01", &fakeExec{})
	runLink(t, g.workerLink(t, "w-other-worker-host", "bugfix", "v1", &fakeLocal{})) // not a configured identity: a host path
	runLink(t, &Link{Addr: g.addr, ServerName: "flotilla", Root: g.root.Cert, Identity: "host-01", Worker: "bugfix", Version: "v1",
		Key: a.Key, LoadChain: a.LoadChain, SaveChain: a.SaveChain, Local: &fakeLocal{}, Heartbeat: 200 * time.Millisecond, Log: quiet})
	time.Sleep(500 * time.Millisecond)
	if g.link.WorkerConnected("bugfix") {
		t.Fatal("a host identity, or an identity no worker is configured for, took the worker's runs")
	}
}

// The connection drops while a run runs: the link keeps the result and sends it on its next connection,
// and flotillad takes it (RFC-0011 §8).
func TestRemoteLateResult(t *testing.T) {
	w := remoteWorld(t)
	local := &fakeLocal{block: make(chan struct{})}
	l := w.workerLink(t, "w-bugfix-worker-host", "bugfix", "v1", local)
	runLink(t, l)
	r := hostlink.RemoteRunner{S: w.link, Worker: "bugfix"}
	waitFor(t, "the link", r.Available)
	done := make(chan error, 1)
	var res *worker.Result
	go func() {
		var err error
		res, err = r.Run(context.Background(), worker.Request{RunID: "r-late", TimeoutS: 30})
		done <- err
	}()
	waitFor(t, "the run at the worker", func() bool { return local.runs() == 1 })
	l.mu.Lock()
	l.cur.nc.Close() // the connection drops mid-run
	l.mu.Unlock()
	time.Sleep(100 * time.Millisecond)
	close(local.block)
	select {
	case err := <-done:
		if err != nil || res == nil || res.OutputTail != "ran r-late" {
			t.Fatalf("the late result: %+v %v", res, err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the result kept across the reconnect never arrived")
	}
}

// Past the run's time flotillad tells the link to cancel, and the call ends unknown.
func TestRemoteTimeoutCancels(t *testing.T) {
	w := remoteWorld(t)
	local := &fakeLocal{block: make(chan struct{})}
	runLink(t, w.workerLink(t, "w-bugfix-worker-host", "bugfix", "v1", local))
	r := hostlink.RemoteRunner{S: w.link, Worker: "bugfix"}
	waitFor(t, "the link", r.Available)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, err := r.Run(ctx, worker.Request{RunID: "r-slow", TimeoutS: 60})
	if !errors.Is(err, hostlink.ErrUnknown) {
		t.Fatalf("timed out: %v", err)
	}
	waitFor(t, "the cancel at the worker", func() bool { local.mu.Lock(); defer local.mu.Unlock(); return local.cancelled == 1 })
}

// An operator's cancel reaches the worker over the link as its /v1/runs/<id>/cancel; the run's request is
// not ended, so the "cancelled" result and its spend come back to flotillad's Run call.
func TestRemoteCancelStopsTheRunAtTheWorker(t *testing.T) {
	w := remoteWorld(t)
	local := &fakeLocal{block: make(chan struct{}), stop: make(chan struct{})}
	runLink(t, w.workerLink(t, "w-bugfix-worker-host", "bugfix", "v1", local))
	r := hostlink.RemoteRunner{S: w.link, Worker: "bugfix"}
	waitFor(t, "the link", r.Available)
	type out struct {
		res *worker.Result
		err error
	}
	done := make(chan out, 1)
	go func() {
		res, err := r.Run(context.Background(), worker.Request{RunID: "r-cancel", TimeoutS: 600})
		done <- out{res, err}
	}()
	waitFor(t, "the run at the worker", func() bool { return local.runs() == 1 })
	if err := r.Cancel(context.Background(), "r-cancel"); err != nil {
		t.Fatal(err)
	}
	select {
	case o := <-done:
		if o.err != nil || o.res.Status != "cancelled" || o.res.SpendUSD != 0.05 {
			t.Fatalf("the cancelled run's result: %+v %v", o.res, o.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the cancel never reached the worker, or the result never came back")
	}
	local.mu.Lock()
	defer local.mu.Unlock()
	if len(local.stops) != 1 || local.stops[0] != "r-cancel" || local.cancelled != 0 {
		t.Fatalf("stops=%v, local request cancelled %d times (it must stay open for the result)", local.stops, local.cancelled)
	}
}

// A cancel for a run nobody waits for is refused and sends nothing.
func TestRemoteCancelRefusals(t *testing.T) {
	w := remoteWorld(t)
	r := hostlink.RemoteRunner{S: w.link, Worker: "bugfix"}
	if err := r.Cancel(context.Background(), "r-x"); !errors.Is(err, worker.ErrNoSuchRun) {
		t.Fatalf("no such run: %v", err)
	}
	local := &fakeLocal{}
	runLink(t, w.workerLink(t, "w-bugfix-worker-host", "bugfix", "v1", local))
	waitFor(t, "the link", r.Available)
	if err := r.Cancel(context.Background(), "r-x"); !errors.Is(err, worker.ErrNoSuchRun) {
		t.Fatalf("connected, but no such run: %v", err)
	}
	time.Sleep(200 * time.Millisecond)
	local.mu.Lock()
	defer local.mu.Unlock()
	if len(local.stops) != 0 {
		t.Fatalf("a refused cancel reached the worker: %v", local.stops)
	}
}
