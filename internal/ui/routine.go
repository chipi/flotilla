package ui

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/chipi/flotilla/internal/api"
	"github.com/chipi/flotilla/internal/spec"
	"github.com/chipi/flotilla/internal/store"
)

// The routine page (UI plan step 3): a routine drawn from its spec. Each block is a part of the spec, or a
// step its code takes that the spec names under steps; its explanation is the spec's own explain text.
// Who acts colours a block's strip (code, agent, tool, you); the lamp is its state. A block opens a
// panel with its explanation, where it is set, the spec it comes from and its counts.

type rBlock struct {
	ID, Name, Sub, Who, Lamp, Nums string
	Explain, Where, Key, YAML      string
	Href                           string
	Extra                          []numItem // small numbers from the routine's calls (cost, turns, tools)
	On                             bool
}

type rLane struct {
	Title  string
	Blocks []rBlock
	Arrow  bool
}

type rRun struct {
	ID, Start, Status, Chip, Took string
	Decisions, Recurrences, Calls int
}

type routineData struct {
	ID, Fleet, FleetName, Title, Desc string
	Lamp, Chip, Status, Sub           string
	Cards                             []scoreCard
	Lanes, Chain                      []rLane
	Caps                              [][2]string
	Sel                               *rBlock
	Runs                              []rRun
	SpecURL, Back                     string
	Routine                           *api.RoutineStatus
	Counted                           string // the counts' window, as words
	Inst                              *instruments
	Panel                             *modelPanel
	Paths                             *pathSummary
}

var laneTitles = map[string]string{"when": "When", "where": "Where and what", "sort": "Sort", "agent": "The agent", "gates": "Gates", "outcome": "Outcome and action"}

// mixCounts is what the store counts for a routine's blocks over a window.
type mixCounts struct {
	runs        map[string]int
	recurrences int
	decided     int
	disp        map[string]int
	gate        map[string]int
	probes      map[string]int
	byDay       map[string]int // decided (not recurrences), by UTC day
	decs        []store.LedgerDecision
}

func (u *Server) routineCounts(ctx context.Context, id string, since time.Time) (mixCounts, map[string][2]int, error) {
	c := mixCounts{runs: map[string]int{}, disp: map[string]int{}, gate: map[string]int{}, probes: map[string]int{}, byDay: map[string]int{}}
	perRun := map[string][2]int{}
	runs, _, err := u.API.Store.RunsPage(ctx, store.RunQuery{Routine: id, Since: since.UTC().Format(time.RFC3339), Limit: 100000})
	if err != nil {
		return c, nil, err
	}
	for _, r := range runs {
		c.runs[r.Status]++
	}
	ds, err := u.API.Store.DecisionsSince(ctx, []string{id}, since.UTC().Format("2006-01-02"))
	if err != nil {
		return c, nil, err
	}
	c.decs = ds
	for _, d := range ds {
		pr := perRun[d.RunID]
		if mixRecurrence(d) {
			pr[1]++
			perRun[d.RunID] = pr
		} else {
			pr[0]++
			perRun[d.RunID] = pr
		}
		if !mixLive[d.Stage] {
			continue
		}
		if mixRecurrence(d) {
			c.recurrences++
			continue
		}
		c.decided++
		if len(d.TS) >= 10 {
			c.byDay[d.TS[:10]]++
		}
		c.disp[mixLabel(d.Disposition)]++
		if d.NProbes != "" {
			c.probes[d.NProbes]++
		}
		for _, g := range mixGates(d.Gates) {
			if g[0] != "operational" && g[1] != "passed" && g[1] != "n/a" {
				c.gate[g[0]]++
			}
		}
	}
	return c, perRun, nil
}

// gateLetter is a harness gate's letter in the ledger's gates column (signal-fleet/mvp/triage.py _GATES).
var gateLetter = map[string]string{"intent": "i", "cleanup": "c", "dismiss_corroborated": "d", "edge": "e"}

func (c mixCounts) show(what string) string {
	switch {
	case what == "runs":
		if len(c.runs) == 0 {
			return ""
		}
		var parts []string
		for _, s := range []string{"ok", "skipped", "cancelled", "failed", "error", "timeout"} {
			if c.runs[s] > 0 {
				parts = append(parts, fmt.Sprintf("%d %s", c.runs[s], s))
			}
		}
		return strings.Join(parts, " · ")
	case what == "recurrences":
		return fmt.Sprintf("%d recurrences", c.recurrences)
	case what == "decided":
		return fmt.Sprintf("%d decided", c.decided)
	case what == "probes":
		if len(c.probes) == 0 {
			return ""
		}
		var parts []string
		for _, k := range sortedKeys(c.probes) {
			parts = append(parts, fmt.Sprintf("%s×%d", k, c.probes[k]))
		}
		return "probes used: " + strings.Join(parts, " · ")
	case strings.HasPrefix(what, "disposition:"):
		return fmt.Sprintf("%d", c.disp[strings.TrimPrefix(what, "disposition:")])
	case strings.HasPrefix(what, "gate:"):
		l := gateLetter[strings.TrimPrefix(what, "gate:")]
		if l == "" {
			return ""
		}
		return fmt.Sprintf("fired %d", c.gate[l])
	}
	return ""
}

func firstSentence(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if i := strings.Index(s, ". "); i > 0 {
		s = s[:i+1]
	}
	if len(s) > max {
		s = strings.TrimRight(s[:max], " ,;:") + " …"
	}
	return s
}

func yamlOf(v any) string {
	b, err := yaml.Marshal(v)
	if err != nil {
		return ""
	}
	return strings.TrimRight(string(b), "\n")
}

// specBlocks draws a routine: its lanes, the blocks of its spec and of its steps, each with its text and counts.
func specBlocks(j *spec.Routine, c mixCounts, href func(string) string) []rLane {
	lanes := map[string][]rBlock{}
	add := func(lane string, b rBlock) {
		if b.Explain == "" {
			b.Explain = j.Explain[b.ID]
		}
		if b.Sub == "" {
			b.Sub = firstSentence(b.Explain, 72)
		}
		if b.Lamp == "" {
			b.Lamp = "blamp"
		}
		b.Href = href(b.ID)
		lanes[lane] = append(lanes[lane], b)
	}
	add("when", rBlock{ID: "trigger", Name: triggerText(j), Who: "code", Where: "spec", Key: "trigger", YAML: yamlOf(map[string]any{"trigger": j.Trigger}), Nums: c.show("runs")})
	if j.Freshness != "" {
		add("when", rBlock{ID: "freshness", Name: "stale after " + j.Freshness, Who: "code", Where: "spec", Key: "freshness", YAML: "freshness: " + j.Freshness})
	}
	add("where", rBlock{ID: "worker", Name: "worker " + j.Worker, Who: "code", Where: "spec", Key: "worker", YAML: "worker: " + j.Worker})
	cmd := strings.Fields(j.Run.Command)
	name := strings.Join(cmd, " ")
	if len(cmd) > 3 {
		name = strings.Join(cmd[:3], " ") + " …"
	}
	add("where", rBlock{ID: "run", Name: name, Who: "code", Where: "spec", Key: "run", YAML: yamlOf(map[string]any{"run": j.Run})})
	if len(j.Models) > 0 {
		add("where", rBlock{ID: "models", Name: "models " + strings.Join(j.Models, ", "), Who: "agent", Where: "spec", Key: "models", YAML: yamlOf(map[string]any{"models": j.Models})})
	}
	if len(j.Detects) > 0 {
		add("outcome", rBlock{ID: "detects", Name: "detects " + strings.Join(j.Detects, ", "), Who: "code", Where: "spec", Key: "detects", YAML: yamlOf(map[string]any{"detects": j.Detects})})
	}
	if len(j.Skills.Rule) > 0 {
		add("sort", rBlock{ID: "skills", Name: fmt.Sprintf("skills: %s", plural(len(j.Skills.Rule), "rule", "rules")), Who: "code", Where: "spec", Key: "skills.rule", YAML: yamlOf(map[string]any{"skills": j.Skills})})
	}
	if len(j.Prompts) > 0 {
		add("agent", rBlock{ID: "prompts", Name: "prompts: " + strings.Join(j.PromptRoles(), ", "), Who: "code", Where: "spec", Key: "prompts", YAML: yamlOf(map[string]any{"prompts": j.Prompts})})
	}
	if h := j.Harness; h != nil {
		add("agent", rBlock{ID: "harness.protocol", Name: "the model", Who: "agent", Where: "spec", Key: "harness.protocol", YAML: "harness:\n  protocol: " + h.Protocol})
		add("agent", rBlock{ID: "harness.limits", Name: fmt.Sprintf("up to %d turns", h.Limits.MaxSteps), Who: "code", Where: "spec", Key: "harness.limits",
			YAML: yamlOf(map[string]any{"harness": map[string]any{"limits": h.Limits}}), Nums: c.show("probes")})
		add("agent", rBlock{ID: "harness.tools", Name: fmt.Sprintf("tools (at most %d)", h.Limits.MaxToolCalls), Who: "tool", Where: "spec", Key: "harness.tools",
			YAML: yamlOf(map[string]any{"harness": map[string]any{"tools": h.Tools}})})
		if h.Quirks.FinalDecisionCall {
			add("agent", rBlock{ID: "harness.quirks", Name: "final decision call", Who: "code", Where: "spec", Key: "harness.quirks",
				YAML: yamlOf(map[string]any{"harness": map[string]any{"quirks": h.Quirks}})})
		}
		if h.Hooks != nil {
			add("agent", rBlock{ID: "harness.hooks", Name: "hooks", Who: "code", Where: "spec", Key: "harness.hooks",
				YAML: yamlOf(map[string]any{"harness": map[string]any{"hooks": h.Hooks}})})
		}
		for _, g := range h.Gates {
			add("gates", rBlock{ID: "gate." + g, Name: strings.ReplaceAll(g, "_", " "), Who: "code", Where: "spec", Key: "harness.gates", YAML: yamlOf(map[string]any{"harness": map[string]any{"gates": h.Gates}}), Nums: c.show("gate:" + g)})
		}
	}
	for _, s := range j.Steps {
		b := rBlock{ID: s.ID, Name: s.Title, Sub: s.Short, Who: s.Who, Explain: s.Explain, Where: "code", Key: "steps: " + s.ID, Nums: c.show(s.Count)}
		if s.Who == "you" {
			b.Lamp = "blamp blamp--wait"
		}
		if s.Count == "recurrences" || s.Count == "disposition:dismiss" {
			b.Lamp = "blamp blamp--idle"
		}
		add(s.Lane, b)
	}
	var out []rLane
	n := 0
	for _, l := range spec.Lanes {
		if len(lanes[l]) == 0 {
			continue
		}
		n++
		out = append(out, rLane{Title: fmt.Sprintf("%d · %s", n, laneTitles[l]), Blocks: lanes[l], Arrow: true})
	}
	if len(out) > 0 {
		out[len(out)-1].Arrow = false
	}
	return out
}

// via is what carries work to a routine when its chain link does not say: its trigger.
func via(j *spec.Routine) (string, string) {
	switch {
	case j == nil:
		return "", "code"
	case j.Trigger.Case != nil:
		return "a case " + j.Trigger.Case.Kind + ", from flotillad", "code"
	case j.Trigger.Execute != nil:
		return "a plan you approved: " + j.Trigger.Execute.Action, "you"
	case j.Trigger.Check != nil:
		return "a check: " + j.Trigger.Check.Verify, "code"
	}
	return "every " + j.Trigger.Every, "code"
}

func chainLanes(j *spec.Routine, by map[string]*spec.Routine, href func(string) string) []rLane {
	var out []rLane
	for i, c := range j.Chain {
		next := by[c.Routine]
		v, who := via(next)
		if c.Via != "" {
			v, who = c.Via, "code"
		}
		lamp := "blamp"
		if who == "you" {
			lamp = "blamp blamp--wait"
		}
		// the lane: what carries the work there, the routine, then the agents its code runs
		lane := rLane{Title: c.Routine, Arrow: i < len(j.Chain)-1,
			Blocks: []rBlock{{ID: fmt.Sprintf("via%d", i), Name: v, Sub: "then", Who: who, Lamp: lamp}}}
		if next != nil {
			text := next.Explain["run"]
			if text == "" {
				text = next.Description
			}
			b := rBlock{ID: c.Routine, Name: c.Routine, Sub: firstSentence(text, 72), Who: whoActs(next), Lamp: "blamp", Href: href("chain:" + c.Routine),
				Explain: strings.Join(strings.Fields(text), " "), Where: "spec", Key: "fleets/" + c.Routine + ".yaml"}
			lane.Blocks = append(lane.Blocks, b)
			for _, s := range next.Steps {
				if s.Who == "agent" {
					lane.Blocks = append(lane.Blocks, rBlock{ID: c.Routine + "#" + s.ID, Name: s.Title, Sub: s.Short, Who: "agent", Lamp: "blamp",
						Explain: s.Explain, Where: "code", Key: c.Routine + " steps: " + s.ID, Href: href("chain:" + c.Routine + "#" + s.ID)})
				}
			}
		}
		out = append(out, lane)
	}
	return out
}

func (u *Server) routinePage(w http.ResponseWriter, r *http.Request, s *store.Session, sid string) {
	ctx := r.Context()
	id := r.PathValue("fleet") + "/" + r.PathValue("routine")
	_, specsBy := u.specs()
	j := specsBy[id]
	if j == nil {
		http.NotFound(w, r)
		return
	}
	rows, err := u.caseRows(ctx)
	if err != nil {
		http.Error(w, "cases failed", 500)
		return
	}
	fs, _, _, err := u.fleetSums(ctx, rows)
	if err != nil {
		http.Error(w, "status failed", 500)
		return
	}
	var st *api.RoutineStatus
	var f *fleetSum
	for i := range fs {
		for k := range fs[i].Routines {
			if fs[i].Routines[k].ID == id {
				st, f = &fs[i].Routines[k], &fs[i]
			}
		}
	}
	if st == nil {
		http.NotFound(w, r)
		return
	}
	now := u.clockNow()
	window := 14
	c, perRun, err := u.routineCounts(ctx, id, now.AddDate(0, 0, -window))
	if err != nil {
		http.Error(w, "counts failed", 500)
		return
	}
	base := "/fleets/" + id
	sel := r.URL.Query().Get("b")
	href := func(b string) string { return base + "?b=" + b + "#block" }
	d := routineData{ID: id, Fleet: j.Fleet, FleetName: f.Name, Title: id, Desc: strings.Join(strings.Fields(j.Description), " "),
		SpecURL: "https://github.com/chipi/flotilla/blob/main/fleets/" + id + ".yaml", Back: r.URL.RequestURI(), Routine: st,
		Counted: fmt.Sprintf("the last %d days", window)}
	lamp := lampOf(*st, f.LastFailed[id])
	d.Lamp = "lamp lamp--big" + map[string]string{"ok": "", "wait": " lamp--wait", "bad": " lamp--stop", "idle": " lamp--idle"}[lamp]
	d.Status = map[string]string{"ok": "ok", "wait": "paused", "bad": "failing", "idle": "idle"}[lamp]
	d.Chip = "chip " + map[string]string{"ok": "chip--ok", "wait": "chip--wait", "bad": "chip--stop", "idle": "chip--neutral"}[lamp]
	d.Sub = fmt.Sprintf("%s on worker %s · autonomy %s · budget %s a day", triggerText(j), j.Worker, st.Autonomy, usd2(st.BudgetDay))
	d.Lanes = specBlocks(j, c, href)
	d.Chain = chainLanes(j, specsBy, href)
	d.Caps = [][2]string{{"Autonomy", fmt.Sprintf("spec %s, set to %s", j.Autonomy, st.Autonomy)}, {"Budget", usd2(j.Budget.DayUSD) + " a day"}}
	if t := j.Explain["autonomy"]; t != "" {
		d.Caps[0][1] += ": " + t
	}
	if t := j.Explain["budget"]; t != "" {
		d.Caps[1][1] += ": " + t
	}
	for li := range d.Lanes {
		for bi := range d.Lanes[li].Blocks {
			if b := &d.Lanes[li].Blocks[bi]; b.ID == sel {
				b.On, d.Sel = true, b
			}
		}
	}
	for li := range d.Chain {
		for bi := range d.Chain[li].Blocks {
			if b := &d.Chain[li].Blocks[bi]; "chain:"+b.ID == sel {
				b.On, d.Sel = true, b
			}
		}
	}
	if err := u.routineModel(ctx, &d, j, id, c, now, window); err != nil {
		http.Error(w, "calls failed", 500)
		return
	}
	ps, err := u.routinePaths(ctx, id)
	if err != nil {
		http.Error(w, "paths failed", 500)
		return
	}
	d.Paths = ps
	// today's cards
	day0 := time.Date(now.UTC().Year(), now.UTC().Month(), now.UTC().Day(), 0, 0, 0, 0, time.UTC)
	today, _, err := u.API.Store.RunsPage(ctx, store.RunQuery{Routine: id, Since: day0.Format(time.RFC3339), Limit: 100000})
	if err != nil {
		http.Error(w, "runs failed", 500)
		return
	}
	n, okN, skip, cancelled, fail, took := len(today), 0, 0, 0, 0, 0.0
	for _, x := range today {
		switch x.Status {
		case "ok":
			okN++
		case "skipped":
			skip++
		case "cancelled":
			cancelled++
		case "failed", "error", "timeout":
			fail++
		}
		if x.DurationS != nil && x.Status == "ok" {
			took += *x.DurationS
		}
	}
	avg := "–"
	if okN > 0 {
		avg = fmt.Sprintf("%.0f s", took/float64(okN))
	}
	lc, err := u.API.Store.LedgerCounts(ctx, id)
	if err != nil {
		http.Error(w, "ledger failed", 500)
		return
	}
	d.Cards = []scoreCard{
		{Label: "Runs today", Value: fmt.Sprint(n), Unit: fmt.Sprintf("%d ok · %d skipped%s · %d failed", okN, skip, cancelledText(cancelled), fail), Delta: "since 00:00 UTC", Tone: map[bool]string{true: "bad", false: "neutral"}[fail > 0], Arrow: "■", Spark: makeSpark(nil)},
		{Label: "A run takes", Value: avg, Unit: "on average today", Delta: "of a " + j.Run.Timeout + " limit", Tone: "neutral", Arrow: "■", Spark: makeSpark(nil)},
	}
	if lc.Decisions > 0 {
		d.Cards = append(d.Cards, scoreCard{Label: "Decisions in the ledger", Value: fmt.Sprintf("%d", lc.Decisions), Unit: fmt.Sprintf("%d recurrences", lc.Recurrences), Delta: "the Decisions tab counts them by day", Tone: "neutral", Arrow: "■", Spark: makeSpark(nil)})
	}
	// last runs, with their decisions and calls
	last, err := u.API.Store.Runs(ctx, id, 12)
	if err != nil {
		http.Error(w, "runs failed", 500)
		return
	}
	ids := make([]string, 0, len(last))
	for _, x := range last {
		ids = append(ids, x.ID)
	}
	calls, err := u.API.Store.RunCalls(ctx, ids)
	if err != nil {
		http.Error(w, "calls failed", 500)
		return
	}
	for _, x := range last {
		t := "–"
		if x.DurationS != nil {
			t = fmt.Sprintf("%.0f s", *x.DurationS)
		}
		chip := map[string]string{"ok": "chip--ok", "skipped": "chip--neutral", "cancelled": "chip--neutral"}[x.Status]
		if chip == "" {
			chip = "chip--stop"
		}
		pr := perRun[x.ID]
		d.Runs = append(d.Runs, rRun{ID: x.ID, Start: u.clock(x.StartedAt), Status: x.Status, Chip: chip, Took: t, Decisions: pr[0], Recurrences: pr[1], Calls: realCalls(calls[x.ID])})
	}
	sort.SliceStable(d.Runs, func(a, b int) bool { return d.Runs[a].Start > d.Runs[b].Start })
	u.render(w, "routine.html", 200, u.pageFor(s, id, "Fleets", r, d))
}

// cancelledText is the " · N cancelled" a day's run counts carry only when a run was cancelled.
func cancelledText(n int) string {
	if n == 0 {
		return ""
	}
	return fmt.Sprintf(" · %d cancelled", n)
}
