package store

import (
	"context"
	"testing"
)

// A hook's activity is read from its handler's events: accepted and refused counted, the last refusal's reason kept.
func TestHookActivities(t *testing.T) {
	ctx := context.Background()
	s, _ := open(t)
	for _, e := range []struct{ kind, detail string }{
		{"hook.refused", "bad signature"}, {"hook.accepted", "opened; delivery d1"}, {"hook.refused", "timestamp too old"},
	} {
		if err := s.Event(ctx, "hook:meeting", "hook", e.kind, "hook:meeting", e.detail); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.HookActivities(ctx, "2000-01-01")
	if err != nil {
		t.Fatal(err)
	}
	if a := got["meeting"]; a.Accepted != 1 || a.Refused != 2 || a.LastRefused != "timestamp too old" || a.LastAt == "" {
		t.Fatalf("%+v", got)
	}
	if got, _ := s.HookActivities(ctx, "2999-01-01"); len(got) != 0 {
		t.Fatalf("nothing since the future: %+v", got)
	}
}

// An answer belongs to the plan the case was asking about when it came: a case that proposed two actions in turn counts each once.
func TestAnswersByAction(t *testing.T) {
	ctx := context.Background()
	s, _ := open(t)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := s.db.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO cases (id, kind, target, canonical_key, project, urgency, status, opened_at, status_at) VALUES
		('m', 'meeting', 'f', 'k=m', 'meetings', 0, 'closed', '2026-11-23T08:00:00Z', '2026-11-23T08:00:00Z')`)
	for _, p := range []struct{ action, at string }{{"plumbing.create_tasks", "2026-11-23T08:01:00Z"}, {"plumbing.post_decisions", "2026-11-23T08:10:00Z"}} {
		exec(`INSERT INTO plans (case_id, run_id, action, params, plan_hash, outcome, autonomy, reasons, decided_at) VALUES ('m', 'r', ?, '{}', 'h', 'propose', 'propose', '[]', ?)`,
			p.action, p.at)
	}
	for _, e := range []struct{ to, at string }{{"executing", "2026-11-23T08:02:00Z"}, {"escalated", "2026-11-23T08:11:00Z"}} {
		exec(`INSERT INTO case_events (case_id, at, kind, actor, by_role, from_status, to_status) VALUES ('m', ?, ?, 'user:ada', 'operator', 'awaiting_approval', ?)`,
			e.at, "case."+e.to, e.to)
	}
	got, err := s.AnswersByAction(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got["plumbing.create_tasks"] != [2]int{1, 0} || got["plumbing.post_decisions"] != [2]int{0, 1} {
		t.Fatalf("%+v", got)
	}
}

// The ledger's live decisions by fleet and disposition; a shadow's and a recurrence are not decisions of the fleet.
func TestLedgerMetrics(t *testing.T) {
	ctx := context.Background()
	s, _ := open(t)
	for _, e := range []struct{ ns, kind, row string }{
		{"triage/cycle", "decision", `{"disposition": "file", "stage": "propose"}`},
		{"triage/cycle", "decision", `{"disposition": "file", "stage": "live"}`},
		{"triage/cycle", "decision", `{"disposition": "dismiss", "stage": "live"}`},
		{"triage/cycle", "decision", `{"disposition": "file", "stage": "shadow"}`},
		{"triage/cycle", "recurrence", `{"disposition": "file"}`},
	} {
		if _, err := s.db.Exec(`INSERT INTO ledger_entries (namespace, kind, row, written_at) VALUES (?, ?, ?, '2026-11-23T08:00:00Z')`, e.ns, e.kind, e.row); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.LedgerMetrics(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != (LedgerCount{"triage", "dismiss", 1}) || got[1] != (LedgerCount{"triage", "file", 2}) {
		t.Fatalf("%+v", got)
	}
}
