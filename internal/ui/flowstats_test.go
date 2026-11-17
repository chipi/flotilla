package ui

import (
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/chipi/flotilla/internal/store"
)

// On the day decision keys began, the earlier calls carry none: cost per decision divides the day's cost by the
// ledger's decisions, not by the few keyed ones (a real copy of the store showed $0.24 for "1 decision, 58 calls").
func TestDecisionsOnTheDayKeysBegan(t *testing.T) {
	day := "2026-11-17"
	var calls []store.ModelCall
	for i := 0; i < 8; i++ {
		c := store.ModelCall{At: day + "T08:0" + fmt.Sprint(i) + ":00Z", Model: "m", CostUSD: f64(0.01)}
		if i >= 6 {
			c.DecisionKey = "fp-late"
		}
		calls = append(calls, c)
	}
	d := dayStatsOf(calls, map[string]int{day: 4})[day]
	if d.Decisions != 4 || d.DecisionCalls != 8 {
		t.Errorf("mixed day: %d decisions over %d calls, want the ledger's 4 over all 8", d.Decisions, d.DecisionCalls)
	}
	for i := range calls {
		calls[i].DecisionKey = fmt.Sprintf("fp-%d", i/2)
	}
	if d := dayStatsOf(calls, map[string]int{day: 9})[day]; d.Decisions != 4 || d.DecisionCalls != 8 {
		t.Errorf("all keyed: %d decisions, want the 4 distinct keys", d.Decisions)
	}
}

func TestMedianAndPercentile(t *testing.T) {
	if got := median([]float64{5, 1, 3}); got != 3 {
		t.Errorf("median of 1,3,5 = %v", got)
	}
	if got := median([]float64{1, 2, 3, 4}); got != 2.5 {
		t.Errorf("median of 1..4 = %v, want 2.5", got)
	}
	if got := percentile([]float64{10, 20, 30, 40, 50}, 10); math.Abs(got-14) > 1e-9 {
		t.Errorf("P10 = %v, want 14 (interpolated)", got)
	}
	if got := percentile([]float64{10, 20, 30, 40, 50}, 90); math.Abs(got-46) > 1e-9 {
		t.Errorf("P90 = %v, want 46", got)
	}
	if median(nil) != 0 || percentile([]float64{7}, 90) != 7 {
		t.Error("an empty list is 0 and a single value is itself")
	}
}

// Below five days of history there is no band to be outside of: no colour (ISA-101: a guess is not a signal).
func TestUsualNeedsFiveDays(t *testing.T) {
	thin := usualOf([]float64{1, 1, 1, 1})
	if thin.Enough || thin.Outside(100) {
		t.Errorf("4 days must not draw a band: %+v", thin)
	}
	full := usualOf([]float64{1, 1.1, 0.9, 1, 1.05})
	if !full.Enough {
		t.Fatalf("5 days make a band: %+v", full)
	}
	if full.Outside(1.0) || !full.Outside(5) || !full.Outside(0.1) {
		t.Errorf("inside the band is normal, beyond P10 and P90 is not: %+v", full)
	}
}

func TestToolsParseBothShapes(t *testing.T) {
	old := parseTools(`["bash","read"]`)
	if len(old) != 2 || old[0].Name != "bash" || old[0].Arg != "" || old[0].OK != nil {
		t.Errorf("old shape: %+v", old)
	}
	nw := parseTools(`[{"name":"bash","arg":"grep -n x f.ts","ok":true},{"name":"edit","arg":"a.ts","ok":false},{"name":"read"}]`)
	if len(nw) != 3 || nw[0].Arg != "grep -n x f.ts" || nw[0].OK == nil || !*nw[0].OK || !nw[1].Failed() || nw[2].OK != nil {
		t.Errorf("new shape: %+v", nw)
	}
	if !nw[1].WritesFile() || nw[0].WritesFile() {
		t.Error("edit changes a file, bash does not")
	}
	mixed := parseTools(`["bash",{"name":"read","arg":"f"},7,{"arg":"no name"}]`)
	if len(mixed) != 2 {
		t.Errorf("a mix is read and what is unreadable skipped: %+v", mixed)
	}
	if parseTools("") != nil || parseTools("not json") != nil {
		t.Error("nothing or garbage is no tools, never a panic")
	}
	if got := toolNames(`[{"name":"bash","arg":"x"},{"name":"read"}]`); got != "bash, read" {
		t.Errorf("toolNames reads the new shape: %q", got)
	}
	if got := toolCounts(parseTools(`["bash","bash","read"]`)); got != "bash 2, read 1" {
		t.Errorf("toolCounts = %q", got)
	}
}

func call(run, at, role, ep string, step int64, cost *float64, finish, tools string) store.ModelCall {
	c := store.ModelCall{RunID: run, Routine: "t/r", At: at, Alias: "m-a", Model: "prov/m", Role: role, Episode: ep, Step: &step,
		CostUSD: cost, FinishReason: finish, Tools: tools, TokensIn: i64(1000), TokensOut: i64(100)}
	return c
}

func TestDayStatsDecisionsAndGaps(t *testing.T) {
	calls := []store.ModelCall{
		{At: "2026-11-16T10:00:00Z", CostUSD: f64(0.01), DecisionKey: "fp1", TokensOut: i64(500), FinishReason: "stop"},
		{At: "2026-11-16T10:01:00Z", CostUSD: f64(0.03), DecisionKey: "fp1", TokensOut: i64(900), FinishReason: "length"},
		{At: "2026-11-16T11:00:00Z", CostUSD: nil, DecisionKey: "fp2"},
		{At: "2026-11-17T09:00:00Z", CostUSD: nil},
	}
	days := dayStatsOf(calls, map[string]int{"2026-11-17": 4})
	d16, d17 := days["2026-11-16"], days["2026-11-17"]
	if d16.Calls != 3 || d16.Priced != 2 || d16.Decisions != 2 || d16.Cut != 1 || d16.MaxOut != 900 {
		t.Errorf("day 16: %+v", d16)
	}
	if v, ok := d16.costPerDecision(); !ok || math.Abs(v-0.02) > 1e-9 {
		t.Errorf("cost per decision = %v %v, want 0.02 (priced calls over decisions)", v, ok)
	}
	if _, ok := d17.costPerDecision(); ok {
		t.Error("a day whose calls reported no cost has no cost per decision: a gap, never 0")
	}
	if d17.Decisions != 4 {
		t.Errorf("with no decision key the ledger's count stands: %d", d17.Decisions)
	}
	if got := focusDay(days, "2026-11-17"); got.Day != "2026-11-17" {
		t.Error("today is the focus when it has calls")
	}
	if got := focusDay(days, "2026-11-20"); got.Day != "2026-11-17" {
		t.Errorf("else the last active day: %s", got.Day)
	}
}

func daysWith(n int, cpd float64) map[string]*dayStat {
	out := map[string]*dayStat{}
	for i := 0; i < n; i++ {
		day := fmt.Sprintf("2026-11-%02d", 10+i)
		out[day] = &dayStat{Day: day, Calls: 4, Cost: cpd * 2, Priced: 4, Decisions: 2, DecisionCalls: 4, In: 4000, Out: 400, MaxOut: 300}
	}
	return out
}

func rowOf(in instruments, label string) *instRow {
	for i := range in.Rows {
		if strings.HasPrefix(in.Rows[i].Label, label) {
			return &in.Rows[i]
		}
	}
	return nil
}

func TestInstrumentsColourSuppressedOnThinHistory(t *testing.T) {
	days := daysWith(2, 0.5)
	days["2026-11-11"].Cost = 50 // would be far outside any band
	inst, ok := buildInstruments(instInput{Days: days, Today: "2026-11-11", Window: 14, DayBudget: 0})
	if !ok {
		t.Fatal("no card")
	}
	r := rowOf(inst, "Cost per decision")
	if r == nil || r.Level != "" || !strings.Contains(r.Gap, "baseline: 2 days") || r.G.HasFill {
		t.Errorf("a thin history draws no colour and says how thin: %+v", r)
	}
	if !strings.Contains(inst.Rest, "too short") {
		t.Errorf("the sentence names the thin history: %q", inst.Rest)
	}
}

func TestInstrumentsAmberAndRed(t *testing.T) {
	days := daysWith(6, 0.5)
	days["2026-11-15"].Cost = 40 // today: cost per decision 20, far above P90 of ~0.5
	days["2026-11-15"].Cut = 1
	inst, _ := buildInstruments(instInput{Days: days, Today: "2026-11-15", Window: 14, DayBudget: 100, MaxSteps: 4})
	if r := rowOf(inst, "Cost per decision"); r.Level != "alert" || !r.G.Band {
		t.Errorf("outside the P10-P90 band is amber, with the band drawn: %+v", r)
	}
	if r := rowOf(inst, "Spend"); r.Level != "" {
		t.Errorf("40%% of the budget is grey: %+v", r)
	}
	if r := rowOf(inst, "Longest reply"); r.Level != "bad" {
		t.Errorf("a reply cut at the cap is a crossed limit: %+v", r)
	}
	if !strings.HasPrefix(inst.Lead, "Past a limit") {
		t.Errorf("lead: %q", inst.Lead)
	}

	days = daysWith(6, 0.5)
	days["2026-11-15"].Cost = 75 // 75% of $100
	inst, _ = buildInstruments(instInput{Days: days, Today: "2026-11-15", Window: 14, DayBudget: 100, MaxSteps: 4})
	if r := rowOf(inst, "Spend"); r.Level != "alert" {
		t.Errorf("past 70%% of a limit is amber: %+v", r)
	}
	days["2026-11-15"].Cost = 120
	inst, _ = buildInstruments(instInput{Days: days, Today: "2026-11-15", Window: 14, DayBudget: 100})
	if r := rowOf(inst, "Spend"); r.Level != "bad" || r.G.Fill != 100 {
		t.Errorf("over the budget is red and the bar is full: %+v", r)
	}
	if r := rowOf(inst, "Turns per decision"); r != nil {
		t.Error("no turn limit in the spec, no turns row")
	}
}

func TestInstrumentsGapsNamedInPlace(t *testing.T) {
	days := map[string]*dayStat{"2026-11-15": {Day: "2026-11-15", Calls: 3, Decisions: 0, DecisionCalls: 3}}
	inst, _ := buildInstruments(instInput{Days: days, Today: "2026-11-15", Window: 14, DayBudget: 1, EvalGap: "no eval result for prompt abc"})
	if r := rowOf(inst, "Spend"); r.Val != "–" || !strings.Contains(r.Gap, "cost not reported") {
		t.Errorf("a null cost is a gap, never $0: %+v", r)
	}
	if rowOf(inst, "Cost per decision") != nil {
		t.Error("no decision, no cost per decision")
	}
	if r := rowOf(inst, "Quality"); r == nil || r.Gap != "no eval result for prompt abc" {
		t.Errorf("quality names its gap: %+v", r)
	}
}

func TestQualityPreviousResultIsTheTick(t *testing.T) {
	days := daysWith(1, 0.1)
	prev := 0.5
	inst, _ := buildInstruments(instInput{Days: days, Today: "2026-11-10", Window: 14,
		Evals: []evalView{{Metric: "triage_cost", Value: 0.45, N: 65, Repeats: 3, SHA: "d99c9ab61234", Prev: &prev}}})
	r := rowOf(inst, "Quality")
	if r == nil || r.Val != "0.45" || len(r.G.Ticks) != 1 || !strings.Contains(r.G.Ticks[0].Label, "0.5 before") || r.Level != "" {
		t.Errorf("quality is grey with the earlier result as its tick: %+v", r)
	}
}

func TestStepClassification(t *testing.T) {
	cur := -1
	cases := []struct {
		detail, label, tone string
	}{
		{"refused: out of scope (a.json, b.json)", "r0 out of scope", "bad"},
		{"pin: messages/x.json", "pin", ""},
		{"round 1: tests fail", "r1 tests fail", "bad"},
		{"round 2: gates clean", "r2 clean", ""},
		{"review: approve", "approve", "ok"},
		{"review: request-changes", "changes", "warn"},
		// the wording bugfix/fix.py's step_row writes (f89b0fa)
		{"round 0: out of scope: the diff touches package-lock.json, package.json", "r0 out of scope", "bad"},
		{"round 1: the fix's own tests fail with the fix: a.test.ts::x", "r1 the fix's own tests fail with the fix", "bad"},
		{"round 2: gates clean (2 files, 53 turns, 11160 output tokens): 1 test files fail on the base", "r2 clean", ""},
		{"verdict: actionable", "verdict: actionable", ""},
	}
	for _, c := range cases {
		s := classifyStep("gates", c.detail, &cur)
		if s.Label != c.label || s.Tone != c.tone {
			t.Errorf("%q -> %q/%q, want %q/%q", c.detail, s.Label, s.Tone, c.label, c.tone)
		}
	}
}

func at(base time.Time, sec int) string {
	return base.Add(time.Duration(sec) * time.Second).UTC().Format(store.TimeFormat)
}

func stepRow(run, at, role, detail string) store.ModelCall {
	return store.ModelCall{RunID: run, Routine: "t/r", At: at, Role: role, Detail: detail}
}

// stepCall is a harness step: no alias, no model; Detail is the outcome text.
func fixtureCalls(run string, base time.Time) []store.ModelCall {
	c := []store.ModelCall{
		call(run, at(base, 0), "verdict", "e1", 1, f64(0.01), "tool_calls", `[{"name":"bash","arg":"ls src","ok":true}]`),
		call(run, at(base, 20), "verdict", "e1", 2, f64(0.01), "stop", ""),
		call(run, at(base, 60), "coder", "e2", 1, f64(0.05), "tool_calls", `[{"name":"read","arg":"a.ts"},{"name":"bash","arg":"grep zzz a.ts","ok":false}]`),
		call(run, at(base, 90), "coder", "e2", 2, f64(0.05), "tool_calls", `[{"name":"edit","arg":"a.ts","ok":true}]`),
		call(run, at(base, 120), "coder", "e2", 3, f64(0.05), "stop", ""),
		call(run, at(base, 200), "advisor", "e3", 1, f64(0.02), "stop", `[{"name":"read","arg":"b.ts"}]`),
		call(run, at(base, 260), "verdict", "e4", 1, f64(0.01), "tool_calls", `[{"name":"bash","arg":"cat c.ts"}]`),
	}
	c = append(c,
		stepRow(run, at(base, 150), "gates", "refused: out of scope (package.json)"),
		stepRow(run, at(base, 210), "advisor", "pin: messages/en-US.json"))
	return c
}

func TestBuildFlowEpisodesStepsAndCost(t *testing.T) {
	base := time.Date(2026, 11, 15, 19, 40, 0, 0, time.UTC)
	calls := fixtureCalls("r1", base)
	// a contract check call and a step row with a cost must not count
	calls = append(calls, store.ModelCall{RunID: "r1", At: at(base, 5), Alias: "m-a", Model: "x", Check: "json", CostUSD: f64(9)})
	calls = append(calls, store.ModelCall{RunID: "r1", At: at(base, 6), Detail: "x", Role: "harness", CostUSD: f64(7)})
	f := buildFlow(calls)
	if f == nil {
		t.Fatal("no flow")
	}
	if len(f.Eps) != 4 || f.Turns != 7 || f.ToolCalls != 6 {
		t.Fatalf("episodes %d turns %d tools %d", len(f.Eps), f.Turns, f.ToolCalls)
	}
	if f.Cost == nil || math.Abs(*f.Cost-0.20) > 1e-9 {
		t.Errorf("step rows and checks are not in the cost (want 0.20): %v", *f.Cost)
	}
	if got := f.Eps[3].Label; got != "verdict (2nd)" {
		t.Errorf("a role that comes back is numbered: %q", got)
	}
	if f.Eps[0].Cut || !f.Eps[3].Cut || f.Eps[3].Ended != "cut off" {
		t.Errorf("only the episode whose last call was not a stop is cut: %+v / %+v", f.Eps[0].Ended, f.Eps[3].Ended)
	}
	if f.Eps[1].FirstWrite != 2 || f.Eps[1].Failed != 1 {
		t.Errorf("coder: first write turn %d, failed %d", f.Eps[1].FirstWrite, f.Eps[1].Failed)
	}
	if len(f.Steps) != 3 {
		t.Errorf("steps: %d", len(f.Steps))
	}
	if got := strings.Join(f.Roles, ","); got != "verdict,coder,advisor" {
		t.Errorf("roles by first appearance: %s", got)
	}
	if buildFlow([]store.ModelCall{{Alias: "a", Model: "m"}}) != nil {
		t.Error("calls with no role keep the old page")
	}
}

func TestNullCostsStayAGap(t *testing.T) {
	base := time.Date(2026, 11, 15, 19, 40, 0, 0, time.UTC)
	f := buildFlow([]store.ModelCall{call("r", at(base, 0), "coder", "e", 1, nil, "stop", "")})
	if f.Cost != nil || f.Eps[0].Cost != nil {
		t.Error("no call reported a cost: the total is nil, not 0")
	}
	if txt, gap := costText(f.Cost); !gap || strings.Contains(txt, "$0") {
		t.Errorf("costText: %q %v", txt, gap)
	}
}

func TestBuildPathsGroupsAndInsight(t *testing.T) {
	base := time.Date(2026, 11, 15, 19, 40, 0, 0, time.UTC)
	mk := func(id string, steps ...string) *runFlow {
		calls := []store.ModelCall{call(id, at(base, 0), "verdict", "e1", 1, f64(0.01), "stop", ""), call(id, at(base, 30), "coder", "e2", 1, f64(0.02), "stop", "")}
		for i, s := range steps {
			role := "gates"
			if strings.HasPrefix(s, "review") {
				role = "reviewer"
			}
			calls = append(calls, stepRow(id, at(base, 40+i*5), role, s))
		}
		return buildFlow(calls)
	}
	flows := map[string]*runFlow{
		"a": mk("a", "round 0: gates clean", "review: approve"),
		"b": mk("b", "refused: out of scope (x)", "round 1: gates clean", "review: request-changes"),
		"c": mk("c", "refused: out of scope (y)", "round 1: gates clean", "review: request-changes"),
	}
	ps := buildPaths([]string{"a", "b", "c"}, flows)
	if ps.Runs != 3 || len(ps.Paths) != 2 {
		t.Fatalf("3 runs in 2 variants: %d %d", ps.Runs, len(ps.Paths))
	}
	if ps.Paths[0].Count != 2 || ps.Paths[0].Share != 66 {
		t.Errorf("the commoner variant leads with its share: %+v", ps.Paths[0])
	}
	var labels []string
	for _, s := range ps.Paths[0].Steps {
		labels = append(labels, s.Label)
	}
	if got := strings.Join(labels, " > "); got != "verdict > coder > r0 out of scope > r1 clean > changes" {
		t.Errorf("path: %s", got)
	}
	if ps.Paths[0].Steps[2].Tone != "bad" || ps.Paths[0].Steps[4].Tone != "warn" || ps.Paths[0].Steps[0].Tone != "agent" {
		t.Errorf("tones: %+v", ps.Paths[0].Steps)
	}
	if ps.Paths[0].Cost == nil || math.Abs(*ps.Paths[0].Cost-0.06) > 1e-9 || !strings.Contains(ps.Paths[0].CostText, "$0.060 in all") {
		t.Errorf("cost per path: %v %q", ps.Paths[0].Cost, ps.Paths[0].CostText)
	}
	for _, want := range []string{"2 of 3 runs needed more than one round", "out of scope (2 runs)", "1 of 3 got the reviewer's approval"} {
		if !strings.Contains(ps.Insight, want) {
			t.Errorf("insight %q lacks %q", ps.Insight, want)
		}
	}
	// no run reported a cost: the path says so, in place
	flows["a"].Cost = nil
	ps = buildPaths([]string{"a"}, map[string]*runFlow{"a": flows["a"]})
	if !ps.Paths[0].CostGap {
		t.Errorf("a path whose runs have no costs shows a gap: %+v", ps.Paths[0])
	}
}
