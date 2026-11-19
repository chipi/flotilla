package sched

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/chipi/flotilla/internal/runtoken"
	"github.com/chipi/flotilla/internal/worker"
)

// tokenRunner is a worker that, as the run is in flight, asks the registry who its token is.
type tokenRunner struct {
	reg *runtoken.Registry
	res *worker.Result
	err error

	mu     sync.Mutex
	reqs   []worker.Request
	during []runtoken.Run
	found  []bool
}

func (r *tokenRunner) Run(_ context.Context, req worker.Request) (*worker.Result, error) {
	run, ok := r.reg.Resolve(req.APIToken)
	r.mu.Lock()
	r.reqs, r.during, r.found = append(r.reqs, req), append(r.during, run), append(r.found, ok)
	r.mu.Unlock()
	return r.res, r.err
}

func TestEveryRunGetsItsOwnTokenForItsDuration(t *testing.T) {
	s, fr, _ := setup(t, 2)
	reg := runtoken.New()
	s.RunTokens = reg
	tr := &tokenRunner{reg: reg, res: fr.res}
	s.Workers["triage"] = tr

	a, err := s.RunNow(context.Background(), "triage/cycle", "manual", "operator")
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.RunNow(context.Background(), "triage/cycle", "manual", "operator")
	if err != nil {
		t.Fatal(err)
	}
	if len(tr.reqs) != 2 {
		t.Fatalf("two runs: %d", len(tr.reqs))
	}
	for i, id := range []string{a.ID, b.ID} {
		if !tr.found[i] {
			t.Fatalf("run %d: its token did not resolve while it ran", i)
		}
		got := tr.during[i]
		if got.ID != id || got.Routine != "triage/cycle" || got.Version != 1 || got.Worker != "triage" || got.Shadow || got.InputKey != "" || got.Key() != "triage/cycle" {
			t.Fatalf("run %d: the token stands for %+v", i, got)
		}
		if len(tr.reqs[i].APIToken) != 64 {
			t.Fatalf("run %d: the request carries a 64-hex token: %q", i, tr.reqs[i].APIToken)
		}
		for k, v := range tr.reqs[i].Env {
			if strings.Contains(v, tr.reqs[i].APIToken) {
				t.Fatalf("run %d: the token is in the environment (%s)", i, k)
			}
		}
		if tr.reqs[i].Shadow {
			t.Fatalf("run %d: a live run is not a shadow", i)
		}
		if _, ok := reg.Resolve(tr.reqs[i].APIToken); ok {
			t.Fatalf("run %d: the token still resolves after the run ended", i)
		}
	}
	if tr.reqs[0].APIToken == tr.reqs[1].APIToken {
		t.Fatal("two runs, two tokens")
	}
	if reg.Len() != 0 {
		t.Fatalf("no token outlives its run: %d", reg.Len())
	}
}

func TestTheTokenIsRevokedWhenTheRunFails(t *testing.T) {
	for name, fail := range map[string]struct {
		res *worker.Result
		err error
	}{
		"the worker is unreachable": {err: worker.ErrUnreachable},
		"the call is lost":          {err: errors.New("connection reset")},
		"the run errors":            {res: &worker.Result{Status: "error", OutputTail: "boom"}},
	} {
		t.Run(name, func(t *testing.T) {
			s, _, _ := setup(t, 2)
			reg := runtoken.New()
			s.RunTokens = reg
			tr := &tokenRunner{reg: reg, res: fail.res, err: fail.err}
			s.Workers["triage"] = tr
			if _, err := s.RunNow(context.Background(), "triage/cycle", "manual", "operator"); err != nil {
				t.Fatal(err)
			}
			if !tr.found[0] || reg.Len() != 0 {
				t.Fatalf("resolved during %v, live tokens after %d", tr.found[0], reg.Len())
			}
		})
	}
}

// A run that is skipped never reaches a worker, so it never has a token.
func TestASkippedRunHasNoToken(t *testing.T) {
	s, fr, _ := setup(t, 2)
	reg := runtoken.New()
	s.RunTokens = reg
	if err := s.Store.SetControl(context.Background(), "triage/cycle", KeyPaused, "1", "operator", "test"); err != nil {
		t.Fatal(err)
	}
	r, err := s.RunNow(context.Background(), "triage/cycle", "manual", "operator")
	if err != nil || r.Status != "skipped" {
		t.Fatalf("%+v %v", r, err)
	}
	if len(fr.got) != 0 || reg.Len() != 0 {
		t.Fatalf("no request, no token: %d %d", len(fr.got), reg.Len())
	}
}

// Without a registry (a scheduler built as before) a run carries no token and runs as it did.
func TestNoRegistryNoToken(t *testing.T) {
	s, fr, _ := setup(t, 2)
	if _, err := s.RunNow(context.Background(), "triage/cycle", "manual", "operator"); err != nil {
		t.Fatal(err)
	}
	if fr.got[0].APIToken != "" {
		t.Fatalf("a token without a registry: %q", fr.got[0].APIToken)
	}
}

// The token is for the version the run started as, whatever the routine is bound to afterwards.
func TestTheTokenNamesTheVersionTheRunStartedAs(t *testing.T) {
	s, fr, _ := setup(t, 2)
	reg := runtoken.New()
	s.RunTokens = reg
	s.Routine("triage/cycle").Version = 3
	tr := &tokenRunner{reg: reg, res: fr.res}
	s.Workers["triage"] = tr
	if _, err := s.RunNow(context.Background(), "triage/cycle", "manual", "operator"); err != nil {
		t.Fatal(err)
	}
	if tr.during[0].Version != 3 {
		t.Fatalf("version %d, want 3", tr.during[0].Version)
	}
}
