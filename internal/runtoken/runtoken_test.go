package runtoken

import (
	"errors"
	"fmt"
	"regexp"
	"sync"
	"testing"
)

func live(id string) Run {
	return Run{ID: id, Routine: "triage/cycle", Version: 1, Worker: "triage"}
}

func TestIssueResolveRevoke(t *testing.T) {
	r := New()
	tok, err := r.Issue(live("run-1"))
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(tok) {
		t.Fatalf("a token is 32 random bytes in hex: %q", tok)
	}
	got, ok := r.Resolve(tok)
	if !ok || got != live("run-1") || got.Key() != "triage/cycle" {
		t.Fatalf("resolve: %+v %v", got, ok)
	}
	if r.Len() != 1 {
		t.Fatalf("one live token: %d", r.Len())
	}
	r.Revoke("run-1")
	if _, ok := r.Resolve(tok); ok {
		t.Fatal("a revoked token must not resolve")
	}
	r.Revoke("run-1") // twice is fine
	r.Revoke("never-issued")
	if r.Len() != 0 {
		t.Fatalf("no live tokens: %d", r.Len())
	}
}

func TestUnknownAndEmptyTokensDoNotResolve(t *testing.T) {
	r := New()
	tok, _ := r.Issue(live("run-1"))
	for _, bad := range []string{"", "x", tok + "0", tok[:63], "Bearer " + tok, " " + tok} {
		if run, ok := r.Resolve(bad); ok {
			t.Errorf("%q resolved to %+v", bad, run)
		}
	}
}

func TestEachRunHasItsOwnToken(t *testing.T) {
	r := New()
	a, _ := r.Issue(live("run-1"))
	b, _ := r.Issue(live("run-2"))
	if a == b {
		t.Fatal("two runs, two tokens")
	}
	ra, _ := r.Resolve(a)
	rb, _ := r.Resolve(b)
	if ra.ID != "run-1" || rb.ID != "run-2" {
		t.Fatalf("each token is its own run's: %+v %+v", ra, rb)
	}
	r.Revoke("run-1")
	if _, ok := r.Resolve(b); !ok {
		t.Fatal("revoking one run leaves the other's token")
	}
	if _, err := r.Issue(live("run-2")); !errors.Is(err, ErrIssued) {
		t.Fatalf("a run has one token: %v", err)
	}
	// after a revoke the id could be issued again (a retry of a start), and the old token stays dead
	c, err := r.Issue(live("run-1"))
	if err != nil || c == a {
		t.Fatalf("reissue after revoke: %q %v", c, err)
	}
	if _, ok := r.Resolve(a); ok {
		t.Fatal("the first token of a run stays revoked")
	}
}

// A shadow run's token resolves to the shadow's own key, whatever the live routine is called.
func TestAShadowTokenNamesTheShadowKey(t *testing.T) {
	r := New()
	tok, err := r.Issue(Run{ID: "s-1", Routine: "triage/cycle", Version: 2, Worker: "triage-shadow", Shadow: true, InputKey: "live-1"})
	if err != nil {
		t.Fatal(err)
	}
	run, _ := r.Resolve(tok)
	if !run.Shadow || run.Key() != "triage/cycle@v2" || run.InputKey != "live-1" || run.Routine != "triage/cycle" {
		t.Fatalf("%+v key %s", run, run.Key())
	}
}

func TestIssueRefusesWhatIsNotARun(t *testing.T) {
	r := New()
	for name, bad := range map[string]Run{
		"no id":         {Routine: "a/b", Version: 1},
		"no routine":    {ID: "x", Version: 1},
		"a shadow key":  {ID: "x", Routine: "a/b@v2", Version: 2},
		"version 0":     {ID: "x", Routine: "a/b"},
		"negative vers": {ID: "x", Routine: "a/b", Version: -1},
	} {
		if _, err := r.Issue(bad); err == nil {
			t.Errorf("%s: want a refusal", name)
		}
	}
	if r.Len() != 0 {
		t.Fatalf("a refused issue leaves nothing: %d", r.Len())
	}
}

func TestOnlyHashesAreKept(t *testing.T) {
	r := New()
	tok, _ := r.Issue(live("run-1"))
	if s := fmt.Sprintf("%v %+v", r.byTok, r.byRun); regexp.MustCompile(tok).MatchString(s) {
		t.Fatal("the registry holds a token in clear")
	}
}

func TestConcurrentUse(t *testing.T) {
	r := New()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := fmt.Sprintf("run-%d", i)
			tok, err := r.Issue(live(id))
			if err != nil {
				t.Error(err)
				return
			}
			if run, ok := r.Resolve(tok); !ok || run.ID != id {
				t.Errorf("%s: %+v %v", id, run, ok)
			}
			r.Revoke(id)
		}()
	}
	wg.Wait()
	if r.Len() != 0 {
		t.Fatalf("all revoked: %d", r.Len())
	}
}
