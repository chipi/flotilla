package ui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/chipi/flotilla/internal/spec"
	"github.com/chipi/flotilla/internal/store"
)

// What the routine page says about the model: small numbers on the blocks that stand for it, and the
// panel the model block opens. Everything is from the routine's own calls (no step rows, no contract
// checks, which share the alias but are not the routine's work); a number the store does not hold is named
// as a gap in its place, never shown as 0.

type numItem struct {
	Text string
	Soft bool
	Gap  bool
}

type kvRow struct {
	K, V, Gap string
	Exact     bool
}

type usualRow struct{ Label, Today, Usual, Gap string }

type distBar struct {
	Label string
	H     int // percent of the tallest bar
	Count int
}

type callRow struct {
	At, Turn, InOut, Cost, Took, Ended string
	TurnGap, CostGap, EndedGap         bool
}

type modelPanel struct {
	Say       string
	Unit      string
	CostHead  string
	Cost      []usualRow
	CostNote  string
	Model     []kvRow
	Harness   []kvRow
	Sees      []kvRow
	Dist      []distBar
	DistLabel string
	ToolNames string
	ToolGap   string
	Last      []callRow
}

type modelCtx struct {
	Days    map[string]*dayStat
	Focus   *dayStat
	Today   string
	Window  int
	Calls   []store.ModelCall // the routine's real calls in the window, oldest first
	Probes  map[string]int    // the ledger's probes-a-decision histogram, when the routine has one
	Decs    []store.LedgerDecision
	Spec    *spec.Routine
	Aliases map[string]aliasInfo
	Loc     *time.Location
}

func (m modelCtx) whenText() string {
	if m.Focus == nil {
		return ""
	}
	if m.Focus.Day == m.Today {
		return "today"
	}
	return "last active day (" + m.Focus.Day[5:] + ")"
}

// tokensPer is a day's tokens per decision (or per day when it recorded no decision).
func (d *dayStat) den() float64 {
	if d.Decisions > 0 {
		return float64(d.Decisions)
	}
	return 1
}

// costNums are the small numbers on the model block: what it cost, tokens, calls.
func (m modelCtx) costNums() []numItem {
	f := m.Focus
	if f == nil {
		return nil
	}
	var out []numItem
	if f.Priced > 0 {
		out = append(out, numItem{Text: usd3(f.Cost)})
	} else {
		out = append(out, numItem{Text: "cost: not reported", Gap: true})
	}
	out = append(out, numItem{Text: tokensText(f.In) + " in · " + tokensText(f.Out) + " out", Soft: true},
		numItem{Text: plural(f.Calls, "call", "calls") + " " + m.shortWhen(), Soft: true})
	return out
}

func (m modelCtx) limitsNums() []numItem {
	f := m.Focus
	if f == nil {
		return nil
	}
	var out []numItem
	if tpd, ok := f.turnsPerDecision(); ok {
		out = append(out, numItem{Text: fmt.Sprintf("%.1f turns a decision", tpd)})
	}
	return append(out, numItem{Text: fmt.Sprintf("%d cut at cap", f.Cut), Soft: true})
}

// probesPerDecision is the mean of the ledger's histogram (keys are the probe counts as text).
func probesPerDecision(h map[string]int) (float64, bool) {
	var n, sum int
	for k, v := range h {
		var p int
		if _, err := fmt.Sscanf(k, "%d", &p); err != nil {
			continue
		}
		n += v
		sum += p * v
	}
	if n == 0 {
		return 0, false
	}
	return float64(sum) / float64(n), true
}

func (m modelCtx) toolsNums() []numItem {
	var out []numItem
	if p, ok := probesPerDecision(m.Probes); ok {
		out = append(out, numItem{Text: fmt.Sprintf("%.1f probes a decision", p)})
	} else if f := m.Focus; f != nil && f.Tools > 0 && f.Decisions > 0 {
		out = append(out, numItem{Text: fmt.Sprintf("%.1f tool calls a decision", float64(f.Tools)/float64(f.Decisions))})
	}
	if names := m.toolNames(); names != "" {
		out = append(out, numItem{Text: shortText(names, 40), Soft: true})
	} else if len(out) > 0 {
		out = append(out, numItem{Text: "which tools: not traced", Gap: true})
	}
	return out
}

func (m modelCtx) toolNames() string {
	var all []toolCall
	for _, c := range m.Calls {
		all = append(all, parseTools(c.Tools)...)
	}
	return toolCounts(all)
}

func p95(xs []float64) float64 { return percentile(xs, 95) }

func usualText(u usual, f func(float64) string) (string, string) {
	if !u.Enough {
		return "", fmt.Sprintf("%s of history", dayWord(u.Days))
	}
	return fmt.Sprintf("%s (%s to %s)", f(u.Median), f(u.P10), f(u.P90)), ""
}

func (m modelCtx) panel() *modelPanel {
	f := m.Focus
	if f == nil {
		return nil
	}
	p := &modelPanel{CostHead: m.whenText()}
	unit := "per decision"
	if f.Decisions == 0 {
		unit = "a day"
	}
	p.Unit = unit
	row := func(label, today string, pick func(*dayStat) (float64, bool), fm func(float64) string) {
		r := usualRow{Label: label, Today: today}
		if pick != nil {
			r.Usual, r.Gap = usualText(usualOf(series(m.Days, pick)), fm)
		} else {
			r.Usual = "–"
		}
		p.Cost = append(p.Cost, r)
	}
	if f.Priced > 0 {
		row("cost", fmt.Sprintf("$%.4f", f.Cost/f.den()), func(d *dayStat) (float64, bool) {
			if d.Priced == 0 {
				return 0, false
			}
			return d.Cost / d.den(), true
		}, func(v float64) string { return fmt.Sprintf("$%.4f", v) })
	} else {
		p.Cost = append(p.Cost, usualRow{Label: "cost", Today: "–", Gap: "not reported on these calls"})
	}
	row("tokens in / out (usual: in + out)", tokensText(int64(float64(f.In)/f.den()))+" / "+tokensText(int64(float64(f.Out)/f.den())),
		func(d *dayStat) (float64, bool) { return float64(d.In+d.Out) / d.den(), d.Calls > 0 },
		func(v float64) string { return tokensText(int64(v)) })
	if tpd, ok := f.turnsPerDecision(); ok {
		row("turns", fmt.Sprintf("%.1f", tpd), func(d *dayStat) (float64, bool) { return d.turnsPerDecision() },
			func(v float64) string { return fmt.Sprintf("%.1f", v) })
	}
	if len(f.Latencies) > 0 {
		row("call latency, p95", secsText(p95(f.Latencies)), func(d *dayStat) (float64, bool) {
			return p95(d.Latencies), len(d.Latencies) > 0
		}, secsText)
	}
	row("errors", fmt.Sprintf("%d of %d", f.Errors, f.Calls), nil, nil)
	p.CostNote = fmt.Sprintf("Its usual is the median and P10 to P90 over the last %d days, shown once %d days are recorded (this routine has %s). Calls of the platform's heartbeat and the model contract checks share the alias and are left out. Figures are %s.", m.Window, minBaselineDays, dayWord(len(m.Days)), unit)

	var parts []string
	if tpd, ok := f.turnsPerDecision(); ok {
		parts = append(parts, fmt.Sprintf("%.1f turns", tpd))
	}
	if f.Priced > 0 {
		parts = append(parts, fmt.Sprintf("$%.4f %s", f.Cost/f.den(), unit))
	}
	if f.Cut == 0 {
		parts = append(parts, "no reply cut at the token cap")
	} else {
		parts = append(parts, fmt.Sprintf("%d replies cut at the token cap", f.Cut))
	}
	if f.Errors == 0 {
		parts = append(parts, fmt.Sprintf("no errors in %s", plural(f.Calls, "call", "calls")))
	} else {
		parts = append(parts, fmt.Sprintf("%d errors in %s", f.Errors, plural(f.Calls, "call", "calls")))
	}
	p.Say = strings.Join(parts, "; ") + ", " + m.whenText() + "."

	p.Model = m.modelRows()
	p.Harness = m.harnessRows()
	p.Sees = m.seesRows()

	if len(m.Probes) > 0 {
		p.DistLabel = "Probes a decision, last " + fmt.Sprint(m.Window) + " days."
		p.Dist = distOf(m.Probes, "probe")
	} else {
		per := map[string]int{}
		for _, c := range m.Calls {
			if c.Tools != "" {
				per[fmt.Sprint(min(len(parseTools(c.Tools)), 3))]++
			}
		}
		if len(per) > 0 {
			p.DistLabel = "Tool calls a turn (3 means 3 or more), last " + fmt.Sprint(m.Window) + " days."
			p.Dist = distOf(per, "tool call")
		}
	}
	p.ToolNames = m.toolNames()
	if p.ToolNames == "" {
		p.ToolGap = "which tool each call was: not traced"
	}

	start := max(0, len(m.Calls)-6)
	for i := len(m.Calls) - 1; i >= start; i-- {
		c := m.Calls[i]
		r := callRow{InOut: "– / –", Took: "–"}
		if t, ok := parseTime(c.At); ok {
			r.At = t.In(m.Loc).Format("15:04")
		}
		if c.Step != nil {
			r.Turn = fmt.Sprint(*c.Step)
		} else {
			r.TurnGap = true
		}
		if c.TokensIn != nil && c.TokensOut != nil {
			r.InOut = groupInt(*c.TokensIn) + " / " + groupInt(*c.TokensOut)
		}
		if c.CostUSD != nil {
			r.Cost = fmt.Sprintf("$%.4f", *c.CostUSD)
		} else {
			r.CostGap = true
		}
		if c.LatencyS != nil {
			r.Took = secsText(*c.LatencyS)
		}
		switch {
		case c.Error != "":
			r.Ended = "error"
		case c.FinishReason == "length":
			r.Ended = "cut at the cap"
		case c.FinishReason != "":
			r.Ended = c.FinishReason
		default:
			r.EndedGap = true
		}
		p.Last = append(p.Last, r)
	}
	return p
}

func distOf(h map[string]int, what string) []distBar {
	keys := make([]string, 0, len(h))
	top := 0
	for k, v := range h {
		keys = append(keys, k)
		top = max(top, v)
	}
	sort.Slice(keys, func(a, b int) bool {
		return len(keys[a]) < len(keys[b]) || (len(keys[a]) == len(keys[b]) && keys[a] < keys[b])
	})
	var out []distBar
	for _, k := range keys {
		out = append(out, distBar{Label: k + " · " + fmt.Sprint(h[k]), H: max(h[k]*100/top, 4), Count: h[k]})
	}
	return out
}

func (m modelCtx) modelRows() []kvRow {
	var rows []kvRow
	if len(m.Calls) == 0 {
		return rows
	}
	count := map[string]int{}
	for _, c := range m.Calls {
		count[c.Alias]++
	}
	alias, n := "", 0
	for a, c := range count {
		if c > n || (c == n && a < alias) {
			alias, n = a, c
		}
	}
	last := m.Calls[len(m.Calls)-1]
	rows = append(rows, kvRow{K: "alias", V: alias, Exact: true})
	info := m.Aliases[alias]
	if info.Model != "" {
		rows = append(rows, kvRow{K: "gateway model", V: info.Model, Exact: true})
	} else {
		rows = append(rows, kvRow{K: "gateway model", Gap: "the installation's models section does not list this alias"})
	}
	if last.Model != "" {
		rows = append(rows, kvRow{K: "provider model", V: last.Model, Exact: true})
	}
	ep := info.Endpoint
	if ep == "" {
		ep = last.Endpoint
	}
	if ep != "" {
		rows = append(rows, kvRow{K: "endpoint", V: ep, Exact: true})
	}
	if last.Deployment != "" {
		rows = append(rows, kvRow{K: "deployment", V: shortText(last.Deployment, 12), Exact: true})
	} else {
		rows = append(rows, kvRow{K: "deployment", Gap: "not recorded"})
	}
	rows = append(rows, kvRow{K: "cost reported by", V: costSourceText(m.Calls), Gap: map[bool]string{true: "not recorded"}[costSourceText(m.Calls) == ""]})
	return rows
}

// costSourceText names where the calls' cost came from: "gateway-share" is the gateway's bill for a run,
// apportioned over its calls (bugfix's coding agent's own calls carry no price).
func costSourceText(calls []store.ModelCall) string {
	seen := map[string]bool{}
	var out []string
	for _, c := range calls {
		if c.CostSource == "" || seen[c.CostSource] {
			continue
		}
		seen[c.CostSource] = true
		switch c.CostSource {
		case "gateway":
			out = append(out, "the gateway, every call")
		case "gateway-share":
			out = append(out, "the gateway's bill for the run, shared out over its calls")
		default:
			out = append(out, c.CostSource)
		}
	}
	return strings.Join(out, "; ")
}

func (m modelCtx) harnessRows() []kvRow {
	h := m.Spec.Harness
	if h == nil {
		return nil
	}
	rows := []kvRow{{K: "protocol", V: h.Protocol, Exact: true}}
	turns := fmt.Sprintf("up to %d", h.Limits.MaxSteps)
	if h.Quirks.FinalDecisionCall {
		turns += ", then one final decision call"
	}
	rows = append(rows, kvRow{K: "turns", V: turns})
	perTurn := "one a turn"
	if h.Limits.ToolCallsPerTurn > 1 {
		perTurn = fmt.Sprintf("%d a turn", h.Limits.ToolCallsPerTurn)
	}
	rows = append(rows, kvRow{K: "tool calls", V: fmt.Sprintf("%s, at most %d", perTurn, h.Limits.MaxToolCalls)})
	rows = append(rows, kvRow{K: "reply cap", Gap: "not in the spec"})
	if len(h.Gates) > 0 {
		rows = append(rows, kvRow{K: "gates after it", V: strings.ReplaceAll(strings.Join(h.Gates, " · "), "_", " ")})
	}
	rows = append(rows, kvRow{K: "quirks hit", Gap: "not recorded yet"})
	return rows
}

func (m modelCtx) seesRows() []kvRow {
	var rows []kvRow
	if cur, since, was := promptHistory(m.Decs); cur != "" {
		v := cur
		rows = append(rows, kvRow{K: "prompt_sha", V: shortText(v, 12), Exact: true})
		if since != "" {
			s := "changed " + since
			if was != "" {
				s += " (was " + shortText(was, 8) + ")"
			}
			rows = append(rows, kvRow{K: "in use since", V: s})
		}
	} else {
		rows = append(rows, kvRow{K: "prompt_sha", Gap: "no decision in the ledger carries one"})
	}
	rows = append(rows, kvRow{K: "prompt_ver", Gap: "not recorded"})
	var ins []float64
	for _, c := range m.Calls {
		if c.TokensIn != nil {
			ins = append(ins, float64(*c.TokensIn))
		}
	}
	if len(ins) > 0 {
		rows = append(rows, kvRow{K: "prompt tokens a turn", V: fmt.Sprintf("%s (%s to %s)", tokensText(int64(median(ins))), tokensText(int64(percentile(ins, 10))), tokensText(int64(percentile(ins, 90)))), Exact: true})
	} else {
		rows = append(rows, kvRow{K: "prompt tokens a turn", Gap: "no call recorded its tokens in"})
	}
	if n := len(m.Spec.Skills.Rule); n > 0 {
		rows = append(rows, kvRow{K: "skill rules", V: plural(n, "rule", "rules") + " load hints by class"})
	}
	return rows
}

// promptHistory is the prompt sha the newest decision used, the day it began (the first decision of the
// unbroken run that used it) and the sha before it.
func promptHistory(decs []store.LedgerDecision) (cur, since, was string) {
	var seq []store.LedgerDecision
	for _, d := range decs {
		if d.PromptSHA != "" {
			seq = append(seq, d)
		}
	}
	if len(seq) == 0 {
		return "", "", ""
	}
	cur = seq[len(seq)-1].PromptSHA
	i := len(seq) - 1
	for i > 0 && seq[i-1].PromptSHA == cur {
		i--
	}
	if i > 0 {
		was = seq[i-1].PromptSHA
		if len(seq[i].TS) >= 10 {
			since = seq[i].TS[:10]
		}
	}
	return cur, since, was
}

// shortWhen is "today", or "on 11-16" for the last active day.
func (m modelCtx) shortWhen() string {
	if m.Focus.Day == m.Today {
		return "today"
	}
	return "on " + m.Focus.Day[5:]
}
