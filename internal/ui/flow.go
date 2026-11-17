package ui

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/chipi/flotilla/internal/store"
)

// A run's flow: its model calls grouped into episodes (one agent session each: a role, a model, a run of
// turns, the tools it called) and the harness's step rows (what the code decided between them). The run page
// draws it as a swimlane; the routine page sums the order of many runs into paths.

type flowEp struct {
	N                int
	Role, Label      string // Label is the role, numbered when the role came back
	Alias, Model     string
	Start, End       time.Time
	Calls            []store.ModelCall
	Tools            []toolCall
	In, Out          int64
	Cost             *float64 // nil when no call of the episode reported one: a gap, never 0
	Failed           int      // tool calls that recorded ok: false
	FirstWrite       int      // the turn of its first file-changing tool call, 0 when none
	Ended, EndedTone string   // answered | cut off | cut at the token cap | error | not recorded
	Cut              bool     // its last call did not end with finish "stop"
	CostSource       string
}

type flowStep struct {
	At                           time.Time
	Role, Detail                 string
	Label, Reason, Tone          string // Tone: "" | bad | ok | warn
	Round, Refused, Approve, Pin bool
}

type runFlow struct {
	Eps                      []flowEp
	Steps                    []flowStep
	Start, End               time.Time
	Roles                    []string // the roles of the episodes, in order of first appearance
	Turns, ToolCalls, Failed int
	Cost                     *float64
	Rounds                   int // rounds the code closed clean
}

var roundRe = regexp.MustCompile(`^round (\d+): (.*)$`)

// classifyStep reads a step row's outcome text. Rounds count from 0: the first refusal is round 0 unless a
// "round N" row said otherwise.
func classifyStep(role, detail string, cur *int) flowStep {
	d := strings.TrimSpace(detail)
	s := flowStep{Role: role, Detail: d}
	low := strings.ToLower(d)
	switch {
	case strings.HasPrefix(low, "refused:"):
		reason := strings.TrimSpace(d[len("refused:"):])
		if i := strings.Index(reason, " ("); i > 0 {
			reason = reason[:i]
		}
		*cur++
		s.Label, s.Reason, s.Tone, s.Round, s.Refused = fmt.Sprintf("r%d %s", *cur, reason), reason, "bad", true, true
	case roundRe.MatchString(d):
		m := roundRe.FindStringSubmatch(d)
		n, _ := strconv.Atoi(m[1])
		*cur = n
		rest := strings.TrimPrefix(strings.TrimSpace(m[2]), "gates ")
		// fix.py's round notes: "gates clean (…)" or the refusal, e.g. "out of scope: the diff touches …",
		// "the fix's own tests fail with the fix: …"; the label keeps the refusal's name, before its colon
		short := rest
		if i := strings.IndexAny(short, ":("); i > 0 {
			short = strings.TrimSpace(short[:i])
		}
		s.Label, s.Round = fmt.Sprintf("r%d %s", n, short), true
		if !strings.HasPrefix(rest, "clean") {
			s.Tone, s.Reason, s.Refused = "bad", short, true
		}
	case strings.HasPrefix(low, "review:"):
		v := strings.TrimSpace(low[len("review:"):])
		switch {
		case strings.HasPrefix(v, "approve"):
			s.Label, s.Tone, s.Approve = "approve", "ok", true
		case strings.HasPrefix(v, "request") || strings.HasPrefix(v, "change"):
			s.Label, s.Tone = "changes", "warn"
		default:
			s.Label = "review " + v
		}
	case strings.HasPrefix(low, "pin:"):
		s.Label, s.Pin = "pin", true
	default:
		s.Label = shortText(d, 28)
	}
	return s
}

func shortText(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func ordinal(n int) string {
	switch n {
	case 2:
		return "2nd"
	case 3:
		return "3rd"
	}
	return fmt.Sprintf("%dth", n)
}

// endedOf says how an episode's last call ended; cut is true when it did not end with finish "stop". A call
// that recorded no finish reason is "not recorded", not a cut: nothing is known.
func endedOf(c store.ModelCall) (text, tone string, cut bool) {
	switch {
	case c.Error != "":
		return "error", "stop", true
	case c.FinishReason == "stop":
		return "answered", "neutral", false
	case c.FinishReason == "":
		return "not recorded", "neutral", false
	case c.FinishReason == "length":
		return "cut at the token cap", "stop", true
	}
	return "cut off", "stop", true
}

func timeOr(s string, prev time.Time) time.Time {
	if t, ok := parseTime(s); ok {
		return t
	}
	return prev
}

// buildFlow groups a run's calls; nil when no real call carries a role (a run the page keeps as it was).
func buildFlow(calls []store.ModelCall) *runFlow {
	var agent []store.ModelCall
	var steps []store.ModelCall
	hasRole := false
	for _, c := range calls {
		switch {
		case isStep(c):
			steps = append(steps, c)
		case c.Check == "":
			agent = append(agent, c)
			hasRole = hasRole || c.Role != ""
		}
	}
	if !hasRole {
		return nil
	}
	sort.SliceStable(agent, func(a, b int) bool { return agent[a].At < agent[b].At })
	f := &runFlow{}
	byKey := map[string]int{}
	var prev time.Time
	seg := 0
	lastRole := ""
	for i, c := range agent {
		role := c.Role
		if role == "" {
			role = "agent"
		}
		key := c.Episode
		if key == "" {
			if i > 0 && role != lastRole {
				seg++
			}
			key = fmt.Sprintf("role:%d:%s", seg, role)
		}
		lastRole = role
		t := timeOr(c.At, prev)
		prev = t
		idx, ok := byKey[key]
		if !ok {
			idx = len(f.Eps)
			byKey[key] = idx
			f.Eps = append(f.Eps, flowEp{Role: role, Alias: c.Alias, Model: c.Model, Start: t, CostSource: c.CostSource})
		}
		e := &f.Eps[idx]
		e.Calls = append(e.Calls, c)
		e.End = t
		in, out := callTokens(c)
		e.In, e.Out = e.In+in, e.Out+out
		if c.CostUSD != nil {
			v := *c.CostUSD
			if e.Cost != nil {
				v += *e.Cost
			}
			e.Cost = &v
		}
		turn := len(e.Calls)
		if c.Step != nil {
			turn = int(*c.Step)
		}
		for _, tc := range parseTools(c.Tools) {
			e.Tools = append(e.Tools, tc)
			if tc.Failed() {
				e.Failed++
			}
			if e.FirstWrite == 0 && tc.WritesFile() {
				e.FirstWrite = turn
			}
		}
	}
	sort.SliceStable(f.Eps, func(a, b int) bool { return f.Eps[a].Start.Before(f.Eps[b].Start) })
	seen := map[string]int{}
	for i := range f.Eps {
		e := &f.Eps[i]
		e.N = i + 1
		seen[e.Role]++
		e.Label = e.Role
		if seen[e.Role] > 1 {
			e.Label = fmt.Sprintf("%s (%s)", e.Role, ordinal(seen[e.Role]))
		}
		if seen[e.Role] == 1 {
			f.Roles = append(f.Roles, e.Role)
		}
		e.Ended, e.EndedTone, e.Cut = endedOf(e.Calls[len(e.Calls)-1])
		f.Turns += len(e.Calls)
		f.ToolCalls += len(e.Tools)
		f.Failed += e.Failed
		if e.Cost != nil {
			v := *e.Cost
			if f.Cost != nil {
				v += *f.Cost
			}
			f.Cost = &v
		}
		if f.Start.IsZero() || e.Start.Before(f.Start) {
			f.Start = e.Start
		}
		if e.End.After(f.End) {
			f.End = e.End
		}
	}
	sort.SliceStable(steps, func(a, b int) bool { return steps[a].At < steps[b].At })
	cur := -1
	prev = f.Start
	for _, c := range steps {
		s := classifyStep(c.Role, c.Detail, &cur)
		s.At = timeOr(c.At, prev)
		prev = s.At
		if s.Round && s.Tone == "" {
			f.Rounds++
		}
		f.Steps = append(f.Steps, s)
		if s.At.Before(f.Start) {
			f.Start = s.At
		}
		if s.At.After(f.End) {
			f.End = s.At
		}
	}
	return f
}

// An ordered item of the flow: an episode or a step, by time.
type flowItem struct {
	At   time.Time
	Ep   *flowEp
	Step *flowStep
}

func (f *runFlow) items() []flowItem {
	var out []flowItem
	for i := range f.Eps {
		out = append(out, flowItem{At: f.Eps[i].Start, Ep: &f.Eps[i]})
	}
	for i := range f.Steps {
		out = append(out, flowItem{At: f.Steps[i].At, Step: &f.Steps[i]})
	}
	sort.SliceStable(out, func(a, b int) bool { return out[a].At.Before(out[b].At) })
	return out
}

// ---- paths: the order of many runs, summed ----

type pathStep struct{ Label, Tone string } // Tone: agent | bad | ok | warn | ""

type flowPath struct {
	Steps    []pathStep
	Key      string
	Runs     []string
	Count    int
	Share    int // percent of the runs with a path
	Cost     *float64
	CostOf   int      // runs of the path that reported a cost
	Shown    []string // the first runs, for links
	MoreRuns int
	CostText string
	CostGap  bool
}

type pathSummary struct {
	Paths   []flowPath
	Runs    int
	Insight string
}

// pathOf is one run's path: its episodes' roles and its step rows, in time order. A pin is left out: the
// advisor episode that wrote it is already a step.
func pathOf(f *runFlow) []pathStep {
	var out []pathStep
	for _, it := range f.items() {
		switch {
		case it.Ep != nil:
			out = append(out, pathStep{Label: it.Ep.Role, Tone: "agent"})
		case it.Step.Pin:
		default:
			out = append(out, pathStep{Label: it.Step.Label, Tone: it.Step.Tone})
		}
	}
	return out
}

// buildPaths groups the runs (ids in the order given) by path. A routine is only worth a paths section when
// some run went through more than one step.
func buildPaths(ids []string, flows map[string]*runFlow) pathSummary {
	var ps pathSummary
	by := map[string]*flowPath{}
	var order []string
	multi, refusedRuns, approved := 0, 0, 0
	firstFail := map[string]int{}
	for _, id := range ids {
		f := flows[id]
		if f == nil || len(f.Eps) == 0 {
			continue
		}
		ps.Runs++
		steps := pathOf(f)
		labels := make([]string, len(steps))
		for i, s := range steps {
			labels[i] = s.Label
		}
		key := strings.Join(labels, " > ")
		p := by[key]
		if p == nil {
			p = &flowPath{Steps: steps, Key: key}
			by[key] = p
			order = append(order, key)
		}
		p.Count++
		p.Runs = append(p.Runs, id)
		if f.Cost != nil {
			v := *f.Cost
			if p.Cost != nil {
				v += *p.Cost
			}
			p.Cost = &v
			p.CostOf++
		}
		rounds := 0
		failed := ""
		for _, s := range f.Steps {
			if s.Round {
				rounds++
			}
			if s.Refused && failed == "" {
				failed = s.Reason
			}
			if s.Approve {
				approved++
				break
			}
		}
		if rounds > 1 {
			multi++
		}
		if failed != "" {
			refusedRuns++
			firstFail[failed]++
		}
	}
	for _, k := range order {
		ps.Paths = append(ps.Paths, *by[k])
	}
	sort.SliceStable(ps.Paths, func(a, b int) bool { return ps.Paths[a].Count > ps.Paths[b].Count })
	for i := range ps.Paths {
		p := &ps.Paths[i]
		p.Share = p.Count * 100 / max(ps.Runs, 1)
		p.Shown = p.Runs[:min(len(p.Runs), 3)]
		p.MoreRuns = len(p.Runs) - len(p.Shown)
		switch {
		case p.Cost == nil:
			p.CostText, p.CostGap = "cost per path: no call recorded a cost", true
		case p.CostOf < p.Count:
			p.CostText = fmt.Sprintf("cost per path: %s over the %d of %d runs that reported one", usd3(*p.Cost), p.CostOf, p.Count)
		default:
			p.CostText = fmt.Sprintf("cost per path: %s in all, %s a run", usd3(*p.Cost), usd3(*p.Cost/float64(p.Count)))
		}
	}
	if ps.Runs > 0 {
		ps.Insight = pathInsight(ps.Runs, multi, refusedRuns, approved, firstFail)
	}
	return ps
}

func pathInsight(runs, multi, refused, approved int, firstFail map[string]int) string {
	s := fmt.Sprintf("%d of %s needed more than one round.", multi, plural(runs, "run", "runs"))
	if multi == 0 {
		s = fmt.Sprintf("No run of %d needed more than one round.", runs)
	}
	if refused > 0 {
		best, n := "", 0
		for k, v := range firstFail {
			if v > n || (v == n && k < best) {
				best, n = k, v
			}
		}
		s += fmt.Sprintf(" The most common first failure was %s (%s).", best, plural(n, "run", "runs"))
	} else {
		s += " No round was refused."
	}
	if approved > 0 {
		s += fmt.Sprintf(" %d of %d got the reviewer's approval.", approved, runs)
	}
	return s
}
