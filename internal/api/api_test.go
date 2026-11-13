package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"log/slog"

	"github.com/chipi/flotilla/internal/sched"
	"github.com/chipi/flotilla/internal/spec"
	"github.com/chipi/flotilla/internal/store"
	"github.com/chipi/flotilla/internal/worker"
)

type okRunner struct{}

func (okRunner) Run(ctx context.Context, r worker.Request) (*worker.Result, error) {
	z := 0
	return &worker.Result{Status: "ok", ExitCode: &z}, nil
}

const (
	opTok     = "operator-token-0123456789abcdef0123456789"
	agentTok  = "agent-token-0123456789abcdef0123456789abc"
	workerTok = "worker-token-0123456789abcdef0123456789ab"
)

func server(t *testing.T) (*httptest.Server, *store.Store) {
	t.Helper()
	return serverWith(t, func(*spec.Routine) {})
}

func serverWith(t *testing.T, mod func(*spec.Routine)) (*httptest.Server, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "a.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	j := &spec.Routine{Fleet: "triage", Routine: "cycle", Description: "d", Enabled: true, Worker: "triage", Autonomy: "shadow",
		Run: spec.Run{Command: "true"}, Every: 10 * time.Minute, Limit: time.Minute, Fresh: 35 * time.Minute}
	mod(j)
	s := &sched.Scheduler{Store: st, Routines: []*spec.Routine{j}, Workers: map[string]sched.Runner{"triage": okRunner{}}}
	srv := &Server{Sched: s, Store: st, Workers: map[string]*worker.Client{}, Version: "test",
		Log: slog.New(slog.NewTextHandler(os.Stderr, nil)),
		Callers: append([]Caller{{Name: "operator", Role: RoleOperator, TokenSHA256: HashToken(opTok)}, {Name: "claude-homelab", Role: RoleAgent, TokenSHA256: HashToken(agentTok)}},
			WorkerCallers(map[string]string{"triage": workerTok})...)}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, st
}

func call(t *testing.T, ts *httptest.Server, method, path, tok, body string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestAuth(t *testing.T) {
	ts, _ := server(t)
	if c, _ := call(t, ts, "GET", "/healthz", "", ""); c != 200 {
		t.Fatalf("healthz needs no token: %d", c)
	}
	if c, _ := call(t, ts, "GET", "/v1/status", "", ""); c != 401 {
		t.Fatalf("no token: %d", c)
	}
	if c, _ := call(t, ts, "GET", "/v1/status", "wrong-token", ""); c != 401 {
		t.Fatalf("unknown token: %d", c)
	}
	if c, _ := call(t, ts, "GET", "/v1/status", agentTok, ""); c != 200 {
		t.Fatalf("agent may read: %d", c)
	}
}

// AGENTS.md rule 2: an agent is never the operator.
func TestAgentCannotDoOperatorThings(t *testing.T) {
	ts, st := server(t)
	for _, p := range []struct{ path, body string }{
		{"/v1/routines/triage/cycle/autonomy", `{"autonomy":"auto"}`},
		{"/v1/routines/triage/cycle/resume", ""},
		{"/v1/fleets/triage/start", ""},
	} {
		if c, _ := call(t, ts, "POST", p.path, agentTok, p.body); c != 403 {
			t.Fatalf("%s by an agent: %d, want 403", p.path, c)
		}
	}
	if v, _ := st.Control(context.Background(), "triage/cycle", sched.KeyAutonomy); v != "" {
		t.Fatalf("a refused call changed the autonomy to %q", v)
	}
}

func TestAgentCanMakeThingsSafer(t *testing.T) {
	ts, st := server(t)
	if c, _ := call(t, ts, "POST", "/v1/fleets/triage/stop", agentTok, ""); c != 200 {
		t.Fatalf("stop: %d", c)
	}
	if c, _ := call(t, ts, "POST", "/v1/routines/triage/cycle/pause", agentTok, ""); c != 200 {
		t.Fatalf("pause: %d", c)
	}
	ev, _ := st.Events(context.Background(), 10)
	if len(ev) < 2 || ev[0].Actor != "claude-homelab" {
		t.Fatalf("the event log names the agent: %+v", ev)
	}
}

func TestOperatorStage(t *testing.T) {
	ts, st := server(t)
	if c, _ := call(t, ts, "POST", "/v1/routines/triage/cycle/autonomy", opTok, `{"autonomy":"yolo"}`); c != 400 {
		t.Fatalf("bad autonomy: %d", c)
	}
	if c, _ := call(t, ts, "POST", "/v1/routines/triage/cycle/autonomy", opTok, `{"autonomy":"propose"}`); c != 400 {
		t.Fatalf("raising autonomy without a reason (ADR-0014): %d, want 400", c)
	}
	if c, _ := call(t, ts, "POST", "/v1/routines/triage/cycle/autonomy", opTok, `{"autonomy":"propose","reason":"test"}`); c != 200 {
		t.Fatalf("autonomy: %d", c)
	}
	if v, _ := st.Control(context.Background(), "triage/cycle", sched.KeyAutonomy); v != "propose" {
		t.Fatalf("autonomy now %q", v)
	}
	_, out := call(t, ts, "GET", "/v1/status", opTok, "")
	routine := out["routines"].([]any)[0].(map[string]any)
	if routine["autonomy"] != "propose" || routine["spec_autonomy"] != "shadow" {
		t.Fatalf("status shows the effective and the spec autonomy: %v", routine)
	}
}

func TestStageCappedBySpec(t *testing.T) {
	ts, st := serverWith(t, func(j *spec.Routine) { j.MaxAutonomy = spec.AutonomyShadow })
	for _, s := range []string{"propose", "auto"} {
		c, out := call(t, ts, "POST", "/v1/routines/triage/cycle/autonomy", opTok, `{"autonomy":"`+s+`"}`)
		if c != 400 || !strings.Contains(fmt.Sprint(out["error"]), "capped at shadow") {
			t.Fatalf("%s on a shadow-capped routine: %d %v", s, c, out)
		}
	}
	if v, _ := st.Control(context.Background(), "triage/cycle", sched.KeyAutonomy); v != "" {
		t.Fatalf("a refused autonomy left a control: %q", v)
	}
	if c, _ := call(t, ts, "POST", "/v1/routines/triage/cycle/autonomy", opTok, `{"autonomy":"shadow"}`); c != 200 {
		t.Fatalf("shadow on a shadow-capped routine: %d", c)
	}
}

func TestUnknownSubjects(t *testing.T) {
	ts, _ := server(t)
	if c, _ := call(t, ts, "POST", "/v1/routines/nope/x/pause", opTok, ""); c != 404 {
		t.Fatalf("unknown routine: %d", c)
	}
	if c, _ := call(t, ts, "POST", "/v1/fleets/nope/stop", opTok, ""); c != 404 {
		t.Fatalf("unknown fleet: %d", c)
	}
}

func TestManualRun(t *testing.T) {
	ts, st := server(t)
	if c, _ := call(t, ts, "POST", "/v1/routines/triage/cycle/run", agentTok, ""); c != 202 {
		t.Fatalf("run: %d", c)
	}
	for i := 0; i < 200; i++ {
		if runs, _ := st.Runs(context.Background(), "triage/cycle", 1); len(runs) == 1 && runs[0].Status == "ok" {
			if runs[0].Actor != "claude-homelab" || runs[0].Trigger != "manual" {
				t.Fatalf("recorded %+v", runs[0])
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("manual run never finished")
}

func TestLoadCallers(t *testing.T) {
	d := t.TempDir()
	bad := map[string]string{
		"role":      "callers:\n  - {name: a, role: admin, token_sha256: " + HashToken("x") + "}\n",
		"hash":      "callers:\n  - {name: a, role: agent, token_sha256: abc}\n",
		"duplicate": "callers:\n  - {name: a, role: agent, token_sha256: " + HashToken("x") + "}\n  - {name: a, role: agent, token_sha256: " + HashToken("y") + "}\n",
		"empty":     "callers: []\n",
		"unknown":   "callers:\n  - {name: a, role: agent, token: plain, token_sha256: " + HashToken("x") + "}\n",
	}
	for name, body := range bad {
		p := filepath.Join(d, name+".yaml")
		os.WriteFile(p, []byte(body), 0o600)
		if _, err := LoadCallers(p); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
	p := filepath.Join(d, "good.yaml")
	os.WriteFile(p, []byte("callers:\n  - {name: operator, role: operator, token_sha256: "+HashToken("x")+"}\n"), 0o600)
	if c, err := LoadCallers(p); err != nil || c[0].Name != "operator" {
		t.Fatalf("good: %v %v", c, err)
	}
}
