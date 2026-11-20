package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/chipi/flotilla/internal/api"
	"github.com/chipi/flotilla/internal/worker"
)

func healthServer(t *testing.T, body string) *worker.Client {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) }))
	t.Cleanup(ts.Close)
	return worker.New(ts.URL, "tok")
}

// Under the default (required) a reachable worker that does not list run_token in /healthz stops the start and -check, naming it; one that does passes;
// an unreachable one is not judged; accept never refuses and -check warns.
func TestAWorkerWithoutRunTokensIsRefusedWhenRequired(t *testing.T) {
	old := healthServer(t, `{"ok":true,"worker":"old","version":"v","running":[],"mode":"live"}`)
	capable := healthServer(t, `{"ok":true,"worker":"new","version":"v","running":[],"mode":"live","caps":["run_token"]}`)
	down := worker.New("http://127.0.0.1:1", "tok")
	cfg := &Config{RunTokens: RunTokensRequired, Workers: map[string]WorkerConfig{"old": {}, "new": {}, "down": {}}}
	clients := map[string]*worker.Client{"old": old, "new": capable, "down": down}
	err := requireRunTokenCaps(context.Background(), cfg, clients)
	if err == nil || !strings.Contains(err.Error(), "worker old does not carry run tokens") || strings.Contains(err.Error(), "new") {
		t.Fatalf("got %v", err)
	}
	lines := strings.Join(workerCapLines(context.Background(), cfg, clients), "\n")
	for _, want := range []string{"worker new: run-token capable", "worker old: NOT run-token capable (update it", "worker down: not reachable"} {
		if !strings.Contains(lines, want) {
			t.Errorf("-check lacks %q:\n%s", want, lines)
		}
	}
	if strings.Contains(lines, "WARNING") {
		t.Error("a warning under required")
	}
	cfg.RunTokens = RunTokensAccept
	if err := requireRunTokenCaps(context.Background(), cfg, clients); err != nil {
		t.Errorf("accept refused a worker: %v", err)
	}
	if lines := strings.Join(workerCapLines(context.Background(), cfg, clients), "\n"); !strings.Contains(lines, "WARNING run_tokens: accept") {
		t.Errorf("no warning under accept:\n%s", lines)
	}
}

// The listener's callers under required are the callers file's alone; under accept each live worker's own token is added, never a shadow
// worker's.
func TestTheListenersCallersCarryNoWorkerTokenWhenRequired(t *testing.T) {
	cfg := &Config{RunTokens: RunTokensRequired, Workers: map[string]WorkerConfig{"live": {}, "shadow": {Shadow: true}}}
	tokens := map[string]string{"live": strings.Repeat("l", 40), "shadow": strings.Repeat("s", 40)}
	file := []api.Caller{{Name: "operator", Role: api.RoleOperator}}
	if got := listenerCallers(cfg, file, tokens); len(got) != 1 || got[0].Name != "operator" {
		t.Fatalf("required: %+v", got)
	}
	cfg.RunTokens = RunTokensAccept
	got := listenerCallers(cfg, file, tokens)
	if len(got) != 2 || got[1].Name != "worker:live" {
		t.Fatalf("accept: %+v", got)
	}
}

// The upgrade path (deploy/server/deploy.sh): -check runs while the previous tag's workers still answer, so against old workers (no run_token in
// /healthz) it must pass and say so; the start after `up`, with the workers recreated, refuses one that is still old and takes the new. -check's
// exit restored (the check refusing an old worker) turns this red.
func TestCheckPassesAgainstOldWorkersAndTheStartRefusesAStillOldOne(t *testing.T) {
	old := healthServer(t, `{"ok":true,"worker":"old","version":"v","running":[],"mode":"live"}`)
	capable := healthServer(t, `{"ok":true,"worker":"new","version":"v","running":[],"mode":"live","caps":["run_token"]}`)
	cfg := &Config{RunTokens: RunTokensRequired, Workers: map[string]WorkerConfig{"old": {}, "new": {}}}
	clients := map[string]*worker.Client{"old": old, "new": capable}
	if err := enforceRunTokenCaps(context.Background(), cfg, clients, true); err != nil {
		t.Fatalf("-check refused an old worker, which an upgrade's check always meets: %v", err)
	}
	if lines := strings.Join(workerCapLines(context.Background(), cfg, clients), "\n"); !strings.Contains(lines, "worker old: NOT run-token capable") {
		t.Errorf("-check does not say which worker is old:\n%s", lines)
	}
	if err := enforceRunTokenCaps(context.Background(), cfg, clients, false); err == nil || !strings.Contains(err.Error(), "worker old does not carry run tokens") {
		t.Fatalf("the start did not refuse the old worker: %v", err)
	}
	delete(clients, "old")
	if err := enforceRunTokenCaps(context.Background(), cfg, clients, false); err != nil {
		t.Fatalf("the start refused after the workers were updated: %v", err)
	}
}
