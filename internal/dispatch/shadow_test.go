package dispatch

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/chipi/flotilla/internal/sched"
	"github.com/chipi/flotilla/internal/spec"
	"github.com/chipi/flotilla/internal/store"
)

// shadowWorld is a store with live rows in it, and a way to read every table that is not a shadow's own.
type shadowWorld struct {
	t    *testing.T
	st   *store.Store
	path string
}

func newShadowWorld(t *testing.T) *shadowWorld {
	t.Helper()
	p := filepath.Join(t.TempDir(), "s.db")
	st, err := store.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	w := &shadowWorld{t: t, st: st, path: p}
	ctx := context.Background()
	// something in every table the live path writes
	id, _, err := st.OpenCase(ctx, store.NewCase{Kind: "backup", Target: "host:b", Keys: []store.Key{{Kind: "target", Value: "backup/host:b"}}, Canonical: store.Key{Kind: "target", Value: "backup/host:b"}}, "ops/backup-monitor", "found")
	if err != nil {
		t.Fatal(err)
	}
	_ = id
	st.WriteFiling(ctx, "ops/backup-monitor", map[string]string{"fingerprint": "a", "issue": "1"}, "r0")
	st.AppendEntry(ctx, "ops/backup-monitor", "decision", map[string]string{"fingerprint": "a"}, "r0")
	st.AddSpend(ctx, "2026-11-19", "ops/backup-monitor", 0.3)
	st.SetControl(ctx, "ops", "stop", "0", "operator", "test")
	st.Event(ctx, "operator", "cli", "x.y", "subject", "detail")
	st.StartRun(ctx, store.Run{ID: "r0", Routine: "ops/backup-monitor", SpecHash: "h", Trigger: "schedule", Actor: "flotillad", Autonomy: "record", StartedAt: "2026-11-19T09:00:00.000000000Z", Status: store.StatusRunning})
	return w
}

// dump is a digest of every table but the shadow's own (shadow_records, shadow_runs, run_inputs), row by row.
func (w *shadowWorld) dump() map[string]string {
	w.t.Helper()
	db, err := sql.Open("sqlite", "file:"+w.path+"?mode=ro")
	if err != nil {
		w.t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		w.t.Fatal(err)
	}
	var names []string
	for rows.Next() {
		var n string
		rows.Scan(&n)
		names = append(names, n)
	}
	rows.Close()
	out := map[string]string{}
	for _, n := range names {
		switch n {
		case "shadow_records", "shadow_runs", "run_inputs":
			continue
		}
		r, err := db.Query(`SELECT * FROM ` + n + ` ORDER BY rowid`)
		if err != nil {
			// a table without rowid: read it unordered
			if r, err = db.Query(`SELECT * FROM ` + n); err != nil {
				w.t.Fatal(err)
			}
		}
		cols, _ := r.Columns()
		var lines []string
		for r.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			r.Scan(ptrs...)
			b, _ := json.Marshal(vals)
			lines = append(lines, string(b))
		}
		r.Close()
		sort.Strings(lines)
		out[n] = strings.Join(lines, "\n")
	}
	return out
}

func (w *shadowWorld) sameAsBefore(before map[string]string) {
	w.t.Helper()
	after := w.dump()
	for table, b := range before {
		if after[table] != b {
			w.t.Errorf("a shadow run changed table %s:\n before %q\n  after %q", table, b, after[table])
		}
	}
	if len(after) != len(before) {
		w.t.Errorf("tables: %d before, %d after", len(before), len(after))
	}
}

func shadowRun(status string) store.Run {
	d := 2.5
	return store.Run{ID: "s-1", Routine: "ops/backup-monitor@v2", Version: 2, Shadow: true, InputKey: "live-1", Status: status, SpendUSD: 0.01,
		DurationS: &d, StartedAt: "2026-11-19T10:00:05.000000000Z", Detail: ""}
}

func plan(raw string) *sched.CaseOutcome { return &sched.CaseOutcome{Plan: json.RawMessage(raw)} }

func inputOf(kind string, handed string, keys ...string) store.Input {
	return store.Input{Key: "live-1", Routine: "ops/backup-monitor", Kind: kind, Version: 1, StartedAt: "2026-11-19T10:00:00.000000000Z",
		Handed: json.RawMessage(handed), LiveCaseKeys: keys}
}

func kindsOf(recs []store.ShadowRecord) string {
	var out []string
	for _, r := range recs {
		out = append(out, r.Kind+":"+r.Subject)
	}
	return strings.Join(out, " ")
}

// A shadow monitor's report is stored whole, with what it would open, attach and clear against the live case keys as of its input,
// and nothing else in the system changes: not a case, a plan, a lease, spend, an event, the brief.
func TestAShadowMonitorsReportIsRecordedAndChangesNothingElse(t *testing.T) {
	w := newShadowWorld(t)
	ctx := context.Background()
	sj := &spec.Routine{Fleet: "ops", Routine: "backup-monitor", Detects: []string{"backup", "disk"}, BriefSection: "Backups"}
	report := `{"outcome":"detections","summary":["a line for the brief"],"detections":[` +
		`{"kind":"backup","target":"host:b","detail":"old","title":"t1"},` + // already an open case on the live keys: attach
		`{"kind":"disk","target":"host:d","detail":"full"},` + // new: open
		`{"kind":"nope","target":"host:x","detail":"x"}]}` // a kind the routine does not detect
	before := w.dump()
	in := inputOf(store.InputTick, "{}", "target=backup/host:b", "target=backup/host:gone")
	if err := ShadowDone(ctx, w.st, sj, shadowRun(store.StatusOK), "h2", in, plan(report)); err != nil {
		t.Fatal(err)
	}
	w.sameAsBefore(before)
	recs, _ := w.st.ShadowRecords(ctx, store.ShadowRecordFilter{InputKey: "live-1"})
	got := kindsOf(recs)
	for _, want := range []string{"run.ended:", "report:", "detect.attach:target=backup/host:b", "detect.open:target=disk/host:d", "detect.refused:", "detect.clear:target=backup/host:gone"} {
		if !strings.Contains(got, want) {
			t.Errorf("no record %q in: %s", want, got)
		}
	}
	for _, r := range recs {
		if r.Routine != "ops/backup-monitor@v2" || r.Version != 2 || r.VersionHash != "h2" || r.RunID != "s-1" || r.InputKey != "live-1" {
			t.Fatalf("every record carries the shadow's key, version, hash, run and the input's key: %+v", r)
		}
		if r.Kind == RecReport && !strings.Contains(string(r.Payload), "a line for the brief") {
			t.Fatalf("the report is stored whole: %s", r.Payload)
		}
	}
}

func TestShadowOutcomesOfCaseRoutines(t *testing.T) {
	ctx := context.Background()
	handed := `{"case":{"id":"case-1","kind":"backup"},"plan":{"action":"backup.rerun_observability","params":{"host":"h1"}}}`
	for name, c := range map[string]struct {
		kind, plan string
		want       []string
		wouldRun   string
	}{
		"a plan is recorded and its action is only a would_run": {store.InputCase, `{"outcome":"plan","action":"backup.rerun_observability","params":{"host":"h1"},"detail":"d"}`,
			[]string{"run.ended:", "plan:case-1", "would_run:case-1"}, "would run backup.rerun_observability (blocked: shadow)"},
		"a close":                      {store.InputCase, `{"outcome":"resolved_itself","detail":"gone"}`, []string{"run.ended:", "decision:case-1"}, ""},
		"an escalation":                {store.InputCase, `{"outcome":"escalate"}`, []string{"run.ended:", "decision:case-1"}, ""},
		"a check":                      {store.InputCheck, `{"outcome":"verified"}`, []string{"run.ended:", "decision:case-1"}, ""},
		"an execute":                   {store.InputExecute, `{"outcome":"executed","ref":"o/r#1"}`, []string{"run.ended:", "would_run:case-1", "decision:case-1"}, "would run backup.rerun_observability (blocked: shadow)"},
		"no plan file":                 {store.InputCase, ``, []string{"run.ended:", "decision:case-1"}, ""},
		"a plan file that is not JSON": {store.InputCase, `nope`, []string{"run.ended:", "decision:case-1"}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			w := newShadowWorld(t)
			before := w.dump()
			sj := &spec.Routine{Fleet: "ops", Routine: "rerun"}
			out := &sched.CaseOutcome{}
			if c.plan != "" {
				out.Plan = json.RawMessage(c.plan)
			}
			if err := ShadowDone(ctx, w.st, sj, shadowRun(store.StatusOK), "h2", inputOf(c.kind, handed), out); err != nil {
				t.Fatal(err)
			}
			w.sameAsBefore(before)
			recs, _ := w.st.ShadowRecords(ctx, store.ShadowRecordFilter{InputKey: "live-1"})
			if got := kindsOf(recs); got != strings.Join(c.want, " ") {
				t.Fatalf("records %q, want %q", got, strings.Join(c.want, " "))
			}
			for _, r := range recs {
				if r.Kind != RecWouldRun {
					continue
				}
				var p map[string]any
				json.Unmarshal(r.Payload, &p)
				if p["blocked"] != "shadow" || p["text"] != c.wouldRun || p["action"] != "backup.rerun_observability" {
					t.Fatalf("would_run: %s", r.Payload)
				}
			}
		})
	}
}

func TestAFailedShadowRunRecordsItsEndingOnly(t *testing.T) {
	w := newShadowWorld(t)
	ctx := context.Background()
	before := w.dump()
	run := shadowRun(store.StatusError)
	run.Detail = "boom"
	sj := &spec.Routine{Fleet: "ops", Routine: "backup-monitor", Detects: []string{"backup"}}
	if err := ShadowDone(ctx, w.st, sj, run, "h2", inputOf(store.InputTick, "{}"), plan(`{"outcome":"detections","detections":[{"kind":"backup","target":"host:z","detail":"x"}]}`)); err != nil {
		t.Fatal(err)
	}
	w.sameAsBefore(before)
	recs, _ := w.st.ShadowRecords(ctx, store.ShadowRecordFilter{})
	if len(recs) != 1 || recs[0].Kind != RecRunEnded || !strings.Contains(string(recs[0].Payload), `"status":"error"`) || !strings.Contains(string(recs[0].Payload), "boom") {
		t.Fatalf("%+v", recs)
	}
}

func TestAMonitorReportThatCannotBeReadIsARecordedRefusal(t *testing.T) {
	ctx := context.Background()
	sj := &spec.Routine{Fleet: "ops", Routine: "backup-monitor", Detects: []string{"backup"}}
	for name, out := range map[string]*sched.CaseOutcome{
		"no plan file": {}, "a plan error": {PlanError: "plan_file: not JSON: x"}, "not a detections report": plan(`{"outcome":"plan"}`),
	} {
		w := newShadowWorld(t)
		before := w.dump()
		if err := ShadowDone(ctx, w.st, sj, shadowRun(store.StatusOK), "h2", inputOf(store.InputTick, "{}"), out); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		w.sameAsBefore(before)
		recs, _ := w.st.ShadowRecords(ctx, store.ShadowRecordFilter{Kind: RecReportBad})
		if len(recs) != 1 {
			t.Errorf("%s: %s", name, kindsOf(recs))
		}
	}
}

// The live path reads a report as it always did: PlanReport is Detected's own reading, so the table of what it refuses is pinned here.
func TestPlanReportRefusesWhatDetectedRefuses(t *testing.T) {
	detects := []string{"backup"}
	if _, why := PlanReport(detects, json.RawMessage(`{"outcome":"plan"}`)); why == "" {
		t.Error("a plan is not a detections report")
	}
	if _, why := PlanReport(detects, json.RawMessage(`[`)); why == "" {
		t.Error("not JSON")
	}
	many := `{"outcome":"detections","detections":[` + strings.Repeat(`{"kind":"backup","target":"host:a"},`, 50) + `{"kind":"backup","target":"host:a"}]}`
	if _, why := PlanReport(detects, json.RawMessage(many)); !strings.Contains(why, "more than 50") {
		t.Errorf("over the cap: %q", why)
	}
	rp, why := PlanReport(detects, json.RawMessage(`{"outcome":"detections","detections":[{"kind":"backup","target":"host:a","data":[1]},{"kind":"backup","target":"BAD"},{"kind":"other","target":"host:b"}]}`))
	if why != "" || len(rp.Items) != 1 || !rp.Items[0].DataDropped || len(rp.Refused) != 2 || !rp.Present["target=backup/host:a"] {
		t.Fatalf("%+v %q", rp, why)
	}
	if why, silent := ReportRefusal(store.StatusError, nil); why != "" || !silent {
		t.Error("a failed run's report is silently not a finding")
	}
	if why, _ := ReportRefusal(store.StatusOK, &sched.CaseOutcome{}); why != "no plan file" {
		t.Errorf("%q", why)
	}
}

// The redirect's guard: the shadow path (this package's shadow.go, the scheduler's and the API's shadow files) never names a function
// that changes the live system. A new call of one is a failed test, not a review comment.
var liveWrites = regexp.MustCompile(`\.(DecidePlan|MoveCase|StageCase|LeaseCase|ReleaseCase|AddCaseSpend|AddCaseEvent|OpenCase|DetectIncident|ClearIncidents|` +
	`ApplyPlan|SaveBrief|AskInSlack|StartHostExecution|EndHostExecution|ActionRecord|LastSuccess|Freshness|SetControl|SetControlBecause|ProposeNote|` +
	`Event|AddSummary|ImportLedger|WriteFiling|AppendEntry|Promote|Rollback|Retire)\(`)

func offending(src string) []string {
	var out []string
	for i, ln := range strings.Split(src, "\n") {
		if strings.HasPrefix(strings.TrimSpace(ln), "//") {
			continue
		}
		if m := liveWrites.FindString(ln); m != "" {
			out = append(out, strings.TrimSpace(ln)+" (line "+lineNo(i+1)+")")
		}
	}
	return out
}

func lineNo(n int) string { b, _ := json.Marshal(n); return string(b) }

func TestTheShadowPathCallsNoLiveWrite(t *testing.T) {
	for _, f := range []string{"shadow.go", filepath.Join("..", "sched", "shadow.go")} {
		b, err := os.ReadFile(f)
		if os.IsNotExist(err) {
			continue // the scheduler's shadow file comes with the scheduler's shadow runs
		}
		if err != nil {
			t.Fatal(err)
		}
		if bad := offending(string(b)); len(bad) > 0 {
			t.Errorf("%s calls a function that changes the live system: %s", f, strings.Join(bad, "; "))
		}
	}
	// the guard itself: a shadow file that calls one is caught
	if len(offending("func x() {\n\td.API.Store.MoveCase(ctx)\n\t// MoveCase( in a comment is fine\n}\n")) != 1 || len(offending("st.RecordShadow(ctx, r)")) != 0 {
		t.Fatal("the guard does not tell a live write from a shadow record")
	}
}
