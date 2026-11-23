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
	p.Run("triage/cycle", "ok", "record", 12.5, 0.08)
	p.Run("triage/cycle", "skipped", "record", 0, 0)
	if !strings.Contains(bodies[0], `flotilla_run{autonomy="record",environment="operations",routine="triage/cycle",service="flotillad",status="ok",version="sha-abc1234"} 1`) {
		t.Fatalf("run sample: %s", bodies[0])
	}
	if !strings.Contains(bodies[0], `flotilla_spend_day_usd{environment="operations",routine="triage/cycle",service="flotillad"} 0.08`) {
		t.Fatalf("spend sample: %s", bodies[0])
	}
	if strings.Contains(bodies[1], "flotilla_spend_day_usd") || strings.Contains(bodies[1], "flotilla_run_seconds") {
		t.Fatalf("a skipped run must not overwrite the day's spend with 0: %s", bodies[1])
	}
	p.StoreError("case_spend")
	if !strings.Contains(bodies[2], `flotilla_store_error{environment="operations",op="case_spend",service="flotillad"} 1`) {
		t.Fatalf("store error sample: %s", bodies[2])
	}
	p.StorePools(3, 7, 0.25, 1.5)
	for _, want := range []string{`flotilla_store_wait_seconds_total{environment="operations",pool="read",service="flotillad"} 0.25`,
		`flotilla_store_wait_seconds_total{environment="operations",pool="write",service="flotillad"} 1.5`,
		`flotilla_store_waits_total{environment="operations",pool="write",service="flotillad"} 7`} {
		if !strings.Contains(bodies[3], want) {
			t.Fatalf("pool sample lacks %s: %s", want, bodies[3])
		}
	}
}

// An open-cases series that empties is pushed at 0, not dropped; the moves are pushed as read.
func TestCasesPushesAnEmptiedSeriesAtZero(t *testing.T) {
	var bodies []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
	}))
	defer ts.Close()
	p := &Pusher{URL: ts.URL, Env: "operations", Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	waiting := Series{Labels: map[string]string{"fleet": "ops", "kind": "failed_backup", "status": "awaiting_approval"}, V: 1}
	opened := Series{Labels: map[string]string{"fleet": "ops", "kind": "failed_backup", "to": "detected"}, V: 3}
	p.Cases([]Series{waiting}, []Series{opened})
	p.Cases(nil, []Series{opened})
	open := `flotilla_cases_open{environment="operations",fleet="ops",kind="failed_backup",service="flotillad",status="awaiting_approval"}`
	moved := `flotilla_case_moves_total{environment="operations",fleet="ops",kind="failed_backup",service="flotillad",to="detected"} 3`
	if len(bodies) != 2 || !strings.Contains(bodies[0], open+" 1") || !strings.Contains(bodies[0], moved) {
		t.Fatalf("first push: %q", bodies)
	}
	if !strings.Contains(bodies[1], open+" 0") || !strings.Contains(bodies[1], moved) {
		t.Fatalf("second push, the case no longer waiting: %q", bodies[1])
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

// A shadow run's samples are on their own series: the shadow's key, shadow="1", routine_version, and never a freshness or
// last-success sample; the live run's lines are unchanged.
func TestShadowRunHasItsOwnSeries(t *testing.T) {
	var bodies []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
	}))
	defer ts.Close()
	p := &Pusher{URL: ts.URL, Env: "operations", Version: "sha-abc1234", Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	p.Run("triage/cycle", "ok", "record", 12.5, 0.08)
	p.ShadowRun("triage/cycle@v2", 2, "error", "record", 30, 0.5)
	p.ShadowRun("triage/cycle@v2", 2, "skipped", "record", 0, 0)
	live := `flotilla_run{autonomy="record",environment="operations",routine="triage/cycle",service="flotillad",status="ok",version="sha-abc1234"} 1`
	if !strings.Contains(bodies[0], live) {
		t.Fatalf("the live run's line is as it was: %s", bodies[0])
	}
	if strings.Contains(bodies[0], "shadow") {
		t.Fatalf("a live run's samples carry no shadow label: %s", bodies[0])
	}
	for _, want := range []string{
		`flotilla_run{autonomy="record",environment="operations",routine="triage/cycle@v2",routine_version="2",service="flotillad",shadow="1",status="error",version="sha-abc1234"} 1`,
		`flotilla_run_seconds{autonomy="record",environment="operations",routine="triage/cycle@v2",routine_version="2",service="flotillad",shadow="1"} 30`,
		`flotilla_spend_day_usd{environment="operations",routine="triage/cycle@v2",routine_version="2",service="flotillad",shadow="1"} 0.5`,
	} {
		if !strings.Contains(bodies[1], want) {
			t.Fatalf("shadow sample missing %s in:\n%s", want, bodies[1])
		}
	}
	if strings.Contains(bodies[2], "flotilla_run_seconds") || strings.Contains(bodies[2], "spend_day") {
		t.Fatalf("a skipped shadow reports no duration or spend: %s", bodies[2])
	}
	for _, b := range bodies[1:] {
		for _, l := range strings.Split(strings.TrimSpace(b), "\n") {
			if !strings.Contains(l, `shadow="1"`) || !strings.Contains(l, `routine="triage/cycle@v2"`) {
				t.Fatalf("every shadow sample is marked and keyed: %s", l)
			}
		}
	}
}

// The inputs of the stale alert (the freshness and last-success gauges) ignore shadows altogether.
func TestAShadowNeverSetsFreshnessOrLastSuccess(t *testing.T) {
	n := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { n++ }))
	defer ts.Close()
	p := &Pusher{URL: ts.URL, Env: "operations", Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	p.LastSuccess("triage/cycle@v2", time.Now())
	p.Freshness("triage/cycle@v2", time.Hour)
	if n != 0 {
		t.Fatalf("%d pushes for a shadow's freshness or last success, want none", n)
	}
	p.LastSuccess("triage/cycle", time.Now())
	p.Freshness("triage/cycle", time.Hour)
	if n != 2 {
		t.Fatalf("a live routine's still push: %d", n)
	}
}

// The two entry points are not interchangeable: a shadow's key through Run, or a live key through ShadowRun, is dropped and says so.
func TestTheWrongKindOfKeyIsDropped(t *testing.T) {
	n := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { n++ }))
	defer ts.Close()
	var logs strings.Builder
	p := &Pusher{URL: ts.URL, Env: "operations", Log: slog.New(slog.NewTextHandler(&logs, nil))}
	p.Run("triage/cycle@v2", "ok", "record", 1, 1)
	p.ShadowRun("triage/cycle", 1, "ok", "record", 1, 1)
	if n != 0 {
		t.Fatalf("%d pushes", n)
	}
	if strings.Count(logs.String(), "wrong kind of routine key") != 2 {
		t.Fatalf("both are logged: %s", logs.String())
	}
}

func TestShadowLoggerMarksEveryLineAndKeepsTheLevel(t *testing.T) {
	var buf strings.Builder
	base := slog.New(slog.NewJSONHandler(&buf, nil)).With("service", Service, "version", "sha-abc1234")
	l := ShadowLogger(base, 2)
	l.Info("run start", "run_id", "s1")
	l.Warn("run end", "status", "error")
	l.Error("store", "err", "boom")
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("%d lines: %s", len(lines), buf.String())
	}
	for i, level := range []string{"INFO", "WARN", "ERROR"} {
		for _, want := range []string{`"shadow":true`, `"routine_version":2`, `"version":"sha-abc1234"`, `"level":"` + level + `"`} {
			if !strings.Contains(lines[i], want) {
				t.Errorf("line %d lacks %s: %s", i, want, lines[i])
			}
		}
	}
	buf.Reset()
	base.Info("live")
	if strings.Contains(buf.String(), "shadow") {
		t.Fatalf("the live logger is not marked: %s", buf.String())
	}
}
