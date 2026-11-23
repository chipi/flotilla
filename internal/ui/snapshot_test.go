package ui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chipi/flotilla/internal/api"
	"github.com/chipi/flotilla/internal/cases"
	"github.com/chipi/flotilla/internal/hook"
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
		Actor: "flotillad", Autonomy: "record", StartedAt: at(time.Minute), Status: "running"})

	// The installation's models and the triage runs' model calls (G1), for Runs, Now and Models.
	f.sch.ModelsJSON = `{"endpoints":{"gateway":{"base_url":"http://litellm:4000/v1","kind":"litellm","key_env":"K","defaults":{"tools":"native","json":"object","cost":"reported"}},` +
		`"model-host":{"base_url":"http://model-host:8003/v1","kind":"openai","defaults":{"tools":"none","json":"object","cost":"zero"}}},` +
		`"aliases":{"triage-flash":{"endpoint":"gateway","model":"fleet-triage-flash"},"triage-pro":{"endpoint":"gateway","model":"fleet-triage-pro"},` +
		`"local-model-host":{"endpoint":"model-host","model":"NVFP4/Qwen3-30B-A3B"}}}`
	recent, _, _ := st.RunsPage(ctx, store.RunQuery{Limit: 100000, Desc: true})
	var calls []store.ModelCall
	for i, r := range recent {
		if !strings.HasPrefix(r.Routine, "triage/") && r.Routine != "platform/heartbeat" {
			continue
		}
		alias, model, cost := "triage-pro", "deepseek/deepseek-v4-pro", 0.0021+float64(i%7)*0.0004
		if r.Routine == "platform/heartbeat" {
			alias, model, cost = "triage-flash", "deepseek/deepseek-v4-flash", 0.0001
		} else if i%3 == 0 {
			alias, model, cost = "local-model-host", "NVFP4/Qwen3-30B-A3B", 0
		}
		in, out, lat := int64(900+i*13%700), int64(80+i*7%300), 2.0+float64(i%9)
		c := store.ModelCall{RunID: r.ID, Routine: r.Routine, At: r.StartedAt, Alias: alias, Model: model, TokensIn: &in, TokensOut: &out,
			CostUSD: &cost, CostSource: "reported:body", LatencyS: &lat}
		if i%23 == 0 {
			c.Error, c.TokensIn, c.TokensOut, c.CostUSD = "Transient", nil, nil, nil
		}
		calls = append(calls, c)
	}
	if err := st.RecordCalls(ctx, calls); err != nil {
		t.Fatal(err)
	}

	// The triage ledger, for History's ledger type.
	for i := 0; i < 6; i++ {
		_, _ = st.AppendEntry(ctx, "triage/cycle", "decision", map[string]string{
			"fingerprint": "glitchtip:API-PIPELINE-DEV-" + string(rune('A'+i)), "occurrence_id": "o" + string(rune('A'+i)),
			"disposition": []string{"file", "dismiss", "escalate"}[i%3], "ts": at(time.Duration(i+1) * time.Hour),
			"alertname": "TypeError in the timeline scrubber (a long alert name that goes on)",
			"reason":    "The diagnosis is unsure between two causes; " + strings.Repeat("the evidence says more ", 6)}, "")
	}

	// Cases: one waiting for approval with its plan, one escalated, one verified, one recorded plan.
	open := func(kind, target, detail string) string {
		k := store.Key{Kind: "target", Value: kind + "/" + target}
		id, _, err := st.OpenCase(ctx, store.NewCase{Kind: kind, Target: target, Project: "jobs", Keys: []store.Key{k}, Canonical: k}, "jobs/backup-monitor", detail)
		if err != nil {
			t.Fatal(err)
		}
		_ = st.MoveCase(ctx, id, cases.HandedOver, cases.ByDetector, "", "jobs/backup-monitor", "a code detector knows the kind: no diagnosis step")
		return id
	}
	plan := func(id, target, run, outcome, autonomy, would, to, resolution string, lock bool) {
		_ = st.StartRun(ctx, store.Run{ID: run, Routine: "jobs/rerun-backup", SpecHash: "9a1c", Trigger: "case", Actor: "flotillad",
			Autonomy: "propose", StartedAt: at(50 * time.Minute), Status: "running"})
		_ = st.FinishRun(ctx, run, store.StatusOK, nil, 0, 31, "rechecked: the backup is 27h old and not running\nplan: backup.rerun")
		_ = st.MoveCase(ctx, id, cases.Planned, cases.ByAct, "", "jobs/rerun-backup",
			"run "+run+": backup_age_above(26h): holds (age 27.1h); not_running: holds")
		if _, err := st.ApplyPlan(ctx, store.PlanRecord{CaseID: id, RunID: run, Action: "backup.rerun",
			Params: map[string]string{"host": strings.TrimPrefix(target, "host:")}, PlanHash: "4f1c9a20b7d3e8c15d6f20a4b9e7c3d19e07",
			Outcome: outcome, Autonomy: autonomy, Would: would,
			Reasons: []string{"registry: backup.rerun for failed_backup on " + target,
				"autonomy: routine propose, action propose, host ceiling propose", "limits: 0 of 3 on this host this hour"}},
			to, resolution, target, lock); err != nil {
			t.Fatal(err)
		}
	}
	waitID := open("failed_backup", "host:backup-01", "run ops-backup-monitor-0001: the newest offsite backup is 27 hours old (limit 26h)")
	plan(waitID, "host:backup-01", "ops-rerun-backup-0001", "propose", "propose", "", cases.AwaitingApproval, "", true)
	escID := open("failed_backup", "host:model-host", "run ops-backup-monitor-0002: the newest offsite backup is 31 hours old (limit 26h)")
	_ = st.MoveCase(ctx, escID, cases.Escalated, cases.ByTimer, "", "flotillad", "handed over for longer than 24h0m0s: no routine planned it")
	okID := open("failed_backup", "host:vps", "run ops-backup-monitor-0003: the newest offsite backup is 26.4 hours old (limit 26h)")
	plan(okID, "host:vps", "ops-rerun-backup-0002", "propose", "propose", "", cases.AwaitingApproval, "", true)
	_ = st.MoveCase(ctx, okID, cases.Executing, cases.ByOperator, "", "user:ada", "approved backup.rerun on host:vps (plan 4f1c9a20b7d3)")
	_ = st.MoveCase(ctx, okID, cases.Checking, cases.ByAct, "", "flotillad", "the host ran it: exit 0")
	_ = st.MoveCase(ctx, okID, cases.Closed, cases.ByCheck, cases.Verified, "jobs/verify-backup", "run ops-verify-backup-0001: a new backup, 3 minutes old")
	recordedID := open("failed_backup", "host:nas", "run ops-backup-monitor-0004: the newest offsite backup is 26.1 hours old (limit 26h)")
	plan(recordedID, "host:nas", "ops-rerun-backup-0003", "record", "record", "propose", cases.Closed, cases.PlanRecorded, false)

	// An action that earned its next level: five recorded follow-up lists, each judged right.
	for i := 1; i <= 5; i++ {
		run, note := fmt.Sprintf("meetings-notes-000%d", i), fmt.Sprintf("2026-11-1%d-weekly-sync", i)
		k := store.Key{Kind: "hook:meeting", Value: note}
		id, _, err := st.OpenCase(ctx, store.NewCase{Kind: "meeting", Target: "meeting:" + note, Project: "meetings", Keys: []store.Key{k}, Canonical: k,
			Title: "Follow-ups from " + note}, "hook/meeting", "")
		if err != nil {
			t.Fatal(err)
		}
		_ = st.StartRun(ctx, store.Run{ID: run, Routine: "meetings/notes", SpecHash: "81e8", Trigger: "case", Actor: "flotillad",
			Autonomy: "propose", StartedAt: at(time.Duration(30+i) * time.Hour), Status: "running"})
		_ = st.MoveCase(ctx, id, cases.HandedOver, cases.ByDetector, "", "hook/meeting", "")
		_ = st.MoveCase(ctx, id, cases.Planned, cases.ByAct, "", "meetings/notes", "run "+run+": 3 follow-up(s)")
		if _, err := st.ApplyPlan(ctx, store.PlanRecord{CaseID: id, RunID: run, Action: "plumbing.create_tasks", Params: map[string]string{"list": "follow-ups"},
			PlanHash: fmt.Sprintf("7d2e%d", i), Outcome: "record", Autonomy: "record", Would: "propose", Reasons: []string{"autonomy: routine propose, action record"}},
			cases.Closed, cases.PlanRecorded, "", false); err != nil {
			t.Fatal(err)
		}
		_ = st.SetVerdict(ctx, id, "ada", store.VerdictRight)
	}

	// A meeting a hook opened (RFC-0014 case 3): its tasks sent and delivered, verified, handed on to decisions, which wait for you.
	mk := store.Key{Kind: "hook:meeting", Value: "2026-11-23-weekly-sync"}
	meetID, _, err := st.OpenCase(ctx, store.NewCase{Kind: "meeting", Target: "2026-11-23-weekly-sync", Project: "meetings", Keys: []store.Key{mk}, Canonical: mk},
		"hook:meeting", "delivery 1xsY:2026-11-23T07:47:38Z")
	if err != nil {
		t.Fatal(err)
	}
	_ = st.MoveCase(ctx, meetID, cases.HandedOver, cases.ByDetector, "", "hook:meeting", "a hook with a known kind")
	_ = st.MoveCase(ctx, meetID, cases.Planned, cases.ByAct, "", "meetings/notes", "run meetings-notes-0001: 3 follow-ups, 2 decisions")
	_ = st.SetPlanThen(ctx, meetID, "meetings-notes-0001", "meetings/notes", "decisions", "{}")
	_ = st.MoveCase(ctx, meetID, cases.AwaitingApproval, cases.ByPolicy, "", "flotillad", "propose: plumbing.create_tasks")
	_ = st.MoveCase(ctx, meetID, cases.Executing, cases.ByOperator, "", "user:ada", "approved plumbing.create_tasks")
	_ = st.MoveCase(ctx, meetID, cases.Checking, cases.ByAct, "", "plumbing/send-create-tasks", "run plumbing-send-0001: plumbing.create_tasks sent as plan-9e07")
	_ = st.AddCaseEvent(ctx, meetID, "action.ref", "plumbing/send-create-tasks", `Google Tasks list "follow-ups": 3 task(s), e.g. MTIzNDU2Nzg5`)
	if err := st.HandOnVerified(ctx, meetID, "decisions", "", "verified: the plumbing confirmed it", "{}"); err != nil {
		t.Fatal(err)
	}
	_ = st.Event(ctx, "hook:meeting", "hook", "hook.accepted", meetID, "opened; delivery 1xsY")
	_ = st.Event(ctx, "hook:meeting", "hook", "hook.refused", "hook:meeting", "signature: the timestamp is 3600 s old, over 300 s")
	f.ui.API.Hooks = map[string]*hook.Hook{"meeting": {Name: "meeting", Kind: "meeting", Project: "meetings"}}

	// A remote worker refused for its version, one connected (the bugfix fleet's, on another machine).
	for _, h := range []string{"w-bugfix-laptop", "w-bugfix-act-laptop"} {
		if _, err := st.AddEnrolment(ctx, h, "tok-"+h, "user:ada", "test"); err != nil {
			t.Fatal(err)
		}
		if err := st.Enrol(ctx, h, "tok-"+h, func() (string, time.Time, error) { return "cert-" + h, now.Add(27 * 24 * time.Hour), nil }); err != nil {
			t.Fatal(err)
		}
	}
	f.ui.API.Remote = func() []api.RemoteWorker {
		return []api.RemoteWorker{
			{Worker: "bugfix", Identity: "w-bugfix-laptop", Refused: "version sha-52bfbb9, not flotillad's sha-ff08c18"},
			{Worker: "bugfix-act", Identity: "w-bugfix-act-laptop", Version: "sha-ff08c18", Connected: true},
		}
	}

	// Hosts: backup-01 and model-host connected, vps down for 12 minutes; executions on vps.
	for _, h := range []string{"backup-01", "model-host", "vps"} {
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
	_ = st.StartHostExecution(ctx, store.HostExecution{ID: "exe-8pz4", CaseID: okID, PlanID: 2, Host: "vps", Action: "backup.rerun", Approved: "user:ada", Envelope: []byte("{}")})
	_ = st.EndHostExecution(ctx, "exe-8pz4", "done", 0, "ab", "exit 0 in 2.1 s", "9b1e5d2a")

	// An autonomy change, for Autonomy's Recent changes.
	_ = st.SetControlBecause(ctx, "action:backup.rerun", "autonomy", "propose", "user:ada", "ui", "5 recorded plans I agreed with")
	_ = st.SetControl(ctx, "jobs/backup-monitor", "paused", "0", "user:ada", "api")

	// Knowledge: three active notes and five proposed, so the proposed fold shows; the brief, written now.
	for i, n := range []string{"The offsite backup runs at 02:00; an age under 26h at 09:00 is normal.", "vps is rebooted on Sundays; a host agent down then is expected.",
		"A meeting's follow-ups go to the follow-ups list, never the decisions doc.", "The NAS backup is slower on the first of the month (a full run).",
		"triage: a TypeError in the scrubber is the timeline bug, filed once.", "bugfix: orrery's flaky e2e test is known; do not fix it.",
		"seo: the sitemap is regenerated hourly; a missing URL can wait an hour.", "plumbing: the Tasks list is shared with a person; keep titles short."} {
		id, err := st.ProposeNote(ctx, []string{"jobs", "jobs", "meetings", "jobs", "triage", "bugfix", "seo", "plumbing"}[i], n, "", "ada")
		if err != nil {
			t.Fatal(err)
		}
		if i < 3 {
			_ = st.DecideNote(ctx, id, store.NoteActive, "ada")
		}
	}
	if b, err := f.ui.composeBrief(ctx, now); err == nil {
		_ = st.SaveBrief(ctx, b)
	} else {
		t.Fatal(err)
	}

	op := f.addOperator()
	ck := f.signin(f.admin)
	ock := f.signin(op)
	vck := f.signin(f.vic)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	pages := map[string]string{
		"now": "/", "approvals": "/approvals", "cases": "/cases", "cases-pivot": "/cases?p=kind%3Afailed_backup&p=target%3Ahost%3Amodel-host",
		"case": "/cases/" + waitID, "case-escalated": "/cases/" + escID, "case-verified": "/cases/" + okID,
		"runs": "/runs", "runs-problems": "/runs?f=problems", "run": "/runs/ops-rerun-backup-0001",
		"fleets": "/fleets", "fleet": "/fleets/jobs", "fleet-triage": "/fleets/triage", "fleet-routines": "/fleets/jobs?tab=routines",
		"fleet-stats": "/fleets/jobs?tab=stats", "fleet-logs": "/fleets/triage?tab=logs",
		"routine": "/fleets/triage/cycle", "routine-block": "/fleets/triage/cycle?b=gate.edge", "routine-bugfix": "/fleets/bugfix/fix",
		"hosts": "/hosts", "host": "/hosts/vps", "host-agent": "/hosts/backup-01?tab=agent", "host-exec": "/hosts/vps?tab=exec",
		"case-meeting": "/cases/" + meetID, "fleet-meetings": "/fleets/meetings", "fleet-bugfix": "/fleets/bugfix",
		"fleet-triage-stats": "/fleets/triage?tab=stats", "fleet-meetings-stats": "/fleets/meetings?tab=stats",
		"autonomy": "/autonomy", "history": "/history?period=all", "history-ledger": "/history?type=ledger&period=all",
		"account": "/account", "users": "/users", "models": "/models", "models-spend": "/models?tab=spend&period=30d",
		"run-triage": "/runs/" + firstTriage(st, t), "signin": "/signin",
		"knowledge": "/knowledge", "brief": "/brief", "brief-archive": "/brief/archive", "examples": "/examples"}
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

// firstTriage is the newest triage/cycle run, for a run page with model calls.
func firstTriage(st *store.Store, t *testing.T) string {
	rs, _, err := st.RunsPage(context.Background(), store.RunQuery{Routine: "triage/cycle", Limit: 1, Desc: true})
	if err != nil || len(rs) == 0 {
		t.Fatalf("no triage run: %v", err)
	}
	return rs[0].ID
}
