package ui

import (
	"sort"

	"github.com/chipi/flotilla/internal/store"
)

// What a routine's model calls usually look like, from the days that recorded any. A band (P10 to P90) is
// only drawn once minBaselineDays days exist: colour on a thinner history would be a guess dressed as a
// signal (ISA-101: colour means something every time it appears).

const minBaselineDays = 5

func median(xs []float64) float64 { return percentile(xs, 50) }

// percentile is the p-th (0-100) of xs by linear interpolation between the two nearest ranks; 0 for none.
func percentile(xs []float64, p float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	if len(s) == 1 {
		return s[0]
	}
	pos := p / 100 * float64(len(s)-1)
	lo := int(pos)
	if lo >= len(s)-1 {
		return s[len(s)-1]
	}
	return s[lo] + (s[lo+1]-s[lo])*(pos-float64(lo))
}

type usual struct {
	Days             int
	Median, P10, P90 float64
	Enough           bool // at least minBaselineDays days: only then is there a band to be outside of
}

func usualOf(vals []float64) usual {
	u := usual{Days: len(vals)}
	if len(vals) == 0 {
		return u
	}
	u.Median, u.P10, u.P90 = median(vals), percentile(vals, 10), percentile(vals, 90)
	u.Enough = len(vals) >= minBaselineDays
	return u
}

// Outside is true only when there is a band and v is beyond it.
func (u usual) Outside(v float64) bool { return u.Enough && (v > u.P90 || v < u.P10) }

// dayStat is one UTC day of a routine's real model calls (no step rows, no contract checks).
type dayStat struct {
	Day           string
	Calls         int
	Cost          float64
	Priced        int // calls that reported a cost; a null cost is a gap, not 0
	In, Out       int64
	MaxOut        int64
	Cut, Errors   int
	Decisions     int // distinct decision keys, or the ledger's count for the day when no call carries one
	DecisionCalls int
	Tools         int
	Latencies     []float64
	Ins           []int64
}

func (d *dayStat) costPerDecision() (float64, bool) {
	if d.Priced == 0 || d.Decisions == 0 {
		return 0, false
	}
	return d.Cost / float64(d.Decisions), true
}

func (d *dayStat) turnsPerDecision() (float64, bool) {
	if d.Decisions == 0 || d.DecisionCalls == 0 {
		return 0, false
	}
	return float64(d.DecisionCalls) / float64(d.Decisions), true
}

func dayOf(at string) string {
	if t, ok := parseTime(at); ok {
		return t.UTC().Format("2006-01-02")
	}
	if len(at) >= 10 {
		return at[:10]
	}
	return ""
}

// dayStatsOf folds already-filtered calls into days. ledger is the decisions the ledger holds per day, used
// when a day's calls name no decision.
func dayStatsOf(calls []store.ModelCall, ledger map[string]int) map[string]*dayStat {
	out := map[string]*dayStat{}
	keys := map[string]map[string]bool{}
	keyed := map[string]int{}
	for _, c := range calls {
		day := dayOf(c.At)
		if day == "" {
			continue
		}
		d := out[day]
		if d == nil {
			d = &dayStat{Day: day}
			out[day] = d
			keys[day] = map[string]bool{}
		}
		d.Calls++
		if c.CostUSD != nil {
			d.Cost += *c.CostUSD
			d.Priced++
		}
		in, o := callTokens(c)
		d.In, d.Out = d.In+in, d.Out+o
		if o > d.MaxOut {
			d.MaxOut = o
		}
		if c.TokensIn != nil {
			d.Ins = append(d.Ins, *c.TokensIn)
		}
		if c.FinishReason == "length" {
			d.Cut++
		}
		if c.Error != "" {
			d.Errors++
		}
		if c.LatencyS != nil {
			d.Latencies = append(d.Latencies, *c.LatencyS)
		}
		d.Tools += len(parseTools(c.Tools))
		if c.DecisionKey != "" {
			keys[day][c.DecisionKey] = true
			keyed[day]++
		}
	}
	for day, d := range out {
		// the calls' own decision keys only when every call of the day has one: on the day the keys began
		// (2026-11-17 for triage) the earlier calls have none, and counting only the keyed decisions divided
		// the whole day's cost by a few of its decisions
		if len(keys[day]) > 0 && (keyed[day] == d.Calls || ledger[day] == 0) {
			d.Decisions, d.DecisionCalls = len(keys[day]), keyed[day]
		} else {
			d.Decisions, d.DecisionCalls = ledger[day], d.Calls
		}
	}
	return out
}

// focusDay is today when it has calls, else the latest day that has any.
func focusDay(days map[string]*dayStat, today string) *dayStat {
	if d := days[today]; d != nil {
		return d
	}
	var latest *dayStat
	for _, d := range days {
		if latest == nil || d.Day > latest.Day {
			latest = d
		}
	}
	return latest
}

// series is one value per day, for the days that have one.
func series(days map[string]*dayStat, pick func(*dayStat) (float64, bool)) []float64 {
	var out []float64
	for _, d := range days {
		if v, ok := pick(d); ok {
			out = append(out, v)
		}
	}
	return out
}
