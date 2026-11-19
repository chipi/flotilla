package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/chipi/flotilla/internal/spec"
	"github.com/chipi/flotilla/internal/store"
	"github.com/chipi/flotilla/internal/worker"
)

// inputServer is the API with one recorded input (a triage tick with a ledger behind it) and a stand-in for the live worker's
// snapshot endpoint.
func inputServer(t *testing.T, snapshot http.HandlerFunc) (*httptest.Server, *Server) {
	t.Helper()
	srv := newServer(t, func(*spec.Routine) {})
	ctx := context.Background()
	ns := "triage/cycle"
	srv.Store.WriteFiling(ctx, ns, map[string]string{"fingerprint": "a", "repo": "o/r", "issue": "1"}, "old")
	srv.Store.AppendEntry(ctx, ns, "decision", map[string]string{"fingerprint": "a", "disposition": "file"}, "old")
	mf, me, _ := srv.Store.LedgerMark(ctx, ns)
	if err := srv.Store.NewInput(ctx, store.Input{Key: "live-1", Routine: ns, Kind: store.InputTick, Version: 1, Trigger: "schedule", Actor: "flotillad",
		StartedAt: "2026-11-19T10:00:00.000000000Z", MarkFilings: mf, MarkEntries: me, SnapshotPaths: []string{"state"}, SnapshotState: store.SnapshotHeld}); err != nil {
		t.Fatal(err)
	}
	// the live run writes on after the mark
	srv.Store.WriteFiling(ctx, ns, map[string]string{"fingerprint": "b", "repo": "o/r", "issue": "2"}, "live-1")
	if snapshot != nil {
		ws := httptest.NewServer(snapshot)
		t.Cleanup(ws.Close)
		srv.Workers = map[string]*worker.Client{"triage": worker.New(ws.URL, "worker-secret")}
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, srv
}

func eventsOfKind(t *testing.T, st *store.Store, kind string) []store.Event {
	t.Helper()
	all, err := st.Events(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	var out []store.Event
	for _, e := range all {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

// An input holds production data: an operator's or an admin's, never an agent's, a worker's or a run's.
func TestInputsAreAnOperatorsOnly(t *testing.T) {
	ts, _ := inputServer(t, nil)
	for _, p := range []struct{ method, path string }{
		{"GET", "/v1/inputs/live-1"}, {"GET", "/v1/inputs/live-1?ledger=1"}, {"GET", "/v1/inputs/live-1/state"},
		{"POST", "/v1/inputs/live-1/pin"}, {"POST", "/v1/inputs/live-1/unpin"},
	} {
		for name, tok := range map[string]string{"an agent": agentTok, "a worker": workerTok} {
			if c, _ := call(t, ts, p.method, p.path, tok, ""); c != 403 {
				t.Errorf("%s %s by %s: %d, want 403", p.method, p.path, name, c)
			}
		}
		if c, _ := call(t, ts, p.method, p.path, "", ""); c != 401 {
			t.Errorf("%s %s with no token: %d, want 401", p.method, p.path, c)
		}
	}
	if c, _ := call(t, ts, "GET", "/v1/inputs/live-1", opTok, ""); c != 200 {
		t.Fatalf("the operator reads it: %d", c)
	}
}

func TestInputShowsTheEnvelopeAndWhatIsHeld(t *testing.T) {
	ts, srv := inputServer(t, nil)
	c, out := call(t, ts, "GET", "/v1/inputs/live-1", opTok, "")
	if c != 200 {
		t.Fatalf("%d %v", c, out)
	}
	env := out["envelope"].(map[string]any)
	if env["input_key"] != "live-1" || env["routine"] != "triage/cycle" || env["kind"] != "tick" || env["schema_version"] != float64(1) {
		t.Fatalf("%v", env)
	}
	if _, has := env["ledger"].(map[string]any)["filings"]; has {
		t.Fatal("the rows are not in the envelope unless asked for")
	}
	if out["snapshot_state"] != "held" || out["pinned"] != false || len(out["shadow_runs"].([]any)) != 0 {
		t.Fatalf("%v", out)
	}
	if n := len(eventsOfKind(t, srv.Store, "input.export")); n != 0 {
		t.Fatalf("reading the envelope is not an export: %d events", n)
	}
	if c, _ := call(t, ts, "GET", "/v1/inputs/nope", opTok, ""); c != 404 {
		t.Fatalf("an unknown input: %d", c)
	}
}

func TestInputWithTheLedgerIsAsOfItsMarkAndIsOnTheRecord(t *testing.T) {
	ts, srv := inputServer(t, nil)
	c, out := call(t, ts, "GET", "/v1/inputs/live-1?ledger=1", opTok, "")
	if c != 200 {
		t.Fatalf("%d %v", c, out)
	}
	led := out["envelope"].(map[string]any)["ledger"].(map[string]any)
	fs := led["filings"].([]any)
	if len(fs) != 1 || fs[0].(map[string]any)["fingerprint"] != "a" || len(led["entries"].([]any)) != 1 {
		t.Fatalf("only what was there at the mark: %v", led)
	}
	ev := eventsOfKind(t, srv.Store, "input.export")
	if len(ev) != 1 || ev[0].Actor != "operator" || ev[0].Subject != "live-1" || ev[0].Via != "api" {
		t.Fatalf("production data read is an event naming who: %+v", ev)
	}
}

func TestPinAndUnpin(t *testing.T) {
	ts, srv := inputServer(t, nil)
	ctx := context.Background()
	if c, out := call(t, ts, "POST", "/v1/inputs/live-1/pin", opTok, ""); c != 200 || out["pinned"] != "true" {
		t.Fatalf("%d %v", c, out)
	}
	if in, _ := srv.Store.Input(ctx, "live-1"); !in.Pinned {
		t.Fatal("not pinned")
	}
	if c, out := call(t, ts, "GET", "/v1/inputs/live-1", opTok, ""); c != 200 || out["pinned"] != true {
		t.Fatalf("%d %v", c, out)
	}
	if c, out := call(t, ts, "POST", "/v1/inputs/live-1/unpin", opTok, ""); c != 200 || out["pinned"] != "false" {
		t.Fatalf("%d %v", c, out)
	}
	if in, _ := srv.Store.Input(ctx, "live-1"); in.Pinned {
		t.Fatal("still pinned")
	}
	if len(eventsOfKind(t, srv.Store, "input.pin")) != 1 || len(eventsOfKind(t, srv.Store, "input.unpin")) != 1 {
		t.Fatal("each is an event")
	}
	if c, _ := call(t, ts, "POST", "/v1/inputs/nope/pin", opTok, ""); c != 404 {
		t.Fatalf("an unknown input: %d", c)
	}
}

func TestInputStateStreamsTheWorkersSnapshot(t *testing.T) {
	var auth, path string
	ts, srv := inputServer(t, func(w http.ResponseWriter, r *http.Request) {
		auth, path = r.Header.Get("Authorization"), r.URL.Path
		w.Header().Set("Content-Type", "application/gzip")
		w.Write([]byte("tar.gz bytes of the state"))
	})
	req, _ := http.NewRequest("GET", ts.URL+"/v1/inputs/live-1/state", nil)
	req.Header.Set("Authorization", "Bearer "+opTok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(b) != "tar.gz bytes of the state" || resp.Header.Get("Content-Type") != "application/gzip" {
		t.Fatalf("%d %q %s", resp.StatusCode, b, resp.Header.Get("Content-Type"))
	}
	if auth != "Bearer worker-secret" || path != "/v1/snapshots/live-1" {
		t.Fatalf("flotillad asked the worker with its own token: %q %q", auth, path)
	}
	if ev := eventsOfKind(t, srv.Store, "input.export"); len(ev) != 1 || ev[0].Detail != "the state snapshot" {
		t.Fatalf("%+v", ev)
	}
}

func TestInputStateRefusals(t *testing.T) {
	// a snapshot already deleted, one that never was, and a worker that lost it
	ts, srv := inputServer(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(404) })
	if c, out := call(t, ts, "GET", "/v1/inputs/live-1/state", opTok, ""); c != 404 || !strings.Contains(out["error"].(string), "no longer has") {
		t.Fatalf("the worker lost it: %d %v", c, out)
	}
	ctx := context.Background()
	if err := srv.Store.SetSnapshot(ctx, "live-1", store.SnapshotDeleted, []string{"state"}); err != nil {
		t.Fatal(err)
	}
	if c, out := call(t, ts, "GET", "/v1/inputs/live-1/state", opTok, ""); c != 409 || !strings.Contains(out["error"].(string), "pin") {
		t.Fatalf("a deleted snapshot says how to keep one: %d %v", c, out)
	}
	if c, _ := call(t, ts, "GET", "/v1/inputs/nope/state", opTok, ""); c != 404 {
		t.Fatalf("%d", c)
	}
	// a worker flotillad has no direct line to
	if err := srv.Store.SetSnapshot(ctx, "live-1", store.SnapshotHeld, []string{"state"}); err != nil {
		t.Fatal(err)
	}
	srv.Workers = map[string]*worker.Client{}
	if c, out := call(t, ts, "GET", "/v1/inputs/live-1/state", opTok, ""); c != 409 || !strings.Contains(out["error"].(string), "remote worker") {
		t.Fatalf("%d %v", c, out)
	}
}

// An archive that breaks part-way is a broken download, never a short one that looks whole.
func TestInputStateThatBreaksMidwayIsNotAShortArchive(t *testing.T) {
	ts, srv := inputServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000")
		w.Write([]byte("the first bytes only"))
	})
	req, _ := http.NewRequest("GET", ts.URL+"/v1/inputs/live-1/state", nil)
	req.Header.Set("Authorization", "Bearer "+opTok)
	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		_, err = io.ReadAll(resp.Body)
		resp.Body.Close()
	}
	if err == nil {
		t.Fatal("the client read a cut archive as a whole one")
	}
	if len(eventsOfKind(t, srv.Store, "input.export")) != 1 {
		t.Fatal("the attempt is on the record")
	}
}
