package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/chipi/flotilla/internal/spec"
	"github.com/chipi/flotilla/internal/store"
)

func pairsServer(t *testing.T) (*Server, func(string, string, string) (int, map[string]any)) {
	t.Helper()
	srv := newServer(t, func(*spec.Routine) {})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	ctx := context.Background()
	if _, err := srv.Store.EnsureVersions(ctx, "triage/cycle", []store.LoadedVersion{{Version: 1, Hash: "h1", Manifest: map[string]string{}}, {Version: 2, Hash: "h2", Manifest: map[string]string{}}}, "flotillad"); err != nil {
		t.Fatal(err)
	}
	for i, start := range []string{"2026-11-19T10:00:00.000000000Z", "2026-11-19T12:00:00.000000000Z"} {
		key := []string{"live-1", "live-2"}[i]
		srv.Store.NewInput(ctx, store.Input{Key: key, Routine: "triage/cycle", Kind: store.InputTick, Version: 1, StartedAt: start})
		id, _ := srv.Store.QueueShadowRun(ctx, store.ShadowRun{InputKey: key, Routine: "triage/cycle", Version: 2, VersionHash: "h2"})
		srv.Store.ShadowRunStep(ctx, id, store.ShadowRunning, "", "s-"+key)
		srv.Store.ShadowRunStep(ctx, id, store.ShadowDone, "", "")
		srv.Store.AppendEntry(ctx, "triage/cycle", "decision", map[string]string{"fingerprint": "fp-" + key, "disposition": "file"}, key)
		disp := []string{"file", "dismiss"}[i]
		srv.Store.RecordShadow(ctx, store.ShadowRecord{RunID: "s-" + key, Routine: "triage/cycle@v2", Version: 2, InputKey: key, Subject: "fp-" + key, Kind: "decision",
			Payload: json.RawMessage(`{"disposition":"` + disp + `"}`)})
	}
	return srv, func(path, tok, _ string) (int, map[string]any) { return call(t, ts, "GET", path, tok, "") }
}

func TestThePairsEndpoint(t *testing.T) {
	_, get := pairsServer(t)
	c, out := get("/v1/routines/triage/cycle/pairs?version=2", opTok, "")
	if c != 200 || out["paired"] != float64(2) || out["differ"] != float64(1) || out["unpaired"] != float64(0) || out["live_version"] != float64(1) || len(out["pairs"].([]any)) != 2 {
		t.Fatalf("%d %v", c, out)
	}
	first := out["pairs"].([]any)[0].(map[string]any)
	if first["input_key"] != "live-2" || first["same"] != false || first["shadow"].(map[string]any)["outcome"] != "dismiss" || first["live"].(map[string]any)["outcome"] != "file" {
		t.Fatalf("newest first, each side's outcome: %v", first)
	}
	if c, out := get("/v1/routines/triage/cycle/pairs?version=v2&show=differ", opTok, ""); c != 200 || len(out["pairs"].([]any)) != 1 || out["pairs"].([]any)[0].(map[string]any)["input_key"] != "live-2" || out["paired"] != float64(2) {
		t.Fatalf("differ keeps one pair and the whole set's counts: %d %v", c, out)
	}
	if c, out := get("/v1/routines/triage/cycle/pairs?version=2&since=2026-11-19T11:00:00Z", opTok, ""); c != 200 || out["paired"] != float64(1) || out["pairs"].([]any)[0].(map[string]any)["input_key"] != "live-2" {
		t.Fatalf("since: %d %v", c, out)
	}
	if c, out := get("/v1/routines/triage/cycle/pairs?version=2&until=2026-11-19T11:00:00Z", opTok, ""); c != 200 || out["paired"] != float64(1) || out["pairs"].([]any)[0].(map[string]any)["input_key"] != "live-1" {
		t.Fatalf("until: %d %v", c, out)
	}
	for name, p := range map[string]string{"no version": "/v1/routines/triage/cycle/pairs", "a bad time": "/v1/routines/triage/cycle/pairs?version=2&since=yesterday",
		"a bad show": "/v1/routines/triage/cycle/pairs?version=2&show=best"} {
		if c, _ := get(p, opTok, ""); c != 400 {
			t.Errorf("%s: %d, want 400", name, c)
		}
	}
	if c, _ := get("/v1/routines/triage/nothing/pairs?version=2", opTok, ""); c != 404 {
		t.Errorf("an unknown routine: %d", c)
	}
	if c, _ := get("/v1/routines/triage/cycle/pairs?version=2", agentTok, ""); c != 200 {
		t.Errorf("an agent reads pairs: %d", c)
	}
	if c, _ := get("/v1/routines/triage/cycle/pairs?version=2", workerTok, ""); c != 403 {
		t.Errorf("a worker reaches the ledger only: %d", c)
	}
	if c, _ := get("/v1/routines/triage/cycle/pairs?version=2", "", ""); c != 401 {
		t.Errorf("no token: %d", c)
	}
}

// A promote's default acceptance, pairs, is satisfiable now: the store's own count is what a promote reads.
func TestPromoteReadsTheRealPairCount(t *testing.T) {
	srv, _ := pairsServer(t)
	if n, err := srv.Store.PairsFor(context.Background(), "triage/cycle", 2); err != nil || n != 2 {
		t.Fatalf("%d %v", n, err)
	}
	vv, err := srv.RoutineVersions(context.Background(), "triage/cycle")
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range vv.Versions {
		if v.Version == 2 && v.Pairs != 2 {
			t.Fatalf("the versions view counts them: %+v", v)
		}
	}
}
