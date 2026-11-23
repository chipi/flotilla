package ui

import (
	"fmt"
	"math"
	"strings"
)

// The Instruments card: a few gauges that say whether a routine's model work is normal. ISA-101: grey is
// normal and carries no signal; amber means outside its usual range or past 70% of a limit; red means a limit
// was crossed. A gauge with no limit and no baseline stays grey and says why.

type gaugeTick struct {
	Pct   int
	Label string
	Soft  bool // a reference (the usual, the previous result), not a limit
	End   bool // the label hangs left of the tick, so it stays inside the box
}

type gauge struct {
	Fill    int // percent, 0-100
	HasFill bool
	Band    bool
	BandLo  int // P10, percent of the scale
	BandW   int
	Ticks   []gaugeTick
	Aria    string
}

type instRow struct {
	Label, Val, Note, Gap, Tail string
	Level                       string // "" | alert | bad
	G                           gauge
}

type instruments struct {
	Lead, Rest string
	Rows       []instRow
}

type evalView struct {
	Metric      string
	Value       float64
	N, Repeats  int64
	SHA, Source string
	Prev        *float64
}

type instInput struct {
	Days      map[string]*dayStat
	Today     string
	Window    int
	DayBudget float64
	MaxSteps  int
	FinalCall bool
	Evals     []evalView
	EvalGap   string
}

func pctOf(v, scale float64) int {
	if scale <= 0 || math.IsNaN(v) {
		return 0
	}
	return int(math.Max(0, math.Min(100, math.Round(v/scale*100))))
}

func groupInt(n int64) string {
	s := fmt.Sprint(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

func dayWord(n int) string { return plural(n, "day", "days") }

func level(frac float64) string {
	switch {
	case frac >= 1:
		return "bad"
	case frac >= 0.7:
		return "alert"
	}
	return ""
}

func ariaOf(label string, fill int) string { return fmt.Sprintf("%s: %d%% of the scale", label, fill) }

// buildInstruments draws the card from a routine's days of calls; false when there is nothing to draw.
func buildInstruments(in instInput) (instruments, bool) {
	var out instruments
	focus := focusDay(in.Days, in.Today)
	if focus == nil {
		return out, false
	}
	isToday := focus.Day == in.Today
	when := "today"
	if !isToday {
		when = focus.Day[5:] + ", the last day with calls"
	}
	add := func(r instRow) { out.Rows = append(out.Rows, r) }

	// spend against the spec's day budget; the key's cap is not in any spec, so there is no second tick
	sp := instRow{Label: "Spend today"}
	if !isToday {
		sp.Label = "Spend, last active day"
	}
	switch {
	case focus.Priced == 0:
		sp.Val, sp.Gap = "–", "cost not reported on these calls"
	case in.DayBudget > 0:
		frac := focus.Cost / in.DayBudget
		fill := pctOf(frac*100, 100)
		sp.Val = fmt.Sprintf("%s of %s", usd3(focus.Cost), usd2(in.DayBudget))
		sp.G = gauge{Fill: fill, HasFill: true, Ticks: []gaugeTick{{Pct: 100, End: true, Label: usd2(in.DayBudget) + " a day"}}, Aria: ariaOf("Spend", fill)}
		sp.Level = level(frac)
		sp.Note = fmt.Sprintf("%s: %s.", when, plural(focus.Calls, "call", "calls"))
	default:
		sp.Val = usd3(focus.Cost)
		sp.Note = fmt.Sprintf("%s: %s. The spec sets no day budget.", when, plural(focus.Calls, "call", "calls"))
	}
	add(sp)

	cpdSeries := series(in.Days, func(d *dayStat) (float64, bool) { return d.costPerDecision() })
	if v, ok := focus.costPerDecision(); ok {
		u := usualOf(cpdSeries)
		r := instRow{Label: "Cost per decision", Val: fmt.Sprintf("$%.4f", v)}
		note := fmt.Sprintf("%s, %s.", plural(focus.Decisions, "decision", "decisions"), plural(focus.Calls, "call", "calls"))
		if u.Enough {
			scale := 2 * u.Median
			if scale <= 0 {
				scale = math.Max(v, 0.0001) * 2
			}
			fill := pctOf(v, scale)
			lo, hi := pctOf(u.P10, scale), pctOf(u.P90, scale)
			r.G = gauge{Fill: fill, HasFill: true, Band: true, BandLo: lo, BandW: max(hi-lo, 1),
				Ticks: []gaugeTick{{Pct: 50, Soft: true, Label: "usual"}}, Aria: ariaOf("Cost per decision", fill)}
			if u.Outside(v) {
				r.Level = "alert"
			}
			r.Note = fmt.Sprintf("%s Usual $%.4f, P10 to P90 $%.4f to $%.4f over %s.", note, u.Median, u.P10, u.P90, dayWord(u.Days))
		} else {
			r.G = gauge{Aria: "Cost per decision: no baseline yet"}
			r.Note = note
			r.Gap = fmt.Sprintf("baseline: %s", dayWord(u.Days))
			r.Tail = fmt.Sprintf("no colour until %d days of history exist.", minBaselineDays)
		}
		add(r)
	}

	if tpd, ok := focus.turnsPerDecision(); ok && in.MaxSteps > 0 {
		frac := tpd / float64(in.MaxSteps)
		fill := pctOf(frac*100, 100)
		r := instRow{Label: "Turns per decision", Val: fmt.Sprintf("%.1f of %d", tpd, in.MaxSteps), Level: level(frac),
			G: gauge{Fill: fill, HasFill: true, Ticks: []gaugeTick{{Pct: 100, End: true, Label: fmt.Sprintf("%d max", in.MaxSteps)}}, Aria: ariaOf("Turns per decision", fill)}}
		r.Note = fmt.Sprintf("The harness allows %d turns", in.MaxSteps)
		if in.FinalCall {
			r.Note += " and one more final decision call"
		}
		r.Note += "."
		add(r)
	}

	if focus.MaxOut > 0 {
		var maxAll int64
		var cutAll int
		for _, d := range in.Days {
			maxAll = max(maxAll, d.MaxOut)
			cutAll += d.Cut
		}
		fill := pctOf(float64(focus.MaxOut), float64(maxAll))
		r := instRow{Label: "Longest reply", Val: groupInt(focus.MaxOut) + " tokens",
			G: gauge{Fill: fill, HasFill: true, Aria: ariaOf("Longest reply", fill)}}
		if focus.Cut > 0 {
			r.Level = "bad"
		}
		r.Note = fmt.Sprintf("%d cut at the token cap in %s. No reply cap in the spec, so the bar is against the longest of those days (%s tokens).", cutAll, dayWord(len(in.Days)), groupInt(maxAll))
		add(r)
	}

	if len(in.Evals) == 0 {
		add(instRow{Label: "Quality (labelled replay)", Val: "–", Gap: in.EvalGap, G: gauge{Aria: "Quality: no result"}})
	}
	for _, e := range in.Evals {
		r := instRow{Label: "Quality: " + strings.ReplaceAll(e.Metric, "_", " "), Val: fmt.Sprintf("%.3g", e.Value)}
		r.Note = fmt.Sprintf("Eval, %d labelled × %d, prompt %s.", e.N, e.Repeats, shortText(e.SHA, 8))
		if e.Prev != nil {
			scale := math.Max(e.Value, *e.Prev)
			fill := pctOf(e.Value, scale)
			r.G = gauge{Fill: fill, HasFill: true, Ticks: []gaugeTick{{Pct: pctOf(*e.Prev, scale), Soft: true, End: pctOf(*e.Prev, scale) > 80, Label: fmt.Sprintf("%.3g before", *e.Prev)}}, Aria: ariaOf("Quality", fill)}
		} else {
			r.G = gauge{Aria: "Quality: no earlier result to compare"}
			r.Tail = "no earlier result to compare."
		}
		add(r)
	}

	out.Lead, out.Rest = instSay(out.Rows, focus, in, len(cpdSeries))
	return out, true
}

// instSay is the one sentence over the gauges: what is off, or that nothing is.
func instSay(rows []instRow, focus *dayStat, in instInput, cpdDays int) (string, string) {
	var bad, alert []string
	for _, r := range rows {
		l := strings.ToLower(r.Label)
		switch r.Level {
		case "bad":
			bad = append(bad, l+" ("+r.Val+")")
		case "alert":
			alert = append(alert, l+" ("+r.Val+")")
		}
	}
	switch {
	case len(bad) > 0:
		rest := "A limit is crossed: " + strings.Join(bad, ", ") + "."
		if len(alert) > 0 {
			rest += " Also off its usual: " + strings.Join(alert, ", ") + "."
		}
		return "Past a limit.", rest
	case len(alert) > 0:
		return "Worth a look.", "Outside its usual range or past 70% of a limit: " + strings.Join(alert, ", ") + "."
	}
	var rest []string
	if in.DayBudget > 0 && focus.Priced > 0 {
		rest = append(rest, fmt.Sprintf("Spend is %d%% of the day's budget.", int(math.Round(focus.Cost/in.DayBudget*100))))
	}
	for _, r := range rows {
		if r.Label == "Cost per decision" && r.Gap != "" {
			rest = append(rest, fmt.Sprintf("History is %s, too short to say whether cost per decision is drifting.", dayWord(cpdDays)))
		}
	}
	return "All within limits.", strings.Join(rest, " ")
}

// Worry is whether a row is off its usual or past a limit: the card opens itself only then, and is one line otherwise.
func (in instruments) Worry() bool {
	for _, r := range in.Rows {
		if r.Level != "" {
			return true
		}
	}
	return false
}

// instCard is the Instruments card's data: the rows and what their colours mean on this page.
type instCard struct {
	I   instruments
	Key string
}
