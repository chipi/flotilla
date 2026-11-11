package telemetry

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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
	if !strings.Contains(bodies[0], `flotilla_run{autonomy="shadow",environment="operations",routine="triage/cycle",service="flotillad",status="ok",version="sha-abc1234"} 1`) {
		t.Fatalf("run sample: %s", bodies[0])
	}
	if !strings.Contains(bodies[0], `flotilla_spend_day_usd{environment="operations",routine="triage/cycle",service="flotillad"} 0.08`) {
		t.Fatalf("spend sample: %s", bodies[0])
	}
	if strings.Contains(bodies[1], "flotilla_spend_day_usd") || strings.Contains(bodies[1], "flotilla_run_seconds") {
		t.Fatalf("a skipped run must not overwrite the day's spend with 0: %s", bodies[1])
	}
}

func TestPushRetriesTransientFailures(t *testing.T) {
	for name, c := range map[string]struct {
		codes    []int
		attempts int
		lost     bool
	}{
		"502 then ok":          {[]int{502, 204}, 2, false},
		"500, 429 then ok":     {[]int{500, 429, 204}, 3, false},
		"three 502s":           {[]int{502, 502, 502}, 3, true},
		"a 400 is not retried": {[]int{400, 204}, 1, true},
	} {
		t.Run(name, func(t *testing.T) {
			n := 0
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(c.codes[n])
				n++
			}))
			defer ts.Close()
			var slept []string
			var logs strings.Builder
			p := &Pusher{URL: ts.URL, Log: slog.New(slog.NewTextHandler(&logs, nil)),
				Sleep: func(d time.Duration) { slept = append(slept, d.String()) }}
			p.Up()
			if n != c.attempts {
				t.Fatalf("attempts %d, want %d", n, c.attempts)
			}
			if lost := logs.Len() > 0; lost != c.lost {
				t.Fatalf("logged a loss: %v, want %v (%s)", lost, c.lost, logs.String())
			}
			if c.attempts == 3 && strings.Join(slept, ",") != "2s,5s" {
				t.Fatalf("waits %v", slept)
			}
		})
	}
}

func TestPushRetriesAConnectionError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := ts.URL
	ts.Close() // nothing listens: the name resolves, the connection is refused, as while a deploy recreates the proxy
	tries := 0
	p := &Pusher{URL: url, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Sleep: func(time.Duration) { tries++ }}
	p.Up()
	if tries != 2 {
		t.Fatalf("waited %d times, want 2", tries)
	}
}

func TestPushDisabled(t *testing.T) {
	p := &Pusher{URL: "", Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	p.Up() // no URL: nothing to do, no panic
}
