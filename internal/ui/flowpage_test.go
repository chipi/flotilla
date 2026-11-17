package ui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chipi/flotilla/internal/spec"
	"github.com/chipi/flotilla/internal/store"
)

// Synthetic routines, runs, commands and ids only: flotilla is a public repo.

// pageNow is the clock these pages are rendered at, so "today" never depends on when the test runs.
var pageNow = time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)

func flowFixture(t *testing.T) *fixture {
	t.Helper()
	triage := &spec.Routine{Fleet: "triage", Routine: "cycle", Description: "one pass", Enabled: true, Worker: "triage", Autonomy: "shadow",
		Run: spec.Run{Command: "python3 orchestrator.py --cycle", Timeout: "20m"}, Every: 10 * time.Minute, Limit: time.Minute, Fresh: 35 * time.Minute,
		Trigger: spec.Trigger{Every: "10m"}, Freshness: "35m", Budget: spec.Budget{DayUSD: 1},
		Harness: &spec.Harness{Protocol: "json", Tools: []string{"event_detail"}, Gates: []string{"edge"},
			Limits: spec.HarnessLimits{MaxToolCalls: 3, MaxSteps: 4, ToolCallsPerTurn: 1}, Quirks: spec.HarnessQuirks{FinalDecisionCall: true}},
		Explain: map[string]string{"harness.protocol": "A model answers in JSON."}}
	fix := &spec.Routine{Fleet: "bugfix", Routine: "fix", Description: "fixes", Enabled: true, Worker: "bugfix", Autonomy: "shadow",
		Run: spec.Run{Command: "python3 fix.py", Timeout: "3h"}, Limit: time.Hour, Trigger: spec.Trigger{Case: &spec.CaseTrigger{Kind: "code_bug"}},
		Budget: spec.Budget{DayUSD: 1}}
	f := setupWith(t, []*spec.Routine{triage, fix})
	f.sch.Now = func() time.Time { return pageNow }
	return f
}

func startRun(t *testing.T, f *fixture, id, routine string, at time.Time) {
	t.Helper()
	if err := f.st.StartRun(context.Background(), store.Run{ID: id, Routine: routine, SpecHash: "h", Trigger: "schedule", Actor: "flotillad",
		Autonomy: "shadow", StartedAt: at.UTC().Format(store.TimeFormat), Status: "running"}); err != nil {
		t.Fatal(err)
	}
}

func record(t *testing.T, f *fixture, calls []store.ModelCall) {
	t.Helper()
	if err := f.st.RecordCalls(context.Background(), calls); err != nil {
		t.Fatal(err)
	}
}

func decided(run, routine, at, key string, step int64, cost *float64) store.ModelCall {
	return store.ModelCall{RunID: run, Routine: routine, At: at, Alias: "tri-pro", Model: "prov/tri-pro", Endpoint: "gw", Deployment: "dep0123456789abcdef",
		TokensIn: i64(1500), TokensOut: i64(300), CostUSD: cost, CostSource: "gateway", LatencyS: f64(4.2), FinishReason: "stop", Step: &step, DecisionKey: key}
}

func TestRoutinePageInstrumentsOnThinHistory(t *testing.T) {
	f := flowFixture(t)
	startRun(t, f, "run-t1", "triage/cycle", pageNow.Add(-time.Hour))
	at := func(m int) string { return pageNow.Add(-time.Duration(m) * time.Minute).Format(store.TimeFormat) }
	record(t, f, []store.ModelCall{
		decided("run-t1", "triage/cycle", at(30), "fp1", 1, f64(0.002)),
		decided("run-t1", "triage/cycle", at(29), "fp1", 2, f64(0.002)),
		decided("run-t1", "triage/cycle", at(20), "fp2", 1, f64(0.002)),
		// a model-contract check and a harness step row share the routine id: neither is the routine's work
		{RunID: "run-t1", Routine: "triage/cycle", At: at(10), Alias: "tri-pro", Model: "m", Check: "json", CostUSD: f64(5)},
		{RunID: "run-t1", Routine: "triage/cycle", At: at(9), Detail: "pin: x", Role: "harness", CostUSD: f64(7)},
	})
	_, page := f.do("GET", "/fleets/triage/cycle", f.signin(f.vic), nil, nil)
	for _, want := range []string{
		`id="inst-h">Instruments`, "Spend today", "$0.006 of $1.00", "Cost per decision", "$0.0030",
		"baseline: 1 day", "no colour until 5 days of history exist", `class="gauge__fill gw-1"`,
		"Turns per decision", "1.5 of 4", "4 max", "Longest reply", "Quality", "no eval result can be matched",
		`node__nums node__nums--cost`, "3 calls today", "1.5 turns a decision",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the routine page lacks %q", want)
		}
	}
	for _, bad := range []string{"irow--alert", "irow--bad", "$5.", "$12", "style="} {
		if strings.Contains(page, bad) {
			t.Errorf("the routine page has %q: colour on a thin history, a check's cost, or an inline style (the CSP forbids it)", bad)
		}
	}
}

func TestRoutinePageColourWithHistoryAndPanel(t *testing.T) {
	f := flowFixture(t)
	var calls []store.ModelCall
	for d := 0; d < 7; d++ {
		id := fmt.Sprintf("run-h%d", d)
		day := pageNow.AddDate(0, 0, -d)
		startRun(t, f, id, "triage/cycle", day.Add(-time.Hour))
		cost := 0.002
		if d == 0 {
			cost = 0.2 // today: 100 times the usual
		}
		calls = append(calls, decided(id, "triage/cycle", day.Add(-30*time.Minute).Format(store.TimeFormat), "fp-"+id, 1, f64(cost)))
	}
	record(t, f, calls)
	ck := f.signin(f.vic)
	_, page := f.do("GET", "/fleets/triage/cycle?b=harness.protocol", ck, nil, nil)
	for _, want := range []string{
		`irow irow--alert`, "worth a look", "gauge__band", "Usual $0.0020",
		"What it cost", "its usual", "Model and where it runs", "tri-pro", "prov/tri-pro", "the gateway, every call", "dep012345678…",
		"What it sees", "prompt_ver", "not recorded", "Tools it called", "Last calls",
		"up to 4, then one final decision call",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the routine page lacks %q", want)
		}
	}
	if strings.Contains(page, "baseline: ") {
		t.Error("seven days of history is a baseline; no thin-history note")
	}
	_, other := f.do("GET", "/fleets/triage/cycle?b=gate.edge", ck, nil, nil)
	if strings.Contains(other, "What it cost") {
		t.Error("only the model block opens the cost panel")
	}
}

func TestRoutinePageWithoutCallsIsUnchanged(t *testing.T) {
	f := flowFixture(t)
	_, page := f.do("GET", "/fleets/bugfix/fix", f.signin(f.vic), nil, nil)
	for _, bad := range []string{"Instruments", "The paths its runs take", "node__nums--cost"} {
		if strings.Contains(page, bad) {
			t.Errorf("a routine with no model call shows %q", bad)
		}
	}
}

// bugfixFlow is one run: a verdict, a coder that edits and is refused by the gates, an advisor's pin, then the
// verdict again, cut off while it waited on a tool.
func bugfixFlow(run string, base time.Time, withCost bool, coderTurns int) []store.ModelCall {
	cost := func(v float64) *float64 {
		if !withCost {
			return nil
		}
		return &v
	}
	at := func(s int) string { return base.Add(time.Duration(s) * time.Second).UTC().Format(store.TimeFormat) }
	mk := func(s int, role, ep string, turn int64, c *float64, finish, tools string) store.ModelCall {
		return store.ModelCall{RunID: run, Routine: "bugfix/fix", At: at(s), Alias: "fix-flash", Model: "prov/flash", Role: role, Episode: ep, Step: &turn,
			CostUSD: c, CostSource: "gateway-share", FinishReason: finish, Tools: tools, TokensIn: i64(2000), TokensOut: i64(250), LatencyS: f64(3)}
	}
	out := []store.ModelCall{
		mk(0, "verdict", "e1", 1, cost(0.01), "tool_calls", `[{"name":"bash","arg":"ls src","ok":true}]`),
		mk(30, "verdict", "e1", 2, cost(0.01), "stop", ""),
	}
	for i := 1; i <= coderTurns; i++ {
		tools := fmt.Sprintf(`[{"name":"read","arg":"mod%d.ts","ok":true}]`, i)
		switch {
		case i == 2:
			tools = `[{"name":"bash","arg":"grep -n nothing mod1.ts","ok":false}]`
		case i == coderTurns-1:
			tools = `[{"name":"edit","arg":"package.json","ok":true}]`
		case i == coderTurns:
			tools = ""
		}
		fin := "tool_calls"
		if i == coderTurns {
			fin = "stop"
		}
		out = append(out, mk(60+i*5, "coder", "e2", int64(i), cost(0.01), fin, tools))
	}
	t3 := 60 + coderTurns*5 + 30
	out = append(out,
		mk(t3, "advisor", "e3", 1, cost(0.02), "stop", `["read"]`),
		mk(t3+40, "verdict", "e4", 1, cost(0.01), "tool_calls", `[{"name":"bash","arg":"cat mod1.ts"}]`),
		store.ModelCall{RunID: run, Routine: "bugfix/fix", At: at(t3 - 10), Role: "gates", Detail: "refused: out of scope (package.json)"},
		store.ModelCall{RunID: run, Routine: "bugfix/fix", At: at(t3 + 5), Role: "advisor", Detail: "pin: messages/en-US.json"})
	return out
}

func TestRunPageSwimlane(t *testing.T) {
	f := flowFixture(t)
	base := pageNow.Add(-time.Hour)
	startRun(t, f, "run-fx1", "bugfix/fix", base)
	record(t, f, bugfixFlow("run-fx1", base, true, 14))
	_, page := f.do("GET", "/runs/run-fx1", f.signin(f.vic), nil, nil)
	for _, want := range []string{
		"In short:", "4 episodes (verdict, coder, advisor)", "The gates refused a round: out of scope", "The verdict episode was cut off",
		`id="inst-h">Instruments`, "How it ended", "cut off",
		`class="swim"`, `The run, role by role`,
		">the harness</text>", ">verdict</text>", ">coder</text>", ">advisor</text>", ">gates</text>", // a lane for the harness, each role, the gates
		"tools the agents called", ">bash</text>", ">read</text>", ">edit</text>", // a row per tool name
		"swim__call swim__call--err", // the failed grep
		"swim__call swim__call--edit",
		"swim__ep swim__ep--cut", // the episode that did not end with a stop
		"swim__hand", "marker-end", "refused: out of scope (package.json)",
		`href="/runs/run-fx1?ep=2#turns"`, "In order", "Episodes", "verdict (2nd)",
		`<th scope="col">Turn</th>`, `aria-label="Filter by role"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the run page lacks %q", want)
		}
	}
	if strings.Contains(page, "style=") {
		t.Error("the run page has an inline style: the CSP forbids it")
	}
	if strings.Contains(page, "no cost recorded") {
		t.Error("every call had a cost")
	}
}

func TestRunPageOpenEpisodeFoldsTheMiddle(t *testing.T) {
	f := flowFixture(t)
	base := pageNow.Add(-time.Hour)
	startRun(t, f, "run-fx2", "bugfix/fix", base)
	record(t, f, bugfixFlow("run-fx2", base, true, 30))
	ck := f.signin(f.vic)
	_, page := f.do("GET", "/runs/run-fx2?ep=2", ck, nil, nil)
	for _, want := range []string{
		`id="turns"`, "2 · coder: its turns", "Its first edit came at turn 29", "before it, 28 turns", "1 tool call failed", "It answered at turn 30",
		`<span class="chip chip--small chip--stop">failed</span>`, "grep -n nothing mod1.ts", "package.json",
		"show turns 7–27", `href="/runs/run-fx2?ep=2&amp;t=7-27#turns"`, "show all turns", "swim__ep swim__ep--sel",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the opened episode lacks %q", want)
		}
	}
	if strings.Contains(page, "<code>mod15.ts</code>") {
		t.Error("a folded turn is on the page")
	}
	_, open := f.do("GET", "/runs/run-fx2?ep=2&t=7-27", ck, nil, nil)
	if !strings.Contains(open, "<code>mod15.ts</code>") || strings.Contains(open, "show turns 7–27") || !strings.Contains(open, "ttab--open") {
		t.Error("an opened stretch lists its turns and is no longer folded")
	}
	_, all := f.do("GET", "/runs/run-fx2?ep=2&all=1", ck, nil, nil)
	if strings.Contains(all, "show turns") || !strings.Contains(all, "<code>mod20.ts</code>") {
		t.Error("all=1 lists every turn")
	}
	_, filtered := f.do("GET", "/runs/run-fx2?role=advisor", ck, nil, nil)
	if strings.Contains(filtered, `<td data-label="Role" class="exact">coder</td>`) || !strings.Contains(filtered, `<td data-label="Role" class="exact">advisor</td>`) {
		t.Error("?role= filters the calls table")
	}
}

func TestRunPageNamesMissingCostAsGap(t *testing.T) {
	f := flowFixture(t)
	base := pageNow.Add(-time.Hour)
	startRun(t, f, "run-fx3", "bugfix/fix", base)
	record(t, f, bugfixFlow("run-fx3", base, false, 6))
	_, page := f.do("GET", "/runs/run-fx3", f.signin(f.vic), nil, nil)
	if !strings.Contains(page, "no cost recorded") || !strings.Contains(page, "no call recorded a cost") {
		t.Error("calls with no cost say so in place")
	}
	if strings.Contains(page, "$0.000 of") {
		t.Error("a null cost is never drawn as $0 against the budget")
	}
}

func TestRunPageWithoutRolesKeepsItsOldShape(t *testing.T) {
	f := flowFixture(t)
	base := pageNow.Add(-time.Hour)
	startRun(t, f, "run-old", "triage/cycle", base)
	record(t, f, []store.ModelCall{decided("run-old", "triage/cycle", base.Format(store.TimeFormat), "fp", 1, f64(0.001))})
	_, page := f.do("GET", "/runs/run-old", f.signin(f.vic), nil, nil)
	for _, bad := range []string{`class="swim"`, "In short:", "The flow, role by role"} {
		if strings.Contains(page, bad) {
			t.Errorf("a run with no role shows %q", bad)
		}
	}
	if !strings.Contains(page, `<th scope="col">Step</th>`) {
		t.Error("the calls table is as it was")
	}
}

func TestRoutinePagePaths(t *testing.T) {
	f := flowFixture(t)
	base := pageNow.Add(-3 * time.Hour)
	for i := 0; i < 3; i++ {
		id := fmt.Sprintf("run-p%d", i)
		b := base.Add(time.Duration(i) * 20 * time.Minute)
		startRun(t, f, id, "bugfix/fix", b)
		record(t, f, bugfixFlow(id, b, i != 2, 6)) // the third run reported no cost
	}
	_, page := f.do("GET", "/fleets/bugfix/fix", f.signin(f.vic), nil, nil)
	for _, want := range []string{
		`id="paths-h">The paths its runs take`, "pstep pstep--agent", "pstep pstep--bad", "r0 out of scope", "3 of 3", "100% of the runs",
		"No run of 3 needed more than one round", "The most common first failure was out of scope (3 runs)",
		"cost per path: $", "over the 2 of 3 runs that reported one", `href="/runs/run-p0"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the paths section lacks %q", want)
		}
	}
	if strings.Contains(page, "style=") {
		t.Error("an inline style on the routine page")
	}
}

// TestFlowSnapshots writes the instruments, paths and run-flow pages from synthetic data as static HTML for a
// browser check at phone and laptop widths. Off unless UI_SNAPSHOT_DIR is set.
func TestFlowSnapshots(t *testing.T) {
	dir := os.Getenv("UI_SNAPSHOT_DIR")
	if dir == "" {
		t.Skip("UI_SNAPSHOT_DIR not set")
	}
	f := flowFixture(t)
	var calls []store.ModelCall
	for d := 0; d < 7; d++ {
		id := fmt.Sprintf("run-h%d", d)
		day := pageNow.AddDate(0, 0, -d)
		startRun(t, f, id, "triage/cycle", day.Add(-time.Hour))
		cost := 0.002
		if d == 0 {
			cost = 0.2
		}
		calls = append(calls, decided(id, "triage/cycle", day.Add(-30*time.Minute).Format(store.TimeFormat), "fp-"+id, 1, f64(cost)))
	}
	record(t, f, calls)
	for i := 0; i < 3; i++ {
		id := fmt.Sprintf("run-p%d", i)
		b := pageNow.Add(-3*time.Hour + time.Duration(i)*20*time.Minute)
		startRun(t, f, id, "bugfix/fix", b)
		record(t, f, bugfixFlow(id, b, i != 2, 30))
	}
	ck := f.signin(f.vic)
	if err := os.MkdirAll(filepath.Join(dir, "fonts"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, p := range map[string]string{"flow-routine": "/fleets/triage/cycle", "flow-routine-panel": "/fleets/triage/cycle?b=harness.protocol",
		"flow-paths": "/fleets/bugfix/fix", "flow-run": "/runs/run-p0", "flow-run-ep": "/runs/run-p0?ep=2"} {
		resp, body := f.do("GET", p, ck, nil, nil)
		if resp.StatusCode != 200 {
			t.Fatalf("%s: %d", p, resp.StatusCode)
		}
		body = strings.NewReplacer(`href="/static/flotilla.css"`, `href="flotilla.css"`, `href="/static/favicon.svg"`, `href="favicon.svg"`,
			`src="/static/flotilla.js"`, `src="flotilla.js"`).Replace(body)
		if err := os.WriteFile(filepath.Join(dir, name+".html"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	fonts, _ := Static.ReadDir("static/fonts")
	for _, e := range fonts {
		b, _ := Static.ReadFile("static/fonts/" + e.Name())
		_ = os.WriteFile(filepath.Join(dir, "fonts", e.Name()), b, 0o644)
	}
	for _, name := range []string{"favicon.svg", "flotilla.css", "flotilla.js"} {
		b, _ := Static.ReadFile("static/" + name)
		_ = os.WriteFile(filepath.Join(dir, name), b, 0o644)
	}
}
