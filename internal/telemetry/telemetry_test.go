package telemetry

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPushFormat(t *testing.T) {
	var bodies []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/import/prometheus" {
			t.Errorf("path %s", r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
	}))
	defer ts.Close()
	p := &Pusher{URL: ts.URL, Env: "operations", Version: "sha-abc1234", Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	p.Run("triage/cycle", "ok", "shadow", 12.5, 0.08)
	p.Run("triage/cycle", "skipped", "shadow", 0, 0)
	if !strings.Contains(bodies[0], `flotilla_run{environment="operations",job="triage/cycle",service="flotillad",stage="shadow",status="ok",version="sha-abc1234"} 1`) {
		t.Fatalf("run sample: %s", bodies[0])
	}
	if !strings.Contains(bodies[0], `flotilla_spend_day_usd{environment="operations",job="triage/cycle",service="flotillad"} 0.08`) {
		t.Fatalf("spend sample: %s", bodies[0])
	}
	if strings.Contains(bodies[1], "flotilla_spend_day_usd") || strings.Contains(bodies[1], "flotilla_run_seconds") {
		t.Fatalf("a skipped run must not overwrite the day's spend with 0: %s", bodies[1])
	}
}

func TestPushDisabled(t *testing.T) {
	p := &Pusher{URL: "", Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	p.Up() // no URL: nothing to do, no panic
}
