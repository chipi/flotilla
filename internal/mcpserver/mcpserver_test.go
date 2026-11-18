package mcpserver

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/chipi/flotilla/internal/api"
	"github.com/chipi/flotilla/internal/registry"
	"github.com/chipi/flotilla/internal/sched"
	"github.com/chipi/flotilla/internal/spec"
	"github.com/chipi/flotilla/internal/store"
	"github.com/chipi/flotilla/internal/worker"
)

const agentTok, viewerTok = "flt_agent-for-tests", "flt_viewer-for-tests"

type okRunner struct{}

func (okRunner) Run(context.Context, worker.Request) (*worker.Result, error) {
	return &worker.Result{Status: "ok"}, nil
}

func world(t *testing.T) (*api.Server, *httptest.Server) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	j := &spec.Routine{Fleet: "ops", Routine: "backup-monitor", Description: "d", Enabled: true, Worker: "ops-read", Autonomy: "record",
		Run: spec.Run{Command: "true"}, Every: 10 * time.Minute, Limit: time.Minute, Fresh: 25 * time.Minute}
	s := &sched.Scheduler{Store: st, Routines: []*spec.Routine{j}, Workers: map[string]sched.Runner{"ops-read": okRunner{}}}
	reg, err := registry.Load(filepath.Join("..", "..", "host", "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	a := &api.Server{Sched: s, Store: st, Workers: map[string]*worker.Client{}, Version: "test", Registry: reg,
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Callers: []api.Caller{{Name: "claude-worker-host", Role: api.RoleAgent, TokenSHA256: api.HashToken(agentTok)}}}
	if err := st.AddUser(context.Background(), "vic", store.UserViewer, api.HashToken(viewerTok), "operator", "test", false); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer((&Server{API: a, Version: "test"}).Handler())
	t.Cleanup(ts.Close)
	return a, ts
}

type bearer struct{ tok string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.tok)
	return http.DefaultTransport.RoundTrip(r)
}

func session(t *testing.T, url, tok string) *mcp.ClientSession {
	t.Helper()
	c := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	cs, err := c.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: url,
		HTTPClient: &http.Client{Transport: bearer{tok}}, MaxRetries: -1, DisableStandaloneSSE: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func call(t *testing.T, cs *mcp.ClientSession, name string, args any) (map[string]any, bool) {
	t.Helper()
	r, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	var out map[string]any
	if b, _ := json.Marshal(r.StructuredContent); len(b) > 0 {
		_ = json.Unmarshal(b, &out)
	}
	text := ""
	for _, c := range r.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			text += tc.Text
		}
	}
	if out == nil {
		out = map[string]any{"text": text}
	}
	return out, r.IsError
}

// The tools are the API's own, read and tightening ones; none approves or loosens (T3).
func TestToolList(t *testing.T) {
	names := ToolNames()
	for _, want := range []string{"status", "runs_list", "run_show", "cases_list", "case_show", "approvals_list", "hosts_list",
		"models_calls", "brief_show", "ledger_query", "routine_run", "routine_dry_run", "routine_pause", "fleet_stop", "freeze",
		"hold", "note_propose"} {
		if !slices.Contains(names, want) {
			t.Errorf("no tool %s", want)
		}
	}
	for _, n := range names {
		for _, bad := range Never {
			if strings.Contains(n, bad) {
				t.Errorf("tool %s offers %s", n, bad)
			}
		}
	}
	_, ts := world(t)
	tools, err := session(t, ts.URL, agentTok).ListTools(context.Background(), nil)
	if err != nil || len(tools.Tools) != len(names) {
		t.Fatalf("over the wire: %d tools, %v (want %d)", len(tools.Tools), err, len(names))
	}
}

// Without a known bearer token nothing is served.
func TestAuth(t *testing.T) {
	_, ts := world(t)
	for _, h := range []string{"", "Bearer nope", "Basic x"} {
		req, _ := http.NewRequest("POST", ts.URL, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		req.Header.Set("Content-Type", "application/json")
		if h != "" {
			req.Header.Set("Authorization", h)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 401 {
			t.Errorf("%q: %d", h, resp.StatusCode)
		}
	}
}

// An agent reads, dry-runs and runs; every call is an event under its name; the API's rules still apply.
func TestAgentCalls(t *testing.T) {
	a, ts := world(t)
	cs := session(t, ts.URL, agentTok)
	st, isErr := call(t, cs, "status", map[string]any{})
	if isErr || len(st["routines"].([]any)) != 1 {
		t.Fatalf("status: %v", st)
	}
	dr, isErr := call(t, cs, "routine_dry_run", map[string]any{"routine": "ops/backup-monitor"})
	if isErr || dr["would_start"] != true || dr["autonomy"] != "record" || dr["worker"] != "ops-read" {
		t.Fatalf("dry run: %v", dr)
	}
	if runs, _, _ := a.Store.RunsPage(context.Background(), store.RunQuery{Limit: 10}); len(runs) != 0 {
		t.Fatal("a dry run started a run")
	}
	if out, isErr := call(t, cs, "routine_run", map[string]any{"routine": "ops/backup-monitor"}); isErr {
		t.Fatalf("run: %v", out)
	}
	if out, isErr := call(t, cs, "routine_pause", map[string]any{"routine": "ops/backup-monitor"}); isErr {
		t.Fatalf("pause: %v", out)
	}
	dr, _ = call(t, cs, "routine_dry_run", map[string]any{"routine": "ops/backup-monitor"})
	if dr["would_start"] != false || !strings.Contains(dr["why"].(string), "paused") {
		t.Fatalf("a paused routine's dry run: %v", dr)
	}
	if out, isErr := call(t, cs, "ledger_query", map[string]any{}); isErr {
		t.Fatalf("an agent reads the ledger (RFC-0008 Q3): %v", out)
	}
	if out, isErr := call(t, cs, "routine_run", map[string]any{"routine": "ops/none"}); !isErr {
		t.Fatalf("an unknown routine: %v", out)
	}
	evs, _ := a.Store.Events(context.Background(), 100)
	seen := map[string]bool{}
	for _, e := range evs {
		if e.Via == "mcp" && e.Actor == "claude-worker-host" {
			seen[e.Kind] = true
		}
	}
	for _, k := range []string{"mcp.status", "mcp.routine_dry_run", "run.dry_run", "run.requested", "mcp.routine_pause"} {
		if !seen[k] {
			t.Errorf("no %s event by claude-worker-host via mcp (%v)", k, seen)
		}
	}
}

// A viewer's token reads, and the API refuses its actions: MCP adds no rule and bypasses none.
func TestViewerRefused(t *testing.T) {
	_, ts := world(t)
	cs := session(t, ts.URL, viewerTok)
	if _, isErr := call(t, cs, "status", map[string]any{}); isErr {
		t.Fatal("a viewer cannot read status")
	}
	for name, args := range map[string]any{"routine_run": map[string]any{"routine": "ops/backup-monitor"},
		"freeze": map[string]any{"reason": "x"}, "note_propose": map[string]any{"case": "case-x", "text": "t"}} {
		if out, isErr := call(t, cs, name, args); !isErr {
			t.Errorf("a viewer's %s was allowed: %v", name, out)
		}
	}
}

// At most PerMinute calls a minute per caller.
func TestRateLimit(t *testing.T) {
	_, ts := world(t)
	limited := false
	for i := 0; i < PerMinute+5 && !limited; i++ {
		req, _ := http.NewRequest("POST", ts.URL, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
		req.Header.Set("Authorization", "Bearer "+agentTok)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		limited = resp.StatusCode == 429
	}
	if !limited {
		t.Fatalf("no 429 within %d calls", PerMinute+5)
	}
}
