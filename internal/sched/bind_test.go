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
	if fr.got[0].Command != "python3 one.py" {
		t.Fatalf("v1's command: %q", fr.got[0].Command)
	}
	if err := s.Bind("triage/cycle", 7); err == nil {
		t.Fatal("a version that is not loaded cannot be bound")
	}
	if err := s.Bind("nope/none", 1); err == nil {
		t.Fatal("a routine that is not loaded cannot be bound")
	}

	fr.block = make(chan struct{})
	done := make(chan error, 1)
	go func() { _, err := s.RunNow(ctx, "triage/cycle", "manual", "operator"); done <- err }()
	for i := 0; i < 200; i++ { // the run is registered, and its request reached the worker
		fr.mu.Lock()
		n := len(fr.got)
		fr.mu.Unlock()
		if n == 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := s.Bind("triage/cycle", 2); err != nil {
		t.Fatal(err)
	}
	close(fr.block)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if fr.got[1].Command != "python3 one.py" {
		t.Fatalf("the run in flight keeps its version: %q", fr.got[1].Command)
	}
	if r, err := s.RunNow(ctx, "triage/cycle", "manual", "operator"); err != nil || r.Version != 2 {
		t.Fatalf("after bind: %+v %v", r, err)
	}
	if fr.got[2].Command != "python3 two.py" {
		t.Fatalf("v2's command: %q", fr.got[2].Command)
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
