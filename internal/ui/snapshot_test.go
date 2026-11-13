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
	// v1's shadow chain on one case, and an escalated one (Now lists it)
	k := store.Key{Kind: "target", Value: "failed_backup/host:mini"}
	cid, _, _ := f.st.OpenCase(ctx, store.NewCase{Kind: "failed_backup", Target: "host:mini", Project: "ops", Keys: []store.Key{k}, Canonical: k},
		"ops/backup-monitor", "run ops-backup-monitor-20261113T101500-a1b2c3: backup 20.4h old, above 20h")
	_ = f.st.MoveCase(ctx, cid, "handed_over", "detector", "", "ops/backup-monitor", "a code detector with a known kind")
	_ = f.st.MoveCase(ctx, cid, "planned", "act", "", "ops/rerun-backup",
		"run ops-rerun-backup-20261113T101515-d4e5f6: backup_age_above(20h): holds (age 20.4h); not_running: the host's to check")
	_, _ = f.st.ApplyPlan(ctx, store.PlanRecord{CaseID: cid, RunID: "ops-rerun-backup-20261113T101515-d4e5f6", Action: "backup.rerun_observability",
		Params: map[string]string{"host": "mini"}, PlanHash: "9f2c41d0b7a3e8c15d6f20a4b9e7c3d1", Outcome: "shadow", Autonomy: "shadow", Would: "propose",
		Reasons: []string{"registry: backup.rerun_observability for failed_backup on host:mini", "autonomy: routine shadow, action shadow, host ceiling propose"}},
		"closed", "shadow_plan_recorded", "host:mini", false)
	k2 := store.Key{Kind: "target", Value: "failed_backup/host:dgx-llm-1"}
	esc, _, _ := f.st.OpenCase(ctx, store.NewCase{Kind: "failed_backup", Target: "host:dgx-llm-1", Keys: []store.Key{k2}, Canonical: k2}, "ops/backup-monitor", "")
	_ = f.st.MoveCase(ctx, esc, "handed_over", "detector", "", "ops/backup-monitor", "")
	_ = f.st.MoveCase(ctx, esc, "escalated", "timer", "", "flotillad", "handed over since 2026-11-12T10:15:00Z, longer than 24h0m0s")
	op := f.addOperator()
	ck := f.signin(f.admin)
	ock := f.signin(op)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	pages := map[string]string{"now": "/", "runs": "/runs", "run": "/runs/triage-cycle-20261113100", "fleets": "/fleets",
		"fleet": "/fleets/triage", "autonomy": "/autonomy", "history": "/history", "users": "/users", "signin": "/signin",
		"cases": "/cases?all=1", "case": "/cases/" + cid}
	snap := func(name, p, cookie string) {
		resp, body := f.do("GET", p, cookie, nil, nil)
		if resp.StatusCode != 200 {
			t.Fatalf("%s: %d", p, resp.StatusCode)
		}
		body = strings.NewReplacer(`href="/static/flotilla.css"`, `href="flotilla.css"`, `href="/static/favicon.svg"`, `href="favicon.svg"`).Replace(body)
		if err := os.WriteFile(filepath.Join(dir, name+".html"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for name, p := range pages {
		cookie := ck
		if name == "fleet" {
			cookie = ock
		}
		snap(name, p, cookie)
	}
	// The same installation frozen, with a routine set above shadow so the pages show both.
	_ = f.st.SetControl(ctx, "triage/cycle", "autonomy", "propose", "user:ada", "api")
	_ = f.st.SetControl(ctx, "*", "frozen", "1", "user:otto", "ui")
	snap("now-frozen", "/", ock)
	snap("fleet-frozen", "/fleets/triage", ock)
	snap("autonomy-frozen", "/autonomy", ck)
	if err := os.MkdirAll(filepath.Join(dir, "fonts"), 0o755); err != nil {
		t.Fatal(err)
	}
	fonts, _ := Static.ReadDir("static/fonts")
	for _, e := range fonts {
		b, _ := Static.ReadFile("static/fonts/" + e.Name())
		if err := os.WriteFile(filepath.Join(dir, "fonts", e.Name()), b, 0o644); err != nil {
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
