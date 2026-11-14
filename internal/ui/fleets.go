package ui

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/chipi/flotilla/internal/api"
	"github.com/chipi/flotilla/internal/spec"
	"github.com/chipi/flotilla/internal/store"
)

// fleetSum is one fleet as Now's aside, Fleets' cards and a fleet's header show it.
type fleetSum struct {
	ID, Name                 string
	Status, Chip, Lamp, Note string // healthy | needs you | degraded | paused
	Routines                 []api.RoutineStatus
	LastFailed               map[string]bool
	Stopped                  bool
	Cases24, Waiting         int
	Spend24, Budget          float64
	Runs24, Failed24         int
	Last                     string
	RunsPerDay               []float64
	Spark                    spark
	Workers                  []string
	Purpose                  string
}

func (f fleetSum) SpendText() string { return usd2(f.Spend24) }

// fleetSums summarises every fleet from the routines' state, the last 14 days of runs and the cases.
func (u *Server) fleetSums(ctx context.Context, rows []caseRow) ([]fleetSum, []api.RoutineStatus, map[string]any, error) {
	routines, workers, err := u.API.Status(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	now := u.clockNow()
	ds := days(now, 14)
	rd, err := u.API.Store.RunDays(ctx, ds[0])
	if err != nil {
		return nil, nil, nil, err
	}
	day24 := now.Add(-24 * time.Hour).UTC().Format(time.RFC3339)
	recent, _, err := u.API.Store.RunsPage(ctx, store.RunQuery{Since: day24, Limit: 100000, Desc: true})
	if err != nil {
		return nil, nil, nil, err
	}
	by := map[string]*fleetSum{}
	var order []string
	for _, x := range routines {
		f := by[x.Fleet]
		if f == nil {
			f = &fleetSum{ID: x.Fleet, Name: title(x.Fleet), Stopped: x.Stopped, LastFailed: map[string]bool{}, RunsPerDay: make([]float64, len(ds))}
			by[x.Fleet] = f
			order = append(order, x.Fleet)
		}
		f.Routines = append(f.Routines, x)
		f.Budget += x.BudgetDay
		if x.Worker != "" && !contains(f.Workers, x.Worker) {
			f.Workers = append(f.Workers, x.Worker)
		}
		last, err := u.API.Store.Runs(ctx, x.ID, 1)
		if err != nil {
			return nil, nil, nil, err
		}
		if len(last) == 1 {
			switch last[0].Status {
			case store.StatusError, store.StatusTimeout, store.StatusInterrupted:
				f.LastFailed[x.ID] = true
			}
		}
	}
	idx := map[string]int{}
	for i, d := range ds {
		idx[d] = i
	}
	for _, d := range rd {
		if f := by[fleetOf(d.Routine)]; f != nil {
			if i, ok := idx[d.Day]; ok {
				f.RunsPerDay[i] += float64(d.Runs)
			}
		}
	}
	for _, x := range recent {
		f := by[fleetOf(x.Routine)]
		if f == nil {
			continue
		}
		f.Runs24++
		f.Spend24 += x.SpendUSD
		switch x.Status {
		case store.StatusError, store.StatusTimeout, store.StatusInterrupted:
			f.Failed24++
		}
		if x.StartedAt > f.Last {
			f.Last = x.StartedAt
		}
	}
	for _, c := range rows {
		f := by[c.Fleet]
		if f == nil {
			continue
		}
		if c.OpenedAt >= day24 {
			f.Cases24++
		}
		if waiting(c.Status) {
			f.Waiting++
		}
	}
	out := make([]fleetSum, 0, len(order))
	sort.Strings(order)
	for _, id := range order {
		f := by[id]
		f.Spark = makeSpark(f.RunsPerDay)
		f.Spark.Label = "Runs a day over the last 14 days; dashed line is the first week's average"
		names := make([]string, 0, len(f.Routines))
		enabled, paused, bad := 0, 0, ""
		for _, x := range f.Routines {
			names = append(names, strings.TrimPrefix(x.ID, id+"/"))
			if !x.Enabled {
				continue
			}
			enabled++
			if x.Paused {
				paused++
			}
			if bad == "" && x.Stale {
				bad = x.ID + " is stale"
			}
			if bad == "" && f.LastFailed[x.ID] {
				bad = x.ID + "'s last run failed"
			}
		}
		// G6: fleets have no description of their own yet, so the purpose is what they are made of.
		f.Purpose = plural(len(f.Routines), "routine", "routines") + ": " + strings.Join(names, ", ")
		last := "no run yet"
		if f.Last != "" {
			last = "last run " + u.clock(f.Last)
		}
		switch {
		case f.Stopped || enabled == 0 || paused == enabled:
			f.Status, f.Chip, f.Lamp = "paused", "chip chip--neutral chip--small", "lamp lamp--idle"
			f.Note = "paused"
			if f.Stopped {
				f.Note = "STOPped"
			} else if enabled == 0 {
				f.Note = "every routine disabled"
			}
		case bad != "":
			f.Status, f.Chip, f.Lamp, f.Note = "degraded", "chip chip--stop chip--small", "lamp lamp--stop", "degraded · "+bad
		case f.Waiting > 0:
			f.Status, f.Chip, f.Lamp = "needs you", "chip chip--wait chip--small", "lamp lamp--wait"
			f.Note = "needs you · " + plural(f.Waiting, "case waits", "cases wait")
		default:
			f.Status, f.Chip, f.Lamp, f.Note = "healthy", "chip chip--ok chip--small", "lamp", "healthy · "+last
		}
		out = append(out, *f)
	}
	return out, routines, workers, nil
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// --- Fleets ---

type fleetsData struct {
	Fleets   []fleetSum
	Tabs     []pillLink
	T        tableQ
	Total    int
	Routines int
	Stopped  bool // every fleet is STOPped
}

type pillLink struct {
	Label, Href string
	N           int
	HasN        bool
	On          bool
}

func (u *Server) fleetsPage(w http.ResponseWriter, r *http.Request, s *store.Session, sid string) {
	ctx := r.Context()
	rows, err := u.caseRows(ctx)
	if err != nil {
		http.Error(w, "cases failed", 500)
		return
	}
	fs, routines, _, err := u.fleetSums(ctx, rows)
	if err != nil {
		http.Error(w, "status failed", 500)
		return
	}
	t := readTable(r, "", false, func(string) bool { return false })
	st, q := t.Get("status"), strings.ToLower(t.Get("q"))
	d := fleetsData{T: t, Total: len(fs), Routines: len(routines), Stopped: len(fs) > 0}
	for _, f := range fs {
		if !f.Stopped {
			d.Stopped = false
		}
	}
	count := map[string]int{}
	for _, f := range fs {
		count[f.Status]++
		if (st == "" || f.Status == st) && (q == "" || strings.Contains(strings.ToLower(f.ID+" "+f.Purpose), q)) {
			d.Fleets = append(d.Fleets, f)
		}
	}
	for _, p := range [][2]string{{"", "All"}, {"needs you", "Needs you"}, {"degraded", "Degraded"}, {"healthy", "Healthy"}, {"paused", "Paused"}} {
		n := count[p[0]]
		if p[0] == "" {
			n = len(fs)
		}
		d.Tabs = append(d.Tabs, pillLink{Label: p[1], Href: t.With("status", p[0]), N: n, HasN: true, On: st == p[0]})
	}
	u.render(w, "fleets.html", 200, u.pageFor(s, "Fleets", "Fleets", r, d))
}

// stopAll is "STOP all routines" and "Release STOP" on Fleets: every fleet, through the same calls.
func (u *Server) stopAll(stop bool) func(http.ResponseWriter, *http.Request, *store.Session, string) {
	return func(w http.ResponseWriter, r *http.Request, s *store.Session, sid string) {
		routines, _, err := u.API.Status(r.Context())
		if err != nil {
			done(w, r, "/fleets", err, "")
			return
		}
		seen := map[string]bool{}
		for _, x := range routines {
			if seen[x.Fleet] || x.Stopped == stop {
				seen[x.Fleet] = true
				continue
			}
			seen[x.Fleet] = true
			if err := u.API.SetStopped(r.Context(), caller(s), "ui", x.Fleet, stop, r.PostFormValue("reason")); err != nil {
				done(w, r, "/fleets", err, "")
				return
			}
		}
		ok := "STOP is on: no routine starts; runs in flight finish their current case."
		if !stop {
			ok = "STOP is released: every fleet's routines start on their triggers again."
		}
		done(w, r, "/fleets", nil, ok)
	}
}

// --- Fleet ---

type boardNode struct {
	ID, Label, Sub, A, B, BClass, Lamp, Href string
	On                                       bool
	Arrow                                    bool
}

type boardStage struct {
	Title string
	Nodes []boardNode
	None  string
}

type routineRow struct {
	api.RoutineStatus
	Trigger, Last, LastHref, Week, Spend string
}

type lastRun struct{ ID, When, Result string }

type selected struct {
	ID, Label, Desc, Lamp string
	Facts                 [][2]string
	Runs                  []lastRun
	Log                   string
	Routine               *api.RoutineStatus
	SpecURL               string
}

type fleetData struct {
	F       fleetSum
	Tab     string
	Tabs    []pillLink
	Stages  []boardStage
	Sel     *selected
	Rows    []routineRow
	T       tableQ
	Cards   []scoreCard
	Log     string
	Back    string
	CanStop bool
}

// triggerText writes a routine's trigger the way the mockup does.
func triggerText(j *spec.Routine) string {
	switch {
	case j == nil:
		return "–"
	case j.Trigger.Case != nil:
		return "case handed over: " + j.Trigger.Case.Kind
	case j.Trigger.Check != nil:
		return "case check due: " + j.Trigger.Check.Verify
	case j.Trigger.Every != "":
		return "every " + j.Trigger.Every
	}
	return "by hand"
}

func (u *Server) fleetPage(w http.ResponseWriter, r *http.Request, s *store.Session, sid string) {
	ctx := r.Context()
	rows, err := u.caseRows(ctx)
	if err != nil {
		http.Error(w, "cases failed", 500)
		return
	}
	fs, _, workers, err := u.fleetSums(ctx, rows)
	if err != nil {
		http.Error(w, "status failed", 500)
		return
	}
	var f *fleetSum
	for i := range fs {
		if fs[i].ID == r.PathValue("fleet") {
			f = &fs[i]
		}
	}
	if f == nil {
		http.NotFound(w, r)
		return
	}
	_, specsBy := u.specs()
	tab := r.URL.Query().Get("tab")
	switch tab {
	case "routines", "stats", "logs":
	default:
		tab = "workflow"
	}
	base := "/fleets/" + f.ID
	d := fleetData{F: *f, Tab: tab, Back: r.URL.RequestURI(), CanStop: true}
	for _, p := range [][2]string{{"workflow", "Workflow"}, {"routines", "Routines"}, {"stats", "Costs and stats"}, {"logs", "Logs"}} {
		href := base
		if p[0] != "workflow" {
			href += "?tab=" + p[0]
		}
		d.Tabs = append(d.Tabs, pillLink{Label: p[1], Href: href, On: tab == p[0]})
	}
	now := u.clockNow()
	day24 := now.Add(-24 * time.Hour).UTC().Format(time.RFC3339)
	recent, _, err := u.API.Store.RunsPage(ctx, store.RunQuery{Fleet: f.ID, Since: day24, Limit: 100000, Desc: true})
	if err != nil {
		http.Error(w, "runs failed", 500)
		return
	}
	runs24, spend24 := map[string]int{}, map[string]float64{}
	for _, x := range recent {
		runs24[x.Routine]++
		spend24[x.Routine] += x.SpendUSD
	}
	waitingKind := map[string]int{}
	for _, c := range rows {
		if waiting(c.Status) {
			waitingKind[c.Kind]++
		}
	}
	switch tab {
	case "workflow":
		sel := r.URL.Query().Get("sel")
		node := func(x api.RoutineStatus, arrow bool) boardNode {
			j := specsBy[x.ID]
			lamp := lampOf(x, f.LastFailed[x.ID])
			n := boardNode{ID: x.ID, Label: x.ID, Sub: triggerText(j), A: plural(runs24[x.ID], "run", "runs"),
				Lamp: "blamp blamp--" + lamp, Href: base + "?sel=" + url.QueryEscape(x.ID), On: x.ID == sel, Arrow: arrow, BClass: "soft"}
			switch {
			case !x.Enabled:
				n.B = "disabled"
			case x.Paused:
				n.B, n.BClass = "paused", "wait"
			case x.Stale:
				n.B, n.BClass = "stale", "bad"
			case f.LastFailed[x.ID]:
				n.B, n.BClass = "last failed", "bad"
			case j != nil && j.OnCase() != "" && waitingKind[j.OnCase()] > 0:
				n.B, n.BClass = fmt.Sprintf("waiting %d", waitingKind[j.OnCase()]), "wait"
			case x.Running != "":
				n.B = "running"
			default:
				n.B = "ok"
			}
			return n
		}
		stages := []boardStage{{Title: "1 · Sources"}, {Title: "2 · Monitors"}, {Title: "3 · Remediation"}, {Title: "4 · Decide and act"}, {Title: "5 · Verify and report"}}
		for _, wk := range f.Workers {
			lamp, b, bc := "blamp", "healthy", "soft"
			if m, ok := workers[wk].(map[string]any); ok && m["ok"] != true {
				lamp, b, bc = "blamp blamp--bad", "not healthy", "bad"
			}
			stages[0].Nodes = append(stages[0].Nodes, boardNode{Label: "worker " + wk, Sub: "the access its routines read through", A: "", B: b, BClass: bc, Lamp: lamp, Arrow: true})
		}
		for _, x := range f.Routines {
			j := specsBy[x.ID]
			switch {
			case j != nil && j.Trigger.Case != nil:
				stages[2].Nodes = append(stages[2].Nodes, node(x, true))
			case j != nil && j.Trigger.Check != nil:
				stages[4].Nodes = append(stages[4].Nodes, node(x, false))
			case j != nil && len(j.Detects) > 0:
				stages[1].Nodes = append(stages[1].Nodes, node(x, true))
			default:
				stages[4].Nodes = append(stages[4].Nodes, node(x, false))
			}
		}
		decided, asked := 0, 0
		for _, c := range rows {
			if c.Fleet == f.ID && c.Last.At >= day24 && (c.Last.By == "policy") {
				decided++
			}
			if c.Fleet == f.ID && waiting(c.Status) {
				asked++
			}
		}
		pol := boardNode{Label: "policy", Sub: "flotillad: run, ask you, or refuse", A: fmt.Sprintf("decided %d", decided), Lamp: "blamp", BClass: "soft", Arrow: true}
		if asked > 0 {
			pol.B, pol.BClass, pol.Lamp = fmt.Sprintf("asked you %d", asked), "wait", "blamp blamp--wait"
		}
		hosts, _ := u.API.Store.Hosts(ctx)
		up, down := 0, 0
		for _, h := range hosts {
			if h.RevokedAt != nil || h.EnrolledAt == nil {
				continue
			}
			if h.Connected {
				up++
			} else {
				down++
			}
		}
		ha := boardNode{Label: "host agents", Sub: fmt.Sprintf("%d connected", up), Lamp: "blamp", BClass: "soft"}
		switch {
		case up+down == 0:
			ha.Sub, ha.B, ha.Lamp = "none enrolled", "idle", "blamp blamp--idle"
		case down > 0:
			ha.B, ha.BClass, ha.Lamp = fmt.Sprintf("%d down", down), "bad", "blamp blamp--bad"
		}
		stages[3].Nodes = []boardNode{pol, ha}
		for i := range stages {
			if len(stages[i].Nodes) == 0 {
				stages[i].None = "None in this fleet."
			}
		}
		d.Stages = stages
		// The selected block: the one asked for, else the first routine with a problem, else the first.
		var pick *api.RoutineStatus
		for i := range f.Routines {
			if f.Routines[i].ID == sel {
				pick = &f.Routines[i]
			}
		}
		if pick == nil {
			for i := range f.Routines {
				if lampOf(f.Routines[i], f.LastFailed[f.Routines[i].ID]) == "bad" {
					pick = &f.Routines[i]
					break
				}
			}
		}
		if pick == nil && len(f.Routines) > 0 {
			pick = &f.Routines[0]
		}
		if pick != nil {
			for si := range d.Stages {
				for ni := range d.Stages[si].Nodes {
					d.Stages[si].Nodes[ni].On = d.Stages[si].Nodes[ni].ID == pick.ID
				}
			}
			j := specsBy[pick.ID]
			lamp := lampOf(*pick, f.LastFailed[pick.ID])
			status := map[string]string{"ok": "running normally", "wait": "paused", "bad": "failing", "idle": "idle"}[lamp]
			nowText := "idle"
			if pick.Running != "" {
				nowText = "running"
			}
			sl := &selected{ID: pick.ID, Label: pick.ID, Desc: pick.Description, Lamp: "lamp lamp--" + map[string]string{"ok": "", "wait": "wait", "bad": "stop", "idle": "idle"}[lamp],
				Routine: pick, SpecURL: "https://github.com/chipi/flotilla/blob/main/fleets/" + pick.ID + ".yaml"}
			if lamp == "ok" {
				sl.Lamp = "lamp"
			}
			sl.Facts = [][2]string{{"Trigger", triggerText(j)}, {"Status", status}, {"Last 24h", plural(runs24[pick.ID], "run", "runs")},
				{"Now", nowText}, {"Worker", orDash(pick.Worker)}, {"Spend 24h", usd2(spend24[pick.ID])},
				{"Autonomy", pick.Autonomy}, {"Spend today", usd2(pick.SpendToday) + " of " + usd2(pick.BudgetDay)}}
			last, err := u.API.Store.Runs(ctx, pick.ID, 3)
			if err != nil {
				http.Error(w, "runs failed", 500)
				return
			}
			for _, x := range last {
				sl.Runs = append(sl.Runs, lastRun{ID: x.ID, When: u.clock(x.StartedAt), Result: x.Status})
			}
			if len(last) > 0 {
				sl.Log = tailLines(last[0].Detail, 8)
			}
			d.Sel = sl
		}
	case "routines":
		t := readTable(r, "routine", false, func(c string) bool {
			return c == "routine" || c == "worker" || c == "autonomy" || c == "last" || c == "spend"
		})
		weekStart := now.AddDate(0, 0, -7).UTC().Format(time.RFC3339)
		var out []routineRow
		for _, x := range f.Routines {
			j := specsBy[x.ID]
			week, _, err := u.API.Store.RunsPage(ctx, store.RunQuery{Routine: x.ID, Since: weekStart, Limit: 100000, Desc: true})
			if err != nil {
				http.Error(w, "runs failed", 500)
				return
			}
			bad := 0
			for _, k := range week {
				switch k.Status {
				case store.StatusError, store.StatusTimeout, store.StatusInterrupted:
					bad++
				}
			}
			rr := routineRow{RoutineStatus: x, Trigger: triggerText(j), Spend: usd2(spend24[x.ID]), Last: "never"}
			if len(week) > 0 {
				rr.Last = u.clock(week[0].StartedAt) + " " + week[0].Status
				rr.LastHref = "/runs/" + week[0].ID
			}
			rr.Week = plural(len(week), "run", "runs")
			if bad > 0 {
				rr.Week += fmt.Sprintf(" · %d failed", bad)
			} else if len(week) > 0 {
				rr.Week += " · ok"
			}
			out = append(out, rr)
		}
		key := func(x routineRow) string {
			switch t.Sort {
			case "worker":
				return x.Worker
			case "autonomy":
				return x.Autonomy
			case "last":
				return x.Last
			case "spend":
				return fmt.Sprintf("%012.4f", spend24[x.ID])
			}
			return x.ID
		}
		sort.SliceStable(out, func(i, j int) bool {
			if t.Desc {
				return key(out[i]) > key(out[j])
			}
			return key(out[i]) < key(out[j])
		})
		d.Rows = window(out, &t)
		d.T = t
	case "stats":
		cards, err := u.fleetCards(ctx, *f, rows)
		if err != nil {
			http.Error(w, "stats failed", 500)
			return
		}
		d.Cards = cards
	case "logs":
		last, _, err := u.API.Store.RunsPage(ctx, store.RunQuery{Fleet: f.ID, Limit: 30, Desc: true})
		if err != nil {
			http.Error(w, "runs failed", 500)
			return
		}
		var b strings.Builder
		for _, x := range last {
			t, _ := parseTime(x.StartedAt)
			fmt.Fprintf(&b, "%s %-6s %s  %s\n", t.In(u.loc()).Format("Jan 2 15:04:05"), strings.ToUpper(x.Status), x.Routine, x.ID)
			if x.Detail != "" {
				for _, l := range strings.Split(tailLines(x.Detail, 3), "\n") {
					b.WriteString("    " + l + "\n")
				}
			}
		}
		d.Log = b.String()
	}
	u.render(w, "fleet.html", 200, u.pageFor(s, f.Name, "Fleets", r, d))
}

func tailLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// fleetCards are the Costs and stats tab's eight cards.
func (u *Server) fleetCards(ctx context.Context, f fleetSum, rows []caseRow) ([]scoreCard, error) {
	now := u.clockNow()
	touched, err := u.touchedByYou(ctx)
	if err != nil {
		return nil, err
	}
	ds := days(now, 14)
	idx := map[string]int{}
	for i, d := range ds {
		idx[d] = i
	}
	opened, fixed, spend, runs, failed := make([]float64, 14), make([]float64, 14), make([]float64, 14), make([]float64, 14), make([]float64, 14)
	var closedTimes []time.Duration
	for _, c := range rows {
		if c.Fleet != f.ID {
			continue
		}
		if t, ok := parseTime(c.OpenedAt); ok {
			if i, ok := idx[t.UTC().Format("2006-01-02")]; ok {
				opened[i]++
			}
		}
		if c.ClosedAt != nil && c.Resolution == "verified" {
			if t, ok := parseTime(*c.ClosedAt); ok {
				if i, ok := idx[t.UTC().Format("2006-01-02")]; ok {
					fixed[i]++
				}
				if o, ok := parseTime(c.OpenedAt); ok && now.Sub(t) < 7*24*time.Hour {
					closedTimes = append(closedTimes, t.Sub(o))
				}
			}
		}
	}
	rd, err := u.API.Store.RunDays(ctx, ds[0])
	if err != nil {
		return nil, err
	}
	for _, d := range rd {
		if fleetOf(d.Routine) != f.ID {
			continue
		}
		if i, ok := idx[d.Day]; ok {
			runs[i] += float64(d.Runs)
			failed[i] += float64(d.Failed)
			spend[i] += d.Spend
		}
	}
	failPct := make([]float64, 14)
	for i := range runs {
		if runs[i] > 0 {
			failPct[i] = failed[i] / runs[i] * 100
		}
	}
	median := "–"
	if len(closedTimes) > 0 {
		sort.Slice(closedTimes, func(i, j int) bool { return closedTimes[i] < closedTimes[j] })
		median = span(closedTimes[len(closedTimes)/2])
	}
	spendCard := countCard("Spend", "last 24h", spend, usd2(f.Spend24), lessIsBetter)
	failCard := countCard("Failed runs", "last 24h", failPct, "", lessIsBetter)
	if f.Runs24 > 0 {
		failCard.Value = fmt.Sprintf("%.1f%%", float64(f.Failed24)/float64(f.Runs24)*100)
	} else {
		failCard.Value = "–"
	}
	calls, err := u.API.Store.CallsSince(ctx, now.AddDate(0, 0, -14).UTC().Format(time.RFC3339))
	if err != nil {
		return nil, err
	}
	tok := make([]float64, 14)
	var tok24 int64
	for _, c := range calls {
		if fleetOf(c.Routine) != f.ID {
			continue
		}
		t, ok := parseTime(c.At)
		if !ok {
			continue
		}
		in, out := callTokens(c)
		if i, ok := idx[t.UTC().Format("2006-01-02")]; ok {
			tok[i] += float64(in + out)
		}
		if now.Sub(t) < 24*time.Hour {
			tok24 += in + out
		}
	}
	tokens := countCard("Tokens", "last 24h", tok, tokensText(tok24), nil)
	return []scoreCard{
		countCard("Cases opened", "last 24h", opened, fmt.Sprint(f.Cases24), nil),
		countCard("Fixed and verified", "last 24h", fixed, "", moreIsBetter),
		closedWithoutYou(rows, touched, f.ID, now),
		{Label: "Time to fix", Value: median, Unit: "median, 7 days", Delta: "from opening to a verified close", Tone: "neutral", Arrow: "■", Spark: makeSpark(fixed)},
		spendCard,
		tokens,
		countCard("Runs", "last 24h", runs, fmt.Sprint(f.Runs24), nil),
		failCard,
	}, nil
}
