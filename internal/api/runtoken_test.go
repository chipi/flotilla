package api

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/chipi/flotilla/internal/runtoken"
	"github.com/chipi/flotilla/internal/spec"
	"github.com/chipi/flotilla/internal/store"
)

// A server whose worker listener has a registry of run tokens, and the store behind it.
func runServer(t *testing.T) (*httptest.Server, *Server, *runtoken.Registry) {
	t.Helper()
	srv := newServer(t, func(*spec.Routine) {})
	reg := runtoken.New()
	srv.RunTokens = reg
	ts := httptest.NewServer(srv.WorkerHandler())
	t.Cleanup(ts.Close)
	return ts, srv, reg
}

func issue(t *testing.T, reg *runtoken.Registry, run runtoken.Run) string {
	t.Helper()
	if run.Routine == "" {
		run.Routine = "triage/cycle"
	}
	if run.Version == 0 {
		run.Version = 1
	}
	if run.Worker == "" {
		run.Worker = "triage"
	}
	tok, err := reg.Issue(run)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func TestARunTokenWritesUnderItsOwnNamespaceWhateverTheRequestNames(t *testing.T) {
	ts, srv, reg := runServer(t)
	ctx := context.Background()
	tok := issue(t, reg, runtoken.Run{ID: "run-1"})

	// the request names another routine's ledger and another run's id
	if c, out := call(t, ts, "POST", "/v1/ledger/filings?ns=bugfix/fix", tok, `{"row":{"fingerprint":"a","repo":"o/r","issue":"1"},"run_id":"someone-elses"}`); c != 200 {
		t.Fatalf("write filing: %d %v", c, out)
	}
	if c, out := call(t, ts, "POST", "/v1/ledger/entries?ns=bugfix/fix", tok, `{"kind":"decision","row":{"fingerprint":"a","occurrence_id":"o1"},"run_id":"someone-elses"}`); c != 200 {
		t.Fatalf("append entry: %d %v", c, out)
	}
	if other, _ := srv.Store.Filings(ctx, "bugfix/fix", nil); len(other) != 0 {
		t.Fatalf("the namespace the request named was written: %+v", other)
	}
	if es, _ := srv.Store.Entries(ctx, "bugfix/fix", 0, 0); len(es) != 0 {
		t.Fatalf("the namespace the request named got an entry: %+v", es)
	}
	own, _ := srv.Store.Filings(ctx, "triage/cycle", nil)
	if len(own) != 1 || own[0].Row["fingerprint"] != "a" {
		t.Fatalf("the filing lands under the run's own routine: %+v", own)
	}
	if d, _ := srv.Store.DecisionsOfRun(ctx, "run-1"); len(d) != 1 {
		t.Fatalf("the entry carries the run's own id, not the one the body claimed: %+v", d)
	}
	if d, _ := srv.Store.DecisionsOfRun(ctx, "someone-elses"); len(d) != 0 {
		t.Fatalf("a row under another run's id: %+v", d)
	}
	// reads too: the run reads its own ledger whatever it asks for, and may leave ns out
	if c, out := call(t, ts, "GET", "/v1/ledger/filings?ns=bugfix/fix", tok, ""); c != 200 || len(out["filings"].([]any)) != 1 {
		t.Fatalf("a read of another namespace is the run's own: %d %v", c, out)
	}
	if c, out := call(t, ts, "GET", "/v1/ledger/counts", tok, ""); c != 200 || out["filings"] != float64(1) || out["decisions"] != float64(1) {
		t.Fatalf("ns may be left out: %d %v", c, out)
	}
}

func TestARunTokenIsRevokedWhenTheRunEnds(t *testing.T) {
	ts, _, reg := runServer(t)
	tok := issue(t, reg, runtoken.Run{ID: "run-1"})
	if c, _ := call(t, ts, "GET", "/v1/ledger/counts", tok, ""); c != 200 {
		t.Fatalf("live: %d", c)
	}
	reg.Revoke("run-1")
	for name, bad := range map[string]string{"a revoked token": tok, "an unknown one": strings.Repeat("0", 64), "garbage": "x"} {
		if c, _ := call(t, ts, "GET", "/v1/ledger/counts", bad, ""); c != 401 {
			t.Errorf("%s: %d, want 401", name, c)
		}
	}
	if c, _ := call(t, ts, "POST", "/v1/ledger/filings", tok, `{"row":{"fingerprint":"z"}}`); c != 401 {
		t.Errorf("a write with a revoked token: %d, want 401", c)
	}
}

// Two runs of two routines each reach only their own namespace, at the same time.
func TestRunsDoNotShareNamespaces(t *testing.T) {
	ts, srv, reg := runServer(t)
	ctx := context.Background()
	a := issue(t, reg, runtoken.Run{ID: "run-a", Routine: "triage/cycle"})
	b := issue(t, reg, runtoken.Run{ID: "run-b", Routine: "triage/cycle-shadow"})
	call(t, ts, "POST", "/v1/ledger/filings?ns=triage/cycle-shadow", a, `{"row":{"fingerprint":"from-a","issue":"1"}}`)
	call(t, ts, "POST", "/v1/ledger/filings?ns=triage/cycle", b, `{"row":{"fingerprint":"from-b","issue":"2"}}`)
	fa, _ := srv.Store.Filings(ctx, "triage/cycle", nil)
	fb, _ := srv.Store.Filings(ctx, "triage/cycle-shadow", nil)
	if len(fa) != 1 || fa[0].Row["fingerprint"] != "from-a" || len(fb) != 1 || fb[0].Row["fingerprint"] != "from-b" {
		t.Fatalf("each run's filing is in its own namespace: %+v %+v", fa, fb)
	}
}

// A shadow run's token names the shadow's key: it can never write the live routine's ledger, whatever ns it asks.
func TestAShadowRunTokenWritesOnlyTheShadowsNamespace(t *testing.T) {
	ts, srv, reg := runServer(t)
	ctx := context.Background()
	tok := issue(t, reg, runtoken.Run{ID: "shadow-1", Routine: "triage/cycle", Version: 2, Shadow: true, InputKey: "live-1"})
	if c, out := call(t, ts, "POST", "/v1/ledger/filings?ns=triage/cycle", tok, `{"row":{"fingerprint":"s","issue":"9"},"run_id":"live-1"}`); c != 200 {
		t.Fatalf("%d %v", c, out)
	}
	if c, out := call(t, ts, "POST", "/v1/ledger/entries?ns=triage/cycle", tok, `{"kind":"decision","row":{"fingerprint":"s"},"run_id":"live-1"}`); c != 200 {
		t.Fatalf("%d %v", c, out)
	}
	if live, _ := srv.Store.Filings(ctx, "triage/cycle", nil); len(live) != 0 {
		t.Fatalf("a shadow wrote the live ledger: %+v", live)
	}
	if live, _ := srv.Store.Entries(ctx, "triage/cycle", 0, 0); len(live) != 0 {
		t.Fatalf("a shadow appended to the live ledger: %+v", live)
	}
	own, _ := srv.Store.Filings(ctx, "triage/cycle@v2", nil)
	if len(own) != 1 {
		t.Fatalf("the shadow's own namespace: %+v", own)
	}
	if d, _ := srv.Store.DecisionsOfRun(ctx, "shadow-1"); len(d) != 1 {
		t.Fatalf("recorded under the shadow's run, not the live run it claimed: %+v", d)
	}
	if ns, _ := srv.Store.Namespaces(ctx); len(ns) != 0 {
		t.Fatalf("a shadow's namespace is not a live ledger: %v", ns)
	}
}

// What a run reaches on the workers' listener is the worker's three things and nothing else; a ledger import is never a run's.
func TestARunTokenReachesOnlyWhatAWorkerDoes(t *testing.T) {
	ts, srv, reg := runServer(t)
	full := httptest.NewServer(srv.Handler())
	t.Cleanup(full.Close)
	tok := issue(t, reg, runtoken.Run{ID: "run-1"})
	for _, p := range []struct{ method, path string }{
		{"GET", "/v1/status"}, {"GET", "/v1/runs"}, {"GET", "/v1/events"}, {"GET", "/v1/actions"},
		{"POST", "/v1/routines/triage/cycle/run"}, {"POST", "/v1/routines/triage/cycle/autonomy"},
		{"POST", "/v1/fleets/triage/stop"}, {"POST", "/v1/runs/run-1/cancel"}, {"POST", "/v1/holds"},
	} {
		if c, _ := call(t, full, p.method, p.path, tok, `{"autonomy":"auto"}`); c != 403 {
			t.Errorf("%s %s with a run token: %d, want 403", p.method, p.path, c)
		}
	}
	// an import is once, and a worker's own act: a run's own token is refused in the handler
	if c, out := call(t, full, "POST", "/v1/ledger/import?ns=triage/cycle", tok, `{"filings":[{"fingerprint":"a"}]}`); c != 403 {
		t.Fatalf("import with a run token: %d %v, want 403", c, out)
	}
	if n, _ := srv.Store.LedgerCounts(context.Background(), "triage/cycle"); n != (store.LedgerCounts{}) {
		t.Fatalf("a run imported a ledger: %+v", n)
	}
	if c, _ := call(t, ts, "GET", "/v1/cases", tok, ""); c != 200 {
		t.Fatalf("a run reads cases on the workers' listener (a review routine): %d", c)
	}
}

func TestAShadowRunsNoteIsARecordNotANote(t *testing.T) {
	ts, srv, reg := runServer(t)
	ctx := context.Background()
	id, _, err := srv.Store.OpenCase(ctx, store.NewCase{Kind: "backup", Target: "t", Keys: []store.Key{{Kind: "alert", Value: "a"}}, Canonical: store.Key{Kind: "alert", Value: "a"}}, "ops/m", "d")
	if err != nil {
		t.Fatal(err)
	}
	live := issue(t, reg, runtoken.Run{ID: "run-1", Routine: "ops/case-review"})
	shadow := issue(t, reg, runtoken.Run{ID: "shadow-1", Routine: "ops/case-review", Version: 2, Shadow: true, InputKey: "run-1"})
	if c, out := call(t, ts, "POST", "/v1/cases/"+id+"/notes", shadow, `{"text":"remember this"}`); c != 200 {
		t.Fatalf("a shadow's note is taken, as a record: %d %v", c, out)
	}
	if n, _ := srv.Store.Notes(ctx, ""); len(n) != 0 {
		t.Fatalf("a shadow left a note: %+v", n)
	}
	if evs, _ := srv.Store.CaseEvents(ctx, id); len(evs) != 1 {
		t.Fatalf("a shadow's note is not on the case's timeline: %+v", evs)
	}
	recs, _ := srv.Store.ShadowRecords(ctx, store.ShadowRecordFilter{InputKey: "run-1", Kind: "note"})
	if len(recs) != 1 || recs[0].Subject != id || recs[0].Routine != "ops/case-review@v2" || !strings.Contains(string(recs[0].Payload), "remember this") {
		t.Fatalf("the note as a shadow record: %+v", recs)
	}
	if c, out := call(t, ts, "POST", "/v1/cases/"+id+"/notes", live, `{"text":"remember this"}`); c != 200 {
		t.Fatalf("a live run still proposes notes: %d %v", c, out)
	}
	if c, _ := call(t, ts, "GET", "/v1/cases/"+id, shadow, ""); c != 200 {
		t.Fatalf("a shadow reads cases: %d", c)
	}
}

// The worker's own token still works beside run tokens ("accept"), and resolves the same way every time.
func TestTheWorkerTokenStillWorksAndAnAmbiguousOneResolvesTheSameEveryTime(t *testing.T) {
	ts, srv, _ := runServer(t)
	if c, _ := call(t, ts, "GET", "/v1/ledger/counts?ns=triage/cycle", workerTok, ""); c != 200 {
		t.Fatalf("the worker's token: %d", c)
	}
	shared := map[string]string{"zeta": workerTok, "alpha": workerTok, "mid": workerTok, "solo": "solo-token-0123456789abcdef0123456789abc"}
	srv.Callers = WorkerCallers(shared)
	for i := 0; i < 50; i++ {
		who, err := srv.WhoIs(context.Background(), workerTok)
		if err != nil || who == nil || who.Name != "worker:alpha" {
			t.Fatalf("a token three workers share is the first by name, always: %+v %v", who, err)
		}
	}
	if got := SharedWorkerTokens(shared); len(got) != 1 || got[0] != "alpha and mid and zeta" {
		t.Fatalf("the shared token is reported: %v", got)
	}
	if got := SharedWorkerTokens(map[string]string{"a": "x", "b": "y"}); len(got) != 0 {
		t.Fatalf("distinct tokens share nothing: %v", got)
	}
}

// With run tokens required, flotillad does not make a caller of any worker's own token: it opens nothing, and a run's does.
func TestWithoutWorkerCallersOnlyRunTokensOpenTheListener(t *testing.T) {
	ts, srv, reg := runServer(t)
	srv.Callers = srv.Callers[:2] // the operator and the agent: what main.go assembles when run_tokens is required
	if c, _ := call(t, ts, "GET", "/v1/ledger/counts?ns=triage/cycle", workerTok, ""); c != 401 {
		t.Fatalf("the worker's own token with run tokens required: %d, want 401", c)
	}
	if c, _ := call(t, ts, "POST", "/v1/ledger/filings?ns=triage/cycle", workerTok, `{"row":{"fingerprint":"a"}}`); c != 401 {
		t.Fatalf("a write with it: %d, want 401", c)
	}
	tok := issue(t, reg, runtoken.Run{ID: "run-1"})
	if c, _ := call(t, ts, "GET", "/v1/ledger/counts", tok, ""); c != 200 {
		t.Fatalf("a run's own token: %d", c)
	}
	if c, _ := call(t, ts, "GET", "/healthz", "", ""); c != 200 {
		t.Fatalf("healthz needs none: %d", c)
	}
}

// A server with no registry (as before this change) knows no run tokens.
func TestNoRegistryNoRunTokens(t *testing.T) {
	srv := newServer(t, func(*spec.Routine) {})
	ts := httptest.NewServer(srv.WorkerHandler())
	t.Cleanup(ts.Close)
	if c, _ := call(t, ts, "GET", "/v1/ledger/counts?ns=a/b", strings.Repeat("a", 64), ""); c != 401 {
		t.Fatalf("%d", c)
	}
}
