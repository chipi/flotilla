package sched

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/chipi/flotilla/internal/spec"
)

func foldered(t *testing.T) []*spec.Routine {
	t.Helper()
	d := t.TempDir()
	for rel, body := range map[string]string{
		"triage/cycle/routine.yaml": "fleet: triage\nroutine: cycle\ndescription: d\nenabled: true\nworker: triage\ntrigger: {every: 10m}\nautonomy: record\nbudget: {day_usd: 2}\nfreshness: 35m\n",
		"triage/cycle/v1/spec.yaml": "run: {command: \"python3 one.py\", workdir: signal-fleet/mvp, timeout: 20m}\n",
		"triage/cycle/v2/spec.yaml": "run: {command: \"python3 two.py\", workdir: signal-fleet/mvp, timeout: 20m}\n",
	} {
		p := filepath.Join(d, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	rs, err := spec.Load(d)
	if err != nil {
		t.Fatal(err)
	}
	return rs
}

// Bind swaps in the routine as another version: the next run starts that version's command and records its
// number. A run in flight keeps the version it started with.
func TestBindRunsTheBoundVersion(t *testing.T) {
	s, fr, _ := setup(t, 2)
	s.SetRoutines(foldered(t))
	ctx := context.Background()
	if r, err := s.RunNow(ctx, "triage/cycle", "manual", "operator"); err != nil || r.Version != 1 {
		t.Fatalf("first run: %+v %v", r, err)
	}
	if c := fr.commandOf(0); c != "python3 one.py" {
		t.Fatalf("v1's command: %q", c)
	}
	if err := s.Bind("triage/cycle", 7); err == nil {
		t.Fatal("a version that is not loaded cannot be bound")
	}
	if err := s.Bind("nope/none", 1); err == nil {
		t.Fatal("a routine that is not loaded cannot be bound")
	}

	fr.block = make(chan struct{})
	fr.reached = make(chan struct{}, 4)
	done := make(chan error, 1)
	go func() { _, err := s.RunNow(ctx, "triage/cycle", "manual", "operator"); done <- err }()
	select { // the run is registered, and its request reached the worker
	case <-fr.reached:
	case <-time.After(10 * time.Second):
		t.Fatal("the run never reached the worker")
	}
	if err := s.Bind("triage/cycle", 2); err != nil {
		t.Fatal(err)
	}
	close(fr.block)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if c := fr.commandOf(1); c != "python3 one.py" {
		t.Fatalf("the run in flight keeps its version: %q", c)
	}
	if r, err := s.RunNow(ctx, "triage/cycle", "manual", "operator"); err != nil || r.Version != 2 {
		t.Fatalf("after bind: %+v %v", r, err)
	}
	if c := fr.commandOf(2); c != "python3 two.py" {
		t.Fatalf("v2's command: %q", c)
	}
	runs, _ := s.Store.Runs(ctx, "triage/cycle", 10)
	got := map[int]int{}
	for _, r := range runs {
		got[r.Version]++
	}
	if len(runs) != 3 || got[1] != 2 || got[2] != 1 {
		t.Fatalf("recorded versions: %v", got)
	}
	if s.Routine("triage/cycle").Version != 2 || len(s.Routines()) != 1 {
		t.Fatalf("the scheduler now holds %+v", s.Routine("triage/cycle"))
	}
}

func (f *fakeRunner) commandOf(i int) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.got[i].Command
}

// Runs started while another goroutine flips the bound version, under -race: every run records the version
// whose command its worker was given, whichever side of a flip it started on.
func TestRunsRecordTheVersionTheirWorkerRan(t *testing.T) {
	s, fr, _ := setup(t, 1000)
	rs := foldered(t)
	rs[0].Budget.DayUSD = 1000 // 60 runs at the fake's $0.4 each
	s.SetRoutines(rs)
	ctx := context.Background()
	stop := make(chan struct{})
	flipped := make(chan struct{})
	go func() {
		defer close(flipped)
		for n := 1; ; n = 3 - n {
			select {
			case <-stop:
				return
			default:
			}
			if err := s.Bind("triage/cycle", n); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	byRun := map[string]int{}
	for i := 0; i < 60; i++ {
		r, err := s.RunNow(ctx, "triage/cycle", "manual", "operator")
		if err != nil {
			t.Fatal(err)
		}
		if r.Status != "ok" {
			t.Fatalf("run %d: %s %s", i, r.Status, r.Detail)
		}
		byRun[r.ID] = r.Version
	}
	close(stop)
	<-flipped
	fr.mu.Lock()
	defer fr.mu.Unlock()
	if len(fr.got) != 60 {
		t.Fatalf("%d requests", len(fr.got))
	}
	seen := map[int]int{}
	for _, req := range fr.got {
		v, ok := byRun[req.RunID]
		if !ok {
			t.Fatalf("a request for run %s that RunNow did not return", req.RunID)
		}
		want := map[int]string{1: "python3 one.py", 2: "python3 two.py"}[v]
		if req.Command != want {
			t.Fatalf("run %s was recorded as v%d and its worker ran %q", req.RunID, v, req.Command)
		}
		seen[v]++
	}
	if seen[1] == 0 || seen[2] == 0 {
		t.Logf("only one version was bound by any run (%v): the flip did not interleave this time", seen)
	}
}
