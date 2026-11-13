package ui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chipi/flotilla/internal/store"
)

// TestSnapshots writes every page, rendered from a seeded store, as static HTML for a browser check
// at phone and laptop widths (DESIGN.md: no sideways scroll, 44px targets). Off unless
// UI_SNAPSHOT_DIR is set:
//
//	UI_SNAPSHOT_DIR=/src/.e2e/ui-snap scripts/go.sh test ./internal/ui/ -run TestSnapshots
func TestSnapshots(t *testing.T) {
	dir := os.Getenv("UI_SNAPSHOT_DIR")
	if dir == "" {
		t.Skip("UI_SNAPSHOT_DIR not set")
	}
	f := setup(t)
	ctx := context.Background()
	for i, st := range []string{"ok", "error", "ok", "timeout", "ok", "running"} {
		end := "2026-11-13T10:0" + string(rune('1'+i)) + ":30Z"
		d := 12.5 + float64(i)
		_ = f.st.StartRun(ctx, store.Run{ID: "triage-cycle-2026111310" + string(rune('0'+i)), Routine: "triage/cycle", SpecHash: "cad5f7e194e0aa",
			Trigger: "schedule", Actor: "flotillad", Autonomy: "propose", StartedAt: "2026-11-13T10:0" + string(rune('0'+i)) + ":00Z",
			Status: "running", Detail: "a long detail line from the run that wraps " + strings.Repeat("and keeps going ", 12)})
		if st != "running" {
			_ = f.st.FinishRun(ctx, "triage-cycle-2026111310"+string(rune('0'+i)), st, nil, 0.0021, d, "detail: "+st+" "+strings.Repeat("x", 300))
			_ = end
		}
	}
	for i := 0; i < 6; i++ {
		_, _ = f.st.AppendEntry(ctx, "triage/cycle", "decision", map[string]string{
			"fingerprint": "glitchtip:PODCAST-PIPELINE-DEV-" + string(rune('A'+i)), "occurrence_id": "o" + string(rune('A'+i)),
			"disposition": []string{"file", "dismiss", "escalate"}[i%3], "ts": "2026-11-13T09:5" + string(rune('0'+i)) + ":00Z",
			"alertname": "TypeError in the timeline scrubber (a long alert name that goes on)",
			"reason":    "The diagnosis is unsure between two causes; " + strings.Repeat("the evidence says more ", 6)}, "")
		_, _ = f.st.AppendEntry(ctx, "triage/cycle", "recurrence", map[string]string{"fingerprint": "glitchtip:PODCAST-PIPELINE-DEV-" + string(rune('A'+i))}, "")
	}
	_ = f.st.SetControl(ctx, "triage/cycle", "paused", "1", "user:ada", "api")
	_ = time.Now
	op := f.addOperator()
	ck := f.signin(f.admin)
	ock := f.signin(op)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	pages := map[string]string{"now": "/", "runs": "/runs", "run": "/runs/triage-cycle-20261113100", "fleets": "/fleets",
		"fleet": "/fleets/triage", "autonomy": "/autonomy", "history": "/history", "users": "/users", "signin": "/signin"}
	for name, p := range pages {
		cookie := ck
		if name == "fleet" {
			cookie = ock
		}
		resp, body := f.do("GET", p, cookie, nil, nil)
		if resp.StatusCode != 200 {
			t.Fatalf("%s: %d", p, resp.StatusCode)
		}
		body = strings.NewReplacer(`href="/static/flotilla.css"`, `href="flotilla.css"`, `href="/static/favicon.svg"`, `href="favicon.svg"`).Replace(body)
		if err := os.WriteFile(filepath.Join(dir, name+".html"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ico, _ := Static.ReadFile("static/favicon.svg")
	_ = os.WriteFile(filepath.Join(dir, "favicon.svg"), ico, 0o644)
	css, _ := Static.ReadFile("static/flotilla.css")
	if err := os.WriteFile(filepath.Join(dir, "flotilla.css"), css, 0o644); err != nil {
		t.Fatal(err)
	}
}
