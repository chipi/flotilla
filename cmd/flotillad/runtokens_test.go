package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
	for _, want := range []string{"worker new: run-token capable", "worker old: NOT run-token capable (update it)", "worker down: not reachable"} {
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
