package ui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chipi/flotilla/internal/cases"
	"github.com/chipi/flotilla/internal/spec"
	"github.com/chipi/flotilla/internal/store"
)

// TestSnapshots writes every page, rendered from a seeded store with the repo's own fleets, as static
// HTML for a browser check against mockup v3 at phone and laptop widths (DESIGN.md: no sideways
// scroll, 44px targets). Off unless UI_SNAPSHOT_DIR is set:
//
//	UI_SNAPSHOT_DIR=/src/.e2e/ui-snap scripts/go.sh test ./internal/ui/ -run TestSnapshots
func TestSnapshots(t *testing.T) {
	dir := os.Getenv("UI_SNAPSHOT_DIR")
	if dir == "" {
		t.Skip("UI_SNAPSHOT_DIR not set")
	}
	routines, err := spec.Load(filepath.Join("..", "..", "fleets"))
	if err != nil {
		t.Fatal(err)
	}
	f := setupWith(t, routines)
	ctx := context.Background()
	st := f.st
	now := time.Now().UTC()
	at := func(d time.Duration) string { return now.Add(-d).Format(store.TimeFormat) }

	// Runs over 14 days: a few a day per routine, with failures, a timeout, a skipped and a running run.
	n := 0
	for day := 13; day >= 0; day-- {
		for _, r := range routines {
			id := r.Fleet + "/" + r.Routine
			per := 3 + (day+len(r.Routine))%4
			for k := 0; k < per; k++ {
				n++
				started := now.Add(-time.Duration(day)*24*time.Hour - time.Duration(k*3+1)*time.Hour)
				runID := fmt.Sprintf("%s-%s-%04d", r.Fleet, r.Routine, n)
				_ = st.StartRun(ctx, store.Run{ID: runID, Routine: id, SpecHash: "cad5f7e194e0aa", Trigger: "schedule", Actor: "flotillad",
					Autonomy: r.Autonomy, StartedAt: started.Format(store.TimeFormat), Status: "running"})
				status := store.StatusOK
				switch {
				case (n % 17) == 0:
					status = store.StatusError
				case (n % 41) == 0:
					status = store.StatusTimeout
				}
				spend := 0.0
				if r.Fleet == "triage" {
					spend = 0.004 + float64(n%5)*0.001
				}
				_ = st.FinishRun(ctx, runID, status, nil, spend, 1.2+float64(n%50), "run report line one\nchecked 3 sources\nresult: "+status)
			}
		}
	}
	_ = st.StartRun(ctx, store.Run{ID: "triage-cycle-running", Routine: "triage/cycle", SpecHash: "cad5f7e194e0aa", Trigger: "schedule",
		Actor: "flotillad", Autonomy: "shadow", StartedAt: at(time.Minute), Status: "running"})

	// The triage ledger, for History's ledger type.
	for i := 0; i < 6; i++ {
		_, _ = st.AppendEntry(ctx, "triage/cycle", "decision", map[string]string{
			"fingerprint": "glitchtip:PODCAST-PIPELINE-DEV-" + string(rune('A'+i)), "occurrence_id": "o" + string(rune('A'+i)),
			"disposition": []string{"file", "dismiss", "escalate"}[i%3], "ts": at(time.Duration(i+1) * time.Hour),
			"alertname": "TypeError in the timeline scrubber (a long alert name that goes on)",
			"reason":    "The diagnosis is unsure between two causes; " + strings.Repeat("the evidence says more ", 6)}, "")
	}

	// Cases: one waiting for approval with its plan, one escalated, one verified, one shadow plan.
	open := func(kind, target, detail string) string {
		k := store.Key{Kind: "target", Value: kind + "/" + target}
		id, _, err := st.OpenCase(ctx, store.NewCase{Kind: kind, Target: target, Project: "ops", Keys: []store.Key{k}, Canonical: k}, "ops/backup-monitor", detail)
		if err != nil {
			t.Fatal(err)
		}
		_ = st.MoveCase(ctx, id, cases.HandedOver, cases.ByDetector, "", "ops/backup-monitor", "a code detector knows the kind: no diagnosis step")
		return id
	}
	plan := func(id, target, run, outcome, autonomy, would, to, resolution string, lock bool) {
		_ = st.StartRun(ctx, store.Run{ID: run, Routine: "ops/rerun-backup", SpecHash: "9a1c", Trigger: "case", Actor: "flotillad",
			Autonomy: "propose", StartedAt: at(50 * time.Minute), Status: "running"})
		_ = st.FinishRun(ctx, run, store.StatusOK, nil, 0, 31, "rechecked: the backup is 27h old and not running\nplan: backup.rerun_observability")
		_ = st.MoveCase(ctx, id, cases.Planned, cases.ByAct, "", "ops/rerun-backup",
			"run "+run+": backup_age_above(26h): holds (age 27.1h); not_running: holds")
		if _, err := st.ApplyPlan(ctx, store.PlanRecord{CaseID: id, RunID: run, Action: "backup.rerun_observability",
			Params: map[string]string{"host": strings.TrimPrefix(target, "host:")}, PlanHash: "4f1c9a20b7d3e8c15d6f20a4b9e7c3d19e07",
			Outcome: outcome, Autonomy: autonomy, Would: would,
			Reasons: []string{"registry: backup.rerun_observability for failed_backup on " + target,
				"autonomy: routine propose, action propose, host ceiling propose", "limits: 0 of 3 on this host this hour"}},
			to, resolution, target, lock); err != nil {
			t.Fatal(err)
		}
	}
	waitID := open("failed_backup", "host:mini", "run ops-backup-monitor-0001: the newest offsite backup is 27 hours old (limit 26h)")
	plan(waitID, "host:mini", "ops-rerun-backup-0001", "propose", "propose", "", cases.AwaitingApproval, "", true)
	escID := open("failed_backup", "host:dgx", "run ops-backup-monitor-0002: the newest offsite backup is 31 hours old (limit 26h)")
	_ = st.MoveCase(ctx, escID, cases.Escalated, cases.ByTimer, "", "flotillad", "handed over for longer than 24h0m0s: no routine planned it")
	okID := open("failed_backup", "host:vps", "run ops-backup-monitor-0003: the newest offsite backup is 26.4 hours old (limit 26h)")
	plan(okID, "host:vps", "ops-rerun-backup-0002", "propose", "propose", "", cases.AwaitingApproval, "", true)
	_ = st.MoveCase(ctx, okID, cases.Executing, cases.ByOperator, "", "user:ada", "approved backup.rerun_observability on host:vps (plan 4f1c9a20b7d3)")
	_ = st.MoveCase(ctx, okID, cases.Checking, cases.ByAct, "", "flotillad", "the host ran it: exit 0")
	_ = st.MoveCase(ctx, okID, cases.Closed, cases.ByCheck, cases.Verified, "ops/verify-backup", "run ops-verify-backup-0001: a new backup, 3 minutes old")
	shadowID := open("failed_backup", "host:nas", "run ops-backup-monitor-0004: the newest offsite backup is 26.1 hours old (limit 26h)")
	plan(shadowID, "host:nas", "ops-rerun-backup-0003", "shadow", "shadow", "propose", cases.Closed, cases.ShadowPlan, false)

	// Hosts: mini and dgx connected, vps down for 12 minutes; executions on vps.
	for _, h := range []string{"mini", "dgx", "vps"} {
		if _, err := st.AddEnrolment(ctx, h, "tok-"+h, "user:ada", "test"); err != nil {
			t.Fatal(err)
		}
		if err := st.Enrol(ctx, h, "tok-"+h, func() (string, time.Time, error) {
			return "cert-sha-" + h + "-0123456789abcdef", now.Add(27 * 24 * time.Hour), nil
		}); err != nil {
			t.Fatal(err)
		}
		c := store.HostContact{Host: h, Connected: h != "vps", Version: "0.1.0", ConnectedAt: now.Add(-48 * time.Hour), LastContact: now.Add(-4 * time.Second),
			ClockOffset: 200 * time.Millisecond, ChainHead: "9b1e5d2a"}
		if h == "vps" {
			c.LastContact = now.Add(-12 * time.Minute)
		}
		_ = st.RecordHostContact(ctx, c)
	}
	_ = st.StartHostExecution(ctx, store.HostExecution{ID: "exe-8pz4", CaseID: okID, PlanID: 2, Host: "vps", Action: "backup.rerun_observability", Approved: "user:ada", Envelope: []byte("{}")})
	_ = st.EndHostExecution(ctx, "exe-8pz4", "done", 0, "ab", "exit 0 in 2.1 s", "9b1e5d2a")

	// An autonomy change, for Autonomy's Recent changes.
	_ = st.SetControlBecause(ctx, "action:backup.rerun_observability", "autonomy", "propose", "user:ada", "ui", "5 shadow plans I agreed with")
	_ = st.SetControl(ctx, "ops/backup-monitor", "paused", "0", "user:ada", "api")

	op := f.addOperator()
	ck := f.signin(f.admin)
	ock := f.signin(op)
	vck := f.signin(f.vic)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	pages := map[string]string{
		"now": "/", "approvals": "/approvals", "cases": "/cases", "cases-pivot": "/cases?p=kind%3Afailed_backup&p=target%3Ahost%3Adgx",
		"case": "/cases/" + waitID, "case-escalated": "/cases/" + escID, "case-verified": "/cases/" + okID,
		"runs": "/runs", "runs-problems": "/runs?f=problems", "run": "/runs/ops-rerun-backup-0001",
		"fleets": "/fleets", "fleet": "/fleets/ops", "fleet-triage": "/fleets/triage", "fleet-routines": "/fleets/ops?tab=routines",
		"fleet-stats": "/fleets/ops?tab=stats", "fleet-logs": "/fleets/triage?tab=logs",
		"hosts": "/hosts", "host": "/hosts/vps", "host-agent": "/hosts/mini?tab=agent", "host-exec": "/hosts/vps?tab=exec",
		"autonomy": "/autonomy", "history": "/history?period=all", "history-ledger": "/history?type=ledger&period=all",
		"account": "/account", "users": "/users", "models": "/models", "signin": "/signin"}
	snap := func(name, p, cookie string) {
		resp, body := f.do("GET", p, cookie, nil, nil)
		if resp.StatusCode != 200 {
			t.Fatalf("%s: %d %s", p, resp.StatusCode, body)
		}
		body = strings.NewReplacer(`href="/static/flotilla.css"`, `href="flotilla.css"`, `href="/static/favicon.svg"`, `href="favicon.svg"`,
			`src="/static/flotilla.js"`, `src="flotilla.js"`).Replace(body)
		if err := os.WriteFile(filepath.Join(dir, name+".html"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for name, p := range pages {
		cookie := ock
		if name == "users" || name == "account" {
			cookie = ck
		}
		snap(name, p, cookie)
	}
	snap("now-viewer", "/", vck)
	_ = st.SetControl(ctx, "*", "frozen", "1", "user:otto", "ui")
	snap("now-frozen", "/", ock)
	snap("autonomy-frozen", "/autonomy", ock)
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
	for _, name := range []string{"favicon.svg", "flotilla.css", "flotilla.js"} {
		b, _ := Static.ReadFile("static/" + name)
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
