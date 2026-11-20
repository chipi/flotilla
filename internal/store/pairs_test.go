package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
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
	if ps.Paired != 2 || ps.Differ != 1 || ps.Unpaired != 2 || len(ps.Pairs) != 4 {
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

func TestPairsCarryTheRenderedTextsOfEachStationAndOldRowsStillRead(t *testing.T) {
	s, ctx := pairWorld(t)
	shadowRec(t, s, "live-1", "s-1", "fp-1", "decision", `{"disposition":"file"}`)
	s.AppendEntry(ctx, "triage/cycle", "decision", map[string]string{"fingerprint": "fp-1", "disposition": "file"}, "live-1")
	s.StartRun(ctx, Run{ID: "live-1", Routine: "triage/cycle", SpecHash: "h", Trigger: "schedule", Actor: "flotillad", Autonomy: "record", StartedAt: "2026-11-19T10:00:00.000000000Z", Status: StatusRunning})
	body := "Decide it."
	sha := strings.Repeat("a", 64)
	call := func(run, routine, station, role string, texts ...CallText) ModelCall {
		return ModelCall{RunID: run, Routine: routine, At: "2026-11-19T10:00:01.000000000Z", Alias: "a", Station: station, Role: role, TextBodies: texts}
	}
	t1 := CallText{Name: "task", SHA256: sha, Bytes: int64(len(body)), Text: &body, Redacted: true}
	// the same text read by two calls is one entry; a row from before stations (a role, no station) groups under its role
	if err := s.RecordCalls(ctx, []ModelCall{call("live-1", "triage/cycle", "decide", "", t1), call("live-1", "triage/cycle", "decide", "", t1), call("live-1", "triage/cycle", "", "verdict"),
		call("s-1", "triage/cycle@v2", "decide", "", t1)}); err != nil {
		t.Fatal(err)
	}
	ps, _ := s.Pairs(ctx, PairQuery{Routine: "triage/cycle", Version: 2})
	live := ps.Pairs[0].Live.Stations
	if len(live) != 2 || live[0].Role != "decide" || live[0].Calls != 2 || len(live[0].Texts) != 1 || *live[0].Texts[0].Text != body || live[1].Role != "verdict" || len(live[1].Texts) != 0 {
		t.Fatalf("%+v", live)
	}
	if sh := ps.Pairs[0].Shadow.Stations; len(sh) != 1 || sh[0].Texts[0].SHA256 != sha {
		t.Fatalf("%+v", sh)
	}
	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM call_texts`).Scan(&n)
	if n != 1 {
		t.Fatalf("a text is kept once by its hash: %d rows", n)
	}
}

func TestATextOverTheCapOrTheRunsBudgetKeepsItsHashAndLengthAndNoBody(t *testing.T) {
	s, ctx := pairWorld(t)
	big := strings.Repeat("x", MaxTextBytes+1)
	ok := "fine"
	nobody := CallText{Name: "n", SHA256: strings.Repeat("b", 64), Bytes: 99, OverCap: true}
	over := CallText{Name: "big", SHA256: strings.Repeat("c", 64), Bytes: int64(len(big)), Text: &big}
	fine := CallText{Name: "ok", SHA256: strings.Repeat("d", 64), Bytes: 4, Text: &ok, Redacted: true}
	if err := s.RecordCalls(ctx, []ModelCall{{RunID: "r", Routine: "triage/cycle", Alias: "a", Station: "s", TextBodies: []CallText{nobody, over, fine}}}); err != nil {
		t.Fatal(err)
	}
	for sha, want := range map[string]bool{nobody.SHA256: false, over.SHA256: false, fine.SHA256: true} {
		var body sql.NullString
		var capped bool
		if err := s.db.QueryRow(`SELECT text, over_cap FROM call_texts WHERE sha256 = ?`, sha).Scan(&body, &capped); err != nil {
			t.Fatal(err)
		}
		if body.Valid != want || capped == want {
			t.Errorf("%s: body kept %v, over_cap %v", sha[:4], body.Valid, capped)
		}
	}
	// past the run's budget the next new body is dropped too, and the call still records
	var cs []ModelCall
	for i := 0; i < 10; i++ {
		b := strings.Repeat("y", MaxTextBytes)
		cs = append(cs, ModelCall{RunID: "r2", Routine: "triage/cycle", Alias: "a", Station: "s", TextBodies: []CallText{{Name: "t", SHA256: strings.Repeat(string(rune('e'+i)), 64), Bytes: int64(len(b)), Text: &b}}})
	}
	if err := s.RecordCalls(ctx, cs); err != nil {
		t.Fatal(err)
	}
	var kept int
	s.db.QueryRow(`SELECT COUNT(*) FROM call_texts WHERE text IS NOT NULL AND length(text) = ?`, MaxTextBytes).Scan(&kept)
	if kept != MaxRunTextBytes/MaxTextBytes {
		t.Fatalf("the run's budget keeps %d bodies, kept %d", MaxRunTextBytes/MaxTextBytes, kept)
	}
}

// The run's budget is charged for a body written, not for a text the table already had or one the run repeats: a 40 KB text on 20 calls costs
// 40 KB, and the 3 KB texts beside it all keep their bodies (charged per call, 12 of them would use the whole 512 KB).
func TestTheBudgetIsChargedOncePerTextWrittenNotPerCall(t *testing.T) {
	s, ctx := pairWorld(t)
	big := strings.Repeat("b", 40*1024)
	var cs []ModelCall
	var smalls []string
	for i := 0; i < 20; i++ {
		small := strings.Repeat(string(rune('a'+i)), 3*1024)
		smalls = append(smalls, small)
		cs = append(cs, ModelCall{RunID: "r", Routine: "triage/cycle", Alias: "a", Station: "s", TextBodies: []CallText{
			{Name: "big", SHA256: strings.Repeat("9", 64), Bytes: int64(len(big)), Text: &big},
			{Name: "small", SHA256: strings.Repeat(string(rune('a'+i)), 64), Bytes: int64(len(small)), Text: &small}}})
	}
	if err := s.RecordCalls(ctx, cs); err != nil {
		t.Fatal(err)
	}
	var withBody int
	s.db.QueryRow(`SELECT COUNT(*) FROM call_texts WHERE text IS NOT NULL`).Scan(&withBody)
	if withBody != 21 {
		t.Fatalf("the big text once and all 20 small ones keep their bodies: %d", withBody)
	}
	// a second run carrying the same big text finds it kept: it is a no-op and costs that run nothing
	small2 := "later small"
	if err := s.RecordCalls(ctx, []ModelCall{{RunID: "r2", Routine: "triage/cycle", Alias: "a", Station: "s", TextBodies: []CallText{
		{Name: "big", SHA256: strings.Repeat("9", 64), Bytes: int64(len(big)), Text: &big}, {Name: "small", SHA256: strings.Repeat("z", 64), Bytes: int64(len(small2)), Text: &small2}}}}); err != nil {
		t.Fatal(err)
	}
	var body sql.NullString
	s.db.QueryRow(`SELECT text FROM call_texts WHERE sha256 = ?`, strings.Repeat("z", 64)).Scan(&body)
	if !body.Valid {
		t.Fatal("a later run's small text keeps its body")
	}
}

// A text the table already holds is a no-op and costs the run nothing: a run that repeats twelve kept 40 KB texts still has its budget for a new one.
func TestARepeatOfAKeptTextIsNotChargedToTheBudget(t *testing.T) {
	s, ctx := pairWorld(t)
	mk := func(i int) CallText {
		b := strings.Repeat(string(rune('a'+i)), 40*1024)
		return CallText{Name: "t", SHA256: strings.Repeat(string(rune('a'+i)), 64), Bytes: int64(len(b)), Text: &b}
	}
	var first []CallText
	for i := 0; i < 12; i++ {
		first = append(first, mk(i))
	}
	if err := s.RecordCalls(ctx, []ModelCall{{RunID: "r0", Routine: "triage/cycle", Alias: "a", Station: "s", TextBodies: first}}); err != nil {
		t.Fatal(err)
	}
	fresh := mk(20)
	if err := s.RecordCalls(ctx, []ModelCall{{RunID: "r1", Routine: "triage/cycle", Alias: "a", Station: "s", TextBodies: append(append([]CallText{}, first...), fresh)}}); err != nil {
		t.Fatal(err)
	}
	var body sql.NullString
	s.db.QueryRow(`SELECT text FROM call_texts WHERE sha256 = ?`, fresh.SHA256).Scan(&body)
	if !body.Valid {
		t.Fatal("the new text keeps its body: the twelve repeats were not charged")
	}
}

// A text kept with no body (it was over its cap, or past a run's budget) gains the body when a later run has it; one with a body is left alone;
// a reference with no body for a hash never seen writes no row.
func TestATextKeptWithoutABodyGainsItWhenALaterRunHasIt(t *testing.T) {
	s, ctx := pairWorld(t)
	sha := strings.Repeat("c", 64)
	if err := s.RecordCalls(ctx, []ModelCall{{RunID: "r1", Routine: "triage/cycle", Alias: "a", Station: "s", TextBodies: []CallText{{Name: "t", SHA256: sha, Bytes: 5, OverCap: true}}}}); err != nil {
		t.Fatal(err)
	}
	read := func() (sql.NullString, bool) {
		var b sql.NullString
		var over bool
		if err := s.db.QueryRow(`SELECT text, over_cap FROM call_texts WHERE sha256 = ?`, sha).Scan(&b, &over); err != nil {
			t.Fatal(err)
		}
		return b, over
	}
	if b, over := read(); b.Valid || !over {
		t.Fatalf("kept without a body: %v %v", b, over)
	}
	body, other := "hello", "other body"
	if err := s.RecordCalls(ctx, []ModelCall{{RunID: "r2", Routine: "triage/cycle", Alias: "a", Station: "s", TextBodies: []CallText{{Name: "t", SHA256: sha, Bytes: 5, Text: &body, Redacted: true}}}}); err != nil {
		t.Fatal(err)
	}
	if b, over := read(); !b.Valid || b.String != "hello" || over {
		t.Fatalf("the later run's body is kept: %v %v", b, over)
	}
	if err := s.RecordCalls(ctx, []ModelCall{{RunID: "r3", Routine: "triage/cycle", Alias: "a", Station: "s", TextBodies: []CallText{{Name: "t", SHA256: sha, Bytes: 5, Text: &other}}}}); err != nil {
		t.Fatal(err)
	}
	if b, _ := read(); b.String != "hello" {
		t.Fatalf("a body already kept is not replaced: %v", b)
	}
	if err := s.RecordCalls(ctx, []ModelCall{{RunID: "r4", Routine: "triage/cycle", Alias: "a", Station: "s", TextBodies: []CallText{{Name: "t", SHA256: strings.Repeat("d", 64), Bytes: 5}}}}); err != nil {
		t.Fatal(err)
	}
	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM call_texts WHERE sha256 = ?`, strings.Repeat("d", 64)).Scan(&n)
	if n != 0 {
		t.Fatal("a reference with no body, for a text never seen, writes no row")
	}
}

func TestAStoreAtTheLatestSchemaReadsOldCallsWithNoStation(t *testing.T) {
	if len(schema) != 33 {
		t.Fatalf("step 33 is the routine snapshots; the schema has %d steps", len(schema))
	}
	s, ctx := pairWorld(t)
	if err := s.RecordCalls(ctx, []ModelCall{{RunID: "old", Routine: "triage/cycle", Alias: "a", Role: "triage"}}); err != nil {
		t.Fatal(err)
	}
	m, _ := s.RunCalls(ctx, []string{"old"})
	if c := m["old"][0]; c.Station != "" || c.Texts != "" || c.Role != "triage" {
		t.Fatalf("%+v", c)
	}
}
