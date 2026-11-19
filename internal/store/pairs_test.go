package store

import (
	"context"
	"encoding/json"
	"testing"
)

// pairWorld has one input, ops/rerun's live run "live-1" (v1), and v2's finished shadow run "s-1" on it.
func pairWorld(t *testing.T) (*Store, context.Context) {
	t.Helper()
	s, ctx := openTest(t), context.Background()
	if err := s.NewInput(ctx, Input{Key: "live-1", Routine: "triage/cycle", Kind: InputTick, Version: 1, StartedAt: "2026-11-19T10:00:00.000000000Z"}); err != nil {
		t.Fatal(err)
	}
	shadowDone(t, s, "live-1", 2, "s-1", ShadowDone)
	return s, ctx
}

func shadowDone(t *testing.T, s *Store, input string, version int, run string, status string) {
	t.Helper()
	ctx := context.Background()
	id, err := s.QueueShadowRun(ctx, ShadowRun{InputKey: input, Routine: "triage/cycle", Version: version, VersionHash: "h2"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ShadowRunStep(ctx, id, ShadowRunning, "", run); err != nil {
		t.Fatal(err)
	}
	if status == ShadowDone {
		if err := s.ShadowRunStep(ctx, id, ShadowDone, "", ""); err != nil {
			t.Fatal(err)
		}
	}
}

func shadowRec(t *testing.T, s *Store, input, run, subject, kind, payload string) {
	t.Helper()
	if _, err := s.RecordShadow(context.Background(), ShadowRecord{RunID: run, Routine: "triage/cycle@v2", Version: 2, VersionHash: "h2", InputKey: input,
		Subject: subject, Kind: kind, Payload: json.RawMessage(payload)}); err != nil {
		t.Fatal(err)
	}
}

func byKey(ps PairSet) map[string]Pair {
	out := map[string]Pair{}
	for _, p := range ps.Pairs {
		out[p.InputKey+"|"+p.Subject] = p
	}
	return out
}

// Ledger decisions: matched per subject; the same disposition is the same, another differs, a subject one side did not decide is unpaired.
func TestPairsOfLedgerDecisions(t *testing.T) {
	s, ctx := pairWorld(t)
	live := func(fp, disp, reason string) {
		if _, err := s.AppendEntry(ctx, "triage/cycle", "decision", map[string]string{"fingerprint": fp, "disposition": disp, "reason": reason}, "live-1"); err != nil {
			t.Fatal(err)
		}
	}
	live("fp-1", "file", "a real error")
	live("fp-2", "file", "the same error, again")
	live("fp-4", "dismiss", "only the live version decided this")
	s.AppendEntry(ctx, "triage/cycle", "recurrence", map[string]string{"fingerprint": "fp-1"}, "live-1") // not a decision
	shadowRec(t, s, "live-1", "s-1", "fp-1", "decision", `{"fingerprint":"fp-1","disposition":"file","reason":"same"}`)
	shadowRec(t, s, "live-1", "s-1", "fp-2", "decision", `{"fingerprint":"fp-2","disposition":"dismiss","reason":"noise"}`)
	shadowRec(t, s, "live-1", "s-1", "fp-3", "decision", `{"fingerprint":"fp-3","disposition":"escalate"}`)
	ps, err := s.Pairs(ctx, PairQuery{Routine: "triage/cycle", Version: 2})
	if err != nil {
		t.Fatal(err)
	}
	if ps.Paired != 2 || ps.Differ != 1 || ps.Unpaired != 2 || len(ps.Pairs) != 4 || ps.StationsNote == "" {
		t.Fatalf("%+v", ps)
	}
	m := byKey(ps)
	if p := m["live-1|fp-1"]; !p.Same || p.Live.Outcome != "file" || p.Shadow.Outcome != "file" || p.Shadow.Hash != "h2" || p.Shadow.Version != 2 || p.Live.Version != 1 {
		t.Fatalf("%+v", p)
	}
	if p := m["live-1|fp-2"]; p.Same || p.Live.Text != "file" || p.Shadow.Text != "dismiss" || p.Shadow.Detail != "noise" || p.Live.Detail != "the same error, again" {
		t.Fatalf("%+v", p)
	}
	if p := m["live-1|fp-3"]; p.Live != nil || p.Shadow == nil || p.Same {
		t.Fatalf("unpaired, the shadow's: %+v", p)
	}
	if p := m["live-1|fp-4"]; p.Shadow != nil || p.Live == nil {
		t.Fatalf("unpaired, the live one's: %+v", p)
	}
	if n, err := s.PairsFor(ctx, "triage/cycle", 2); err != nil || n != 2 {
		t.Fatalf("a version's pairs are the paired subjects: %d %v", n, err)
	}
	if n, _ := s.PairsFor(ctx, "triage/cycle", 3); n != 0 {
		t.Fatalf("another version has none: %d", n)
	}
	// a shadow's own ledger rows are not the live side
	s.AppendEntry(ctx, "triage/cycle@v2", "decision", map[string]string{"fingerprint": "fp-3", "disposition": "escalate"}, "live-1")
	if ps2, _ := s.Pairs(ctx, PairQuery{Routine: "triage/cycle", Version: 2}); ps2.Paired != 2 {
		t.Fatalf("%+v", ps2)
	}
}

// A case: the live plan (sent to a host or not) against the shadow's plan and the action it would have run.
func TestPairsOfCasePlans(t *testing.T) {
	s, ctx := pairWorld(t)
	mk := func(key string) string {
		id, _, err := s.OpenCase(ctx, NewCase{Kind: "backup", Target: "host:b", Keys: []Key{{"target", key}}, Canonical: Key{"target", key}}, "ops/m", "d")
		if err != nil {
			t.Fatal(err)
		}
		s.db.Exec(`UPDATE cases SET status = 'planned' WHERE id = ?`, id)
		return id
	}
	sent, notSent := mk("backup/host:a"), mk("backup/host:b")
	for _, c := range []struct{ id, action string }{{sent, "backup.rerun"}, {notSent, "backup.rerun"}} {
		pid, err := s.ApplyPlan(ctx, PlanRecord{CaseID: c.id, RunID: "live-1", Action: c.action, Params: map[string]string{"host": "h"}, Outcome: "propose", Autonomy: "propose"}, "awaiting_approval", "", "", false)
		if err != nil {
			t.Fatal(err)
		}
		if c.id == sent {
			if err := s.StartHostExecution(ctx, HostExecution{ID: "x1", CaseID: c.id, PlanID: pid, Host: "h", Action: c.action, Approved: "a", Envelope: []byte("{}")}); err != nil {
				t.Fatal(err)
			}
		}
	}
	plan := `{"outcome":"plan","action":"backup.rerun","params":{"host":"h"},"detail":"rerun"}`
	would := `{"action":"backup.rerun","params":{"host":"h"},"blocked":"shadow","text":"would run backup.rerun (blocked: shadow)"}`
	shadowRec(t, s, "live-1", "s-1", sent, "plan", plan)
	shadowRec(t, s, "live-1", "s-1", sent, "would_run", would)
	shadowRec(t, s, "live-1", "s-1", notSent, "plan", `{"outcome":"plan","action":"backup.other"}`)
	ps, _ := s.Pairs(ctx, PairQuery{Routine: "triage/cycle", Version: 2})
	m := byKey(ps)
	p := m["live-1|"+sent]
	if !p.Same || p.Live.Text != "ran backup.rerun" || !p.Live.Plan.Ran || p.Shadow.Text != "would run backup.rerun (blocked: shadow)" || p.Shadow.Plan.Blocked != "shadow" || p.Shadow.Plan.Params["host"] != "h" {
		t.Fatalf("%+v %+v", p.Live, p.Shadow)
	}
	q := m["live-1|"+notSent]
	if q.Same || q.Live.Text != "plan backup.rerun (propose)" || q.Shadow.Plan.Action != "backup.other" {
		t.Fatalf("%+v %+v", q.Live, q.Shadow)
	}
}

// A monitor: what the live run did to the case or incident of a detection key, and what a shadow would have.
func TestPairsOfDetections(t *testing.T) {
	s, ctx := pairWorld(t)
	k := func(v string) Key { return Key{"target", v} }
	if _, how, err := s.DetectIncident(ctx, NewCase{Kind: "backup", Target: "host:new", Keys: []Key{k("backup/host:new")}, Canonical: k("backup/host:new")}, "triage/cycle", "run live-1: found"); err != nil || how != Opened {
		t.Fatalf("%v %v", how, err)
	}
	// an older case, attached by the live run again
	if _, _, err := s.DetectIncident(ctx, NewCase{Kind: "backup", Target: "host:old", Keys: []Key{k("backup/host:old")}, Canonical: k("backup/host:old")}, "triage/cycle", "run live-0: found"); err != nil {
		t.Fatal(err)
	}
	if _, how, err := s.DetectIncident(ctx, NewCase{Kind: "backup", Target: "host:old", Keys: []Key{k("backup/host:old")}, Canonical: k("backup/host:old")}, "triage/cycle", "run live-1: still there"); err != nil || how == Opened {
		t.Fatalf("%v %v", how, err)
	}
	shadowRec(t, s, "live-1", "s-1", "target=backup/host:new", "detect.open", `{"kind":"backup","target":"host:new","detail":"d"}`)
	shadowRec(t, s, "live-1", "s-1", "target=backup/host:old", "detect.open", `{"kind":"backup","target":"host:old"}`) // the live run attached it: they differ
	shadowRec(t, s, "live-1", "s-1", "target=backup/host:gone", "detect.clear", `{}`)
	ps, _ := s.Pairs(ctx, PairQuery{Routine: "triage/cycle", Version: 2})
	m := byKey(ps)
	if p := m["live-1|target=backup/host:new"]; !p.Same || p.Live.Text != "open case" || p.Shadow.Text != "open case" {
		t.Fatalf("%+v", p)
	}
	if p := m["live-1|target=backup/host:old"]; p.Same || p.Live.Outcome != "attach" || p.Shadow.Outcome != "open" {
		t.Fatalf("%+v %+v", p.Live, p.Shadow)
	}
	if p := m["live-1|target=backup/host:gone"]; p.Live != nil || p.Shadow.Text != "clear" {
		t.Fatalf("%+v", p)
	}
	if ps.Paired != 2 || ps.Differ != 1 || ps.Unpaired != 1 {
		t.Fatalf("%+v", ps)
	}
}

// Only inputs a version's shadow run finished count, in the window asked for, per version.
func TestPairsCountOnlyFinishedRunsInTheWindow(t *testing.T) {
	s, ctx := pairWorld(t)
	shadowRec(t, s, "live-1", "s-1", "fp-1", "decision", `{"disposition":"file"}`)
	s.AppendEntry(ctx, "triage/cycle", "decision", map[string]string{"fingerprint": "fp-1", "disposition": "file"}, "live-1")
	// a second input whose shadow run is still running, and a third later one
	s.NewInput(ctx, Input{Key: "live-2", Routine: "triage/cycle", Kind: InputTick, Version: 1, StartedAt: "2026-11-19T11:00:00.000000000Z"})
	shadowDone(t, s, "live-2", 2, "s-2", ShadowRunning)
	shadowRec(t, s, "live-2", "s-2", "fp-1", "decision", `{"disposition":"file"}`)
	s.AppendEntry(ctx, "triage/cycle", "decision", map[string]string{"fingerprint": "fp-1", "disposition": "file"}, "live-2")
	s.NewInput(ctx, Input{Key: "live-3", Routine: "triage/cycle", Kind: InputTick, Version: 1, StartedAt: "2026-11-19T12:00:00.000000000Z"})
	shadowDone(t, s, "live-3", 2, "s-3", ShadowDone)
	shadowRec(t, s, "live-3", "s-3", "fp-1", "decision", `{"disposition":"file"}`)
	s.AppendEntry(ctx, "triage/cycle", "decision", map[string]string{"fingerprint": "fp-1", "disposition": "file"}, "live-3")
	all, _ := s.Pairs(ctx, PairQuery{Routine: "triage/cycle", Version: 2})
	if all.Paired != 2 || all.Pairs[0].InputKey != "live-3" || all.Pairs[1].InputKey != "live-1" {
		t.Fatalf("a running shadow's input is not counted, newest first: %+v", all.Pairs)
	}
	since, _ := s.Pairs(ctx, PairQuery{Routine: "triage/cycle", Version: 2, Since: "2026-11-19T11:30:00.000000000Z"})
	until, _ := s.Pairs(ctx, PairQuery{Routine: "triage/cycle", Version: 2, Until: "2026-11-19T11:30:00.000000000Z"})
	if since.Paired != 1 || since.Pairs[0].InputKey != "live-3" || until.Paired != 1 || until.Pairs[0].InputKey != "live-1" {
		t.Fatalf("%+v %+v", since.Pairs, until.Pairs)
	}
}

func TestPairsCarryEachSidesUsagePerRole(t *testing.T) {
	s, ctx := pairWorld(t)
	shadowRec(t, s, "live-1", "s-1", "fp-1", "decision", `{"disposition":"file"}`)
	s.AppendEntry(ctx, "triage/cycle", "decision", map[string]string{"fingerprint": "fp-1", "disposition": "file"}, "live-1")
	in, out, cost := int64(100), int64(10), 0.01
	calls := func(run, routine string, roles ...string) {
		var cs []ModelCall
		for _, r := range roles {
			cs = append(cs, ModelCall{RunID: run, Routine: routine, At: "2026-11-19T10:00:01.000000000Z", Alias: "a", Role: r, TokensIn: &in, TokensOut: &out, CostUSD: &cost})
		}
		cs = append(cs, ModelCall{RunID: run, Routine: routine, Role: "harness", Kind: KindStep, Detail: "a step is not a call"})
		if err := s.RecordCalls(ctx, cs); err != nil {
			t.Fatal(err)
		}
	}
	// the live run's rows under the live key, the shadow's under its own
	s.StartRun(ctx, Run{ID: "live-1", Routine: "triage/cycle", SpecHash: "h", Trigger: "schedule", Actor: "flotillad", Autonomy: "record", StartedAt: "2026-11-19T10:00:00.000000000Z", Status: StatusRunning})
	calls("live-1", "triage/cycle", "triage", "triage", "verdict")
	calls("s-1", "triage/cycle@v2", "triage")
	ps, _ := s.Pairs(ctx, PairQuery{Routine: "triage/cycle", Version: 2})
	p := ps.Pairs[0]
	if len(p.Live.Stations) != 2 || p.Live.Stations[0].Role != "triage" || p.Live.Stations[0].Calls != 2 || p.Live.Stations[0].TokensIn != 200 || p.Live.Stations[1].Role != "verdict" {
		t.Fatalf("%+v", p.Live.Stations)
	}
	if len(p.Shadow.Stations) != 1 || p.Shadow.Stations[0].Calls != 1 || p.Shadow.Stations[0].CostUSD != 0.01 {
		t.Fatalf("%+v", p.Shadow.Stations)
	}
}
