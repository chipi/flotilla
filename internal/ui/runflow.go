package ui

import (
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/chipi/flotilla/internal/spec"
	"github.com/chipi/flotilla/internal/store"
)

// The run page's flow sections (what happened between the roles), built from the run's calls. A run whose
// calls carry no role keeps the page it had.

type orderItem struct {
	Who, Text, Meta string
	Chip, Tone      string
	Href            string
}

type epRow struct {
	N                int
	Label, Alias     string
	Turns            int
	Tools            string
	Out              string
	Cost             string
	CostGap          bool
	Took             string
	Ended, EndedTone string
	Href             string
	Sel              bool
}

type toolRow struct {
	Name, Arg      string
	Failed, Writes bool
}

type turnRow struct {
	Turn    int
	At      string
	Tools   []toolRow
	Out     string
	Cost    string
	CostGap bool
	Ended   string
	// a folded stretch of turns, with the link that opens it
	Fold     bool
	FoldText string
	FoldHref string
}

type epOpen struct {
	Title, Say string
	Rows       []turnRow
	AllHref    string
	Expanded   bool
	More       bool
}

type runFlowView struct {
	Say       string
	Inst      instruments
	Swim      swim
	Order     []orderItem
	Eps       []epRow
	Open      *epOpen
	Calls     []store.ModelCall
	CallsMore string
	Roles     []pillLink
	Total     int
}

func costText(c *float64) (string, bool) {
	if c == nil {
		return "no cost recorded", true
	}
	return usd3(*c), false
}

func tookText(d time.Duration) string {
	switch {
	case d < time.Second:
		return "under 1 s"
	case d < 90*time.Second:
		return fmt.Sprintf("%.0f s", d.Seconds())
	}
	return fmt.Sprintf("%.1f min", d.Minutes())
}

// flowSay is the run in one sentence: what ran, what it cost, what went wrong first.
func flowSay(f *runFlow) string {
	var roles []string
	seen := map[string]bool{}
	for _, e := range f.Eps {
		if !seen[e.Role] {
			seen[e.Role] = true
			roles = append(roles, e.Role)
		}
	}
	s := fmt.Sprintf("%s (%s), %s, %s", plural(len(f.Eps), "episode", "episodes"), strings.Join(roles, ", "),
		plural(f.Turns, "turn", "turns"), plural(f.ToolCalls, "tool call", "tool calls"))
	if f.Cost != nil {
		s += ", " + usd3(*f.Cost)
	} else {
		s += ", no cost recorded"
	}
	s += "."
	if f.Failed > 0 {
		s += fmt.Sprintf(" %s failed.", plural(f.Failed, "tool call", "tool calls"))
	}
	for _, st := range f.Steps {
		if st.Refused {
			s += " The gates refused a round: " + st.Reason + "."
			break
		}
	}
	for i := len(f.Eps) - 1; i >= 0; i-- {
		if f.Eps[i].Cut {
			s += fmt.Sprintf(" The %s episode was cut off: %s.", f.Eps[i].Role, strings.TrimPrefix(cutText(f.Eps[i]), "cut off: "))
			break
		}
	}
	return s
}

func runInstruments(f *runFlow, run store.Run, j *spec.Routine, calls []store.ModelCall) instruments {
	var out instruments
	cost := instRow{Label: "Cost"}
	if f.Cost == nil {
		cost.Val, cost.Gap = "–", "no call recorded a cost"
	} else {
		cost.Val = usd3(*f.Cost)
		if j != nil && j.Budget.DayUSD > 0 {
			frac := *f.Cost / j.Budget.DayUSD
			fill := pctOf(frac*100, 100)
			cost.Level = level(frac)
			cost.G = gauge{Fill: fill, HasFill: true, Ticks: []gaugeTick{{Pct: 100, End: true, Label: usd2(j.Budget.DayUSD) + " a day"}}, Aria: ariaOf("Cost", fill)}
			cost.Note = "The routine's day budget; this run's share of it. "
		}
		if t := costSourceText(calls); t != "" {
			cost.Note += "Cost from " + t + "."
		}
	}
	out.Rows = append(out.Rows, cost)

	turns := instRow{Label: "Turns", Val: fmt.Sprint(f.Turns)}
	if j != nil && j.Harness != nil && j.Harness.Limits.MaxSteps > 0 {
		longest := 0
		for _, e := range f.Eps {
			longest = max(longest, len(e.Calls))
		}
		lim := j.Harness.Limits.MaxSteps
		frac := float64(longest) / float64(lim)
		fill := pctOf(frac*100, 100)
		turns.Val = fmt.Sprintf("%d, longest episode %d of %d", f.Turns, longest, lim)
		turns.Level = level(frac)
		turns.G = gauge{Fill: fill, HasFill: true, Ticks: []gaugeTick{{Pct: 100, End: true, Label: fmt.Sprintf("%d max", lim)}}, Aria: ariaOf("Turns", fill)}
	} else {
		turns.Note = "The spec sets no turn limit for this routine."
	}
	out.Rows = append(out.Rows, turns)

	rounds := 0
	for _, s := range f.Steps {
		if s.Round {
			rounds++
		}
	}
	if rounds > 0 {
		r := instRow{Label: "Rounds", Val: fmt.Sprintf("%d clean of %d", f.Rounds, rounds), G: gauge{Aria: "Rounds"}}
		r.Gap = "round limit: not in the spec"
		if f.Rounds == 0 {
			r.Note = "No round finished clean."
		}
		out.Rows = append(out.Rows, r)
	}

	end := instRow{Label: "How it ended", Val: "answered"}
	var cut *flowEp
	for i := range f.Eps {
		if f.Eps[i].Cut {
			cut = &f.Eps[i]
		}
	}
	switch {
	case run.Status == store.StatusError || run.Status == store.StatusTimeout:
		end.Val, end.Level = run.Status, "bad"
	case cut != nil:
		end.Val, end.Level = "cut off", "alert"
	case run.Status == store.StatusInterrupted:
		end.Val, end.Level = "interrupted", "alert"
	case run.Status == store.StatusRunning:
		end.Val = "still running"
	}
	if cut != nil {
		end.Note = fmt.Sprintf("The %s episode: %s.", cut.Role, strings.TrimPrefix(cutText(*cut), "cut off: "))
	}
	if run.Status != store.StatusRunning {
		end.G = gauge{Fill: 100, HasFill: true, Aria: "How it ended: " + end.Val}
	}
	out.Rows = append(out.Rows, end)

	switch {
	case end.Level == "bad":
		out.Lead, out.Rest = "Past a limit.", "The run ended in "+run.Status+"."
	case end.Level == "alert":
		out.Lead, out.Rest = "Worth a look.", "The run did not finish as planned: "+end.Val+"."
	default:
		out.Lead, out.Rest = "All within limits.", ""
	}
	return out
}

// hrmm is a time as the installation's clock shows it.
func hrmm(t time.Time, loc *time.Location) string { return t.In(loc).Format("15:04") }

func (u *Server) runFlowView(f *runFlow, run store.Run, calls []store.ModelCall, q url.Values) *runFlowView {
	loc := u.loc()
	_, by := u.specs()
	j := by[run.Routine]
	sel, _ := strconv.Atoi(q.Get("ep"))
	base := "/runs/" + run.ID
	epHref := func(n int) string { return base + "?ep=" + strconv.Itoa(n) + "#turns" }
	v := &runFlowView{Say: flowSay(f), Inst: runInstruments(f, run, j, calls)}
	v.Swim = buildSwim(f, sel, loc, epHref)

	for _, it := range f.items() {
		switch {
		case it.Ep != nil:
			e := it.Ep
			ct, _ := costText(e.Cost)
			meta := fmt.Sprintf("%s · %s, %s, %s", hrmm(e.Start, loc), plural(len(e.Calls), "turn", "turns"), plural(len(e.Tools), "tool call", "tool calls"), ct)
			item := orderItem{Who: e.Label, Text: "ran" + map[bool]string{true: " as " + e.Alias}[e.Alias != ""], Meta: meta, Href: epHref(e.N)}
			if e.Cut {
				item.Chip, item.Tone = e.Ended, "stop"
			}
			v.Order = append(v.Order, item)
		default:
			s := it.Step
			item := orderItem{Who: s.Role, Text: s.Detail, Meta: hrmm(s.At, loc)}
			switch s.Tone {
			case "bad":
				item.Chip, item.Tone = s.Label, "stop"
			case "ok":
				item.Chip, item.Tone = s.Label, "ok"
			case "warn":
				item.Chip, item.Tone = s.Label, "wait"
			}
			v.Order = append(v.Order, item)
		}
	}

	for _, e := range f.Eps {
		r := epRow{N: e.N, Label: e.Label, Alias: e.Alias, Turns: len(e.Calls), Tools: toolCounts(e.Tools), Out: tokensText(e.Out),
			Took: tookText(e.End.Sub(e.Start)), Ended: e.Ended, EndedTone: e.EndedTone, Href: epHref(e.N), Sel: e.N == sel}
		r.Cost, r.CostGap = costText(e.Cost)
		if r.Tools == "" {
			r.Tools = "–"
		}
		v.Eps = append(v.Eps, r)
	}
	if sel >= 1 && sel <= len(f.Eps) {
		v.Open = openEpisode(f.Eps[sel-1], base, q, loc)
	}

	// the calls table: every call, or one role's, the first few unless asked for all
	role := q.Get("role")
	var all []store.ModelCall
	for _, c := range calls {
		if isAgentCall(c) && (role == "" || c.Role == role) {
			all = append(all, c)
		}
	}
	v.Total = len(all)
	v.Calls = all
	if q.Get("calls") != "all" && len(all) > 12 {
		v.Calls = all[:12]
		v.CallsMore = fmt.Sprintf("%s more", plural(len(all)-12, "call", "calls"))
	}
	roleHref := func(r string) string {
		if r == "" {
			return base + "#calls-h"
		}
		return base + "?role=" + url.QueryEscape(r) + "#calls-h"
	}
	v.Roles = append(v.Roles, pillLink{Label: "All roles", Href: roleHref(""), On: role == ""})
	for _, r := range f.Roles {
		v.Roles = append(v.Roles, pillLink{Label: r, Href: roleHref(r), On: role == r})
	}
	return v
}

const headTurns, tailTurns, foldChunk = 6, 3, 60

// openEpisode lists an episode's turns, the middle folded: the first few, the last few, and any stretch the
// operator opened with ?t=a-b; ?all=1 opens every turn.
func openEpisode(e flowEp, base string, q url.Values, loc *time.Location) *epOpen {
	o := &epOpen{Title: fmt.Sprintf("%d · %s: its turns", e.N, e.Label)}
	n := len(e.Calls)
	turnNo := func(i int) int {
		if s := e.Calls[i].Step; s != nil {
			return int(*s)
		}
		return i + 1
	}
	lo, hi := 0, 0
	if t := q.Get("t"); t != "" {
		if a, b, ok := strings.Cut(t, "-"); ok {
			lo, _ = strconv.Atoi(a)
			hi, _ = strconv.Atoi(b)
		}
	}
	all := q.Get("all") == "1"
	o.Expanded = all || lo > 0
	visible := func(i int) bool {
		tn := turnNo(i)
		return all || i < headTurns || i >= n-tailTurns || (lo > 0 && tn >= lo && tn <= hi)
	}
	base2 := base + "?ep=" + strconv.Itoa(e.N)
	o.AllHref = base2 + "&all=1#turns"
	for i := 0; i < n; {
		if visible(i) {
			c := e.Calls[i]
			r := turnRow{Turn: turnNo(i)}
			if t, ok := parseTime(c.At); ok {
				r.At = t.In(loc).Format("15:04:05")
			}
			for _, tc := range parseTools(c.Tools) {
				r.Tools = append(r.Tools, toolRow{Name: tc.Name, Arg: tc.Arg, Failed: tc.Failed(), Writes: tc.WritesFile()})
			}
			if c.TokensOut != nil {
				r.Out = fmt.Sprint(*c.TokensOut)
			}
			if c.CostUSD != nil {
				r.Cost = fmt.Sprintf("$%.4f", *c.CostUSD)
			} else {
				r.CostGap = true
			}
			switch {
			case c.Error != "":
				r.Ended = "error"
			case c.FinishReason == "":
				r.Ended = "–"
			case c.FinishReason == "tool_calls":
				r.Ended = "tool calls"
			default:
				r.Ended = c.FinishReason
			}
			o.Rows = append(o.Rows, r)
			i++
			continue
		}
		j := i
		for j < n && !visible(j) {
			j++
		}
		a, b := turnNo(i), turnNo(j-1)
		o.More = true
		o.Rows = append(o.Rows, turnRow{Fold: true, FoldText: fmt.Sprintf("show turns %d–%d", a, b),
			FoldHref: fmt.Sprintf("%s&t=%d-%d#turns", base2, a, min(b, a+foldChunk-1))})
		i = j
	}

	var parts []string
	if e.FirstWrite > 0 {
		parts = append(parts, fmt.Sprintf("Its first edit came at turn %d; before it, %s of reading and running.", e.FirstWrite, plural(e.FirstWrite-1, "turn", "turns")))
	} else {
		parts = append(parts, "It changed no file.")
	}
	if e.Failed > 0 {
		parts = append(parts, fmt.Sprintf("%s failed (a grep that found nothing counts).", plural(e.Failed, "tool call", "tool calls")))
	}
	if e.Ended == "answered" {
		parts = append(parts, fmt.Sprintf("It answered at turn %d.", turnNo(n-1)))
	} else {
		parts = append(parts, fmt.Sprintf("It ended at turn %d: %s.", turnNo(n-1), e.Ended))
	}
	o.Say = strings.Join(parts, " ")
	return o
}

// sortedRoles keeps the role filter links stable.
func sortedRoles(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
