package ui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/chipi/flotilla/internal/cases"
	"github.com/chipi/flotilla/internal/models"
	"github.com/chipi/flotilla/internal/store"
)

// Models and spend (mockup v3's Models page, G1): the installation's model aliases, and every model call
// a run made as the model client recorded it. "Local" is an alias the installation declares with
// cost: zero. "Check now" is the newest contract check (G15: platform/model-contract, daily and on
// demand), or the newest real call while an alias has no check yet.

func tokensText(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.2fM", float64(n)/1e6)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	}
	return fmt.Sprint(n)
}

func callTokens(c store.ModelCall) (in, out int64) {
	if c.TokensIn != nil {
		in = *c.TokensIn
	}
	if c.TokensOut != nil {
		out = *c.TokensOut
	}
	return in, out
}

type aliasInfo struct {
	Name, Endpoint, Model, Caps string
	Local                       bool
	PricePerM                   string // declared (cost: table) or "–"
}

// aliases reads the installation's models section (flotillad.yaml, as the runs get it).
func (u *Server) aliases() map[string]aliasInfo {
	out := map[string]aliasInfo{}
	if u.API.Sched == nil || u.API.Sched.ModelsJSON == "" {
		return out
	}
	var cfg models.Config
	if json.Unmarshal([]byte(u.API.Sched.ModelsJSON), &cfg) != nil {
		return out
	}
	for name, a := range cfg.Aliases {
		d := cfg.Endpoints[a.Endpoint].Defaults
		pick := func(v, def string) string {
			if v != "" {
				return v
			}
			return def
		}
		tools, js, cost := pick(a.Tools, d.Tools), pick(a.JSON, d.JSON), pick(a.Cost, d.Cost)
		info := aliasInfo{Name: name, Endpoint: a.Endpoint, Model: a.Model, Local: cost == "zero", PricePerM: "–",
			Caps: "tools " + pick(tools, "none") + " · json " + pick(js, "none") + " · cost " + pick(cost, "unknown")}
		if p, ok := cfg.Prices[name]; ok && p.InputPerMTok != nil && p.OutputPerMTok != nil {
			info.PricePerM = fmt.Sprintf("$%.2f in · $%.2f out", *p.InputPerMTok, *p.OutputPerMTok)
		}
		out[name] = info
	}
	return out
}

type invRow struct {
	aliasInfo
	Engine, LastCall, LastClass, P50, UsedBy, Effective string
	Calls                                               int
	Checks                                              []string // the last contract checks, newest first (G15)
	CheckFailed                                         bool
}

// ContractRoutine runs the contract checks (fleets/platform/model-contract/).
const ContractRoutine = "platform/model-contract"

// checkRun is one contract-check run's results for one alias.
type checkRun struct {
	At     string
	Names  []string
	Failed []string
}

type spendRow struct {
	Name, Tokens, IO, Local, Spend, Per string
	Requests, Share                     int
	Spark                               spark
	tokens                              int64
}

type modelsData struct {
	Period        string
	Periods, Tabs []pillLink
	Tab           string
	Cards         []scoreCard
	Inventory     []invRow
	Sel           *invRow
	SelRoutines   []string
	ByFleet       []spendRow
	ByModel       []spendRow
	Unpriced      int
	HasCalls      bool
	CanCheck      bool // the contract routine is loaded
}

func (u *Server) modelsPage(w http.ResponseWriter, r *http.Request, s *store.Session, sid string) {
	ctx := r.Context()
	now := u.clockNow()
	q := r.URL.Query()
	period := q.Get("period")
	var since time.Time
	switch period {
	case "24h":
		since = now.Add(-24 * time.Hour)
	case "30d":
		since = now.AddDate(0, 0, -30)
	case "month":
		n := now.In(u.loc())
		since = time.Date(n.Year(), n.Month(), 1, 0, 0, 0, 0, u.loc())
	default:
		period, since = "7d", now.AddDate(0, 0, -7)
	}
	tab := q.Get("tab")
	if tab != "spend" {
		tab = "inv"
	}
	t := readTable(r, "", false, func(string) bool { return false })
	d := modelsData{Period: period, Tab: tab}
	if u.API.Sched != nil {
		for _, j := range u.API.Sched.Routines() {
			d.CanCheck = d.CanCheck || j.ID() == ContractRoutine
		}
	}
	for _, p := range [][2]string{{"24h", "Last 24 hours"}, {"7d", "Last 7 days"}, {"30d", "Last 30 days"}, {"month", "This month"}} {
		d.Periods = append(d.Periods, pillLink{Label: p[1], Href: t.With("period", p[0]), On: period == p[0]})
	}
	for _, p := range [][2]string{{"inv", "Inventory and health"}, {"spend", "Spend and tokens"}} {
		d.Tabs = append(d.Tabs, pillLink{Label: p[1], Href: t.With("tab", p[0]), On: tab == p[0]})
	}

	// The period and the one before it, for the cards' deltas; 14 days for the sparklines.
	prevSince := since.Add(-now.Sub(since))
	from := prevSince
	if f := now.AddDate(0, 0, -14); f.Before(from) {
		from = f
	}
	all, err := u.API.Store.CallsSince(ctx, from.UTC().Format(time.RFC3339))
	if err != nil {
		http.Error(w, "models failed", 500)
		return
	}
	info := u.aliases()
	ds := days(now, 14)
	idx := map[string]int{}
	for i, x := range ds {
		idx[x] = i
	}
	type acc struct {
		calls, errors int
		in, out       int64
		cost          float64
		unpriced      int
		lat           []float64
		last          store.ModelCall
		routines      map[string]int64
		series        []float64
	}
	byAlias, byFleet := map[string]*acc{}, map[string]*acc{}
	checks := map[string][]*checkRun{} // alias -> runs, in the order seen
	runOf := map[string]*checkRun{}
	get := func(m map[string]*acc, k string) *acc {
		a := m[k]
		if a == nil {
			a = &acc{routines: map[string]int64{}, series: make([]float64, len(ds))}
			m[k] = a
		}
		return a
	}
	var tok, prevTok, localTok int64
	var cloud, prevCloud float64
	tokSeries, localSeries, cloudSeries := make([]float64, 14), make([]float64, 14), make([]float64, 14)
	for _, c := range all {
		at, ok := parseTime(c.At)
		if !ok {
			continue
		}
		if c.Check != "" {
			k := c.Alias + "\x00" + c.RunID
			cr := runOf[k]
			if cr == nil {
				cr = &checkRun{At: c.At}
				runOf[k] = cr
				checks[c.Alias] = append(checks[c.Alias], cr)
			}
			if c.At > cr.At {
				cr.At = c.At
			}
			cr.Names = append(cr.Names, c.Check)
			if c.CheckOK == nil || !*c.CheckOK {
				cr.Failed = append(cr.Failed, c.Check)
			}
		}
		in, out := callTokens(c)
		local := info[c.Alias].Local
		cost := 0.0
		if c.CostUSD != nil {
			cost = *c.CostUSD
		}
		if i, ok := idx[at.UTC().Format("2006-01-02")]; ok {
			tokSeries[i] += float64(in + out)
			if local {
				localSeries[i] += float64(in + out)
			} else {
				cloudSeries[i] += cost
			}
			get(byAlias, c.Alias).series[i] += float64(in + out)
			get(byFleet, fleetOf(c.Routine)).series[i] += float64(in + out)
		}
		if at.Before(since) {
			if !at.Before(prevSince) {
				prevTok += in + out
				if !local {
					prevCloud += cost
				}
			}
			continue
		}
		tok += in + out
		if local {
			localTok += in + out
		} else {
			cloud += cost
		}
		for _, a := range []*acc{get(byAlias, c.Alias), get(byFleet, fleetOf(c.Routine))} {
			a.calls++
			if c.Error != "" {
				a.errors++
			}
			a.in += in
			a.out += out
			if c.CostUSD != nil {
				a.cost += *c.CostUSD
			} else {
				a.unpriced++
			}
			if c.LatencyS != nil {
				a.lat = append(a.lat, *c.LatencyS)
			}
			if c.Check == "" && c.At >= a.last.At {
				a.last = c
			}
			a.routines[c.Routine] += in + out
		}
		if c.CostUSD == nil {
			d.Unpriced++
		}
		d.HasCalls = true
	}
	share := func(local, total float64) float64 {
		if total == 0 {
			return 0
		}
		return local / total * 100
	}
	shareSeries := make([]float64, 14)
	for i := range tokSeries {
		shareSeries[i] = share(localSeries[i], tokSeries[i])
	}

	// Closed cases in the period, for the cost per closed case.
	rows, err := u.caseRows(ctx)
	if err != nil {
		http.Error(w, "cases failed", 500)
		return
	}
	closed, closedByFleet := 0, map[string]int{}
	for _, c := range rows {
		if c.Status != cases.Closed || c.ClosedAt == nil {
			continue
		}
		if t, ok := parseTime(*c.ClosedAt); ok && !t.Before(since) {
			closed++
			closedByFleet[c.Fleet]++
		}
	}
	unit := map[string]string{"24h": "last 24 hours", "7d": "last 7 days", "30d": "last 30 days", "month": "this month"}[period]
	delta := func(cur, prev float64, money bool) string {
		if prev == 0 {
			return "nothing in the period before"
		}
		return fmt.Sprintf("%+.0f%% vs the period before", (cur-prev)/prev*100)
	}
	for _, runs := range checks {
		sort.Slice(runs, func(i, j int) bool { return runs[i].At > runs[j].At })
	}
	down := 0
	for n, a := range byAlias {
		if runs := checks[n]; len(runs) > 0 && len(runs[0].Failed) > 0 || len(runs) == 0 && a.last.Error != "" {
			down++
		}
	}
	perCase := "–"
	if closed > 0 {
		perCase = usd2(cloud / float64(closed))
	}
	d.Cards = []scoreCard{
		{Label: "All tokens", Value: tokensText(tok), Unit: unit, Delta: delta(float64(tok), float64(prevTok), false), Tone: "neutral", Arrow: "■", Spark: makeSpark(tokSeries)},
		{Label: "Local share", Value: fmt.Sprintf("%.0f%%", share(float64(localTok), float64(tok))), Unit: "aliases declared cost: zero", Delta: "of the tokens in the period", Tone: "neutral", Arrow: "■", Spark: makeSpark(shareSeries)},
		{Label: "Cloud spend", Value: usd2(cloud), Unit: unit, Delta: delta(cloud, prevCloud, true), Tone: "neutral", Arrow: "■", Spark: makeSpark(cloudSeries)},
		{Label: "Cost per closed case", Value: perCase, Unit: plural(closed, "case closed", "cases closed"), Delta: "cloud spend over the cases closed in the period", Tone: "neutral", Arrow: "■", Spark: makeSpark(cloudSeries)},
		{Label: "Models down", Value: fmt.Sprint(down), Unit: fmt.Sprintf("of %d with calls", len(byAlias)), Delta: "an alias whose last contract check (or, without one, last call) failed", Tone: map[bool]string{true: "bad", false: "neutral"}[down > 0], Arrow: "■", Spark: makeSpark(nil)},
	}

	// Inventory: every configured alias, plus any alias a call named that the config no longer has.
	names := map[string]bool{}
	for n := range info {
		names[n] = true
	}
	for n := range byAlias {
		names[n] = true
	}
	sel := q.Get("alias")
	for _, n := range sortStrings(names) {
		a := byAlias[n]
		in := info[n]
		if in.Name == "" {
			in = aliasInfo{Name: n, Caps: "not in the config now", PricePerM: "–"}
		}
		row := invRow{aliasInfo: in, Engine: in.Endpoint, LastCall: "no call in the period", LastClass: "lamp lamp--idle", P50: "–", UsedBy: "–", Effective: "–"}
		if in.Local {
			row.Engine += " (local)"
		}
		if a != nil {
			row.Calls = a.calls
			if a.last.Error != "" {
				row.LastCall, row.LastClass = a.last.Error+" at "+u.clock(a.last.At), "lamp lamp--stop"
			} else {
				row.LastCall, row.LastClass = "ok at "+u.clock(a.last.At), "lamp"
			}
			if len(a.lat) > 0 {
				sort.Float64s(a.lat)
				row.P50 = fmt.Sprintf("%.1f s", a.lat[len(a.lat)/2])
			}
			row.UsedBy = strings.Join(sortStrings(keysOf(a.routines)), ", ")
			if a.in+a.out > 0 && a.cost > 0 {
				row.Effective = fmt.Sprintf("$%.2f", a.cost/float64(a.in+a.out)*1e6)
			}
		}
		if runs := checks[n]; len(runs) > 0 {
			last := runs[0]
			if len(last.Failed) > 0 {
				row.LastCall, row.LastClass, row.CheckFailed = "fail: "+strings.Join(last.Failed, ", ")+" at "+u.clock(last.At), "lamp lamp--stop", true
			} else {
				row.LastCall, row.LastClass = "pass at "+u.clock(last.At), "lamp"
			}
			for i, cr := range runs {
				if i == 5 {
					break
				}
				res := "pass"
				if len(cr.Failed) > 0 {
					res = "fail: " + strings.Join(cr.Failed, ", ")
				}
				row.Checks = append(row.Checks, u.clock(cr.At)+" · "+strings.Join(cr.Names, ", ")+" · "+res)
			}
		} else if a == nil {
			row.LastCall = "no check or call in the period"
		}
		d.Inventory = append(d.Inventory, row)
		if n == sel {
			rr := row
			d.Sel = &rr
			if a != nil {
				for _, rt := range sortStrings(keysOf(a.routines)) {
					d.SelRoutines = append(d.SelRoutines, rt+" · "+tokensText(a.routines[rt])+" tokens")
				}
			}
		}
	}
	if d.Sel == nil && len(d.Inventory) > 0 {
		rr := d.Inventory[0]
		d.Sel = &rr
		if a := byAlias[rr.Name]; a != nil {
			for _, rt := range sortStrings(keysOf(a.routines)) {
				d.SelRoutines = append(d.SelRoutines, rt+" · "+tokensText(a.routines[rt])+" tokens")
			}
		}
	}

	spend := func(name string, a *acc, local bool, per string) spendRow {
		r := spendRow{Name: name, Requests: a.calls, Tokens: tokensText(a.in + a.out), IO: tokensText(a.in) + " / " + tokensText(a.out),
			Spend: usd3(a.cost), Per: per, Spark: makeSpark(a.series), tokens: a.in + a.out, Local: "–"}
		r.Spark.Label = "Tokens a day over the last 14 days"
		if a.unpriced > 0 {
			r.Spend += fmt.Sprintf(" (+%d unpriced)", a.unpriced)
		}
		if local {
			r.Local = "yes"
		}
		return r
	}
	for name, a := range byAlias {
		if a.calls > 0 {
			d.ByModel = append(d.ByModel, spend(name+" · "+info[name].Model, a, info[name].Local, ""))
		}
	}
	for name, a := range byFleet {
		if a.calls == 0 {
			continue
		}
		per := "–"
		if n := closedByFleet[name]; n > 0 {
			per = usd2(a.cost / float64(n))
		}
		row := spend(title(name), a, false, per)
		if a.in+a.out > 0 {
			var l int64
			for alias, x := range byAlias {
				if info[alias].Local {
					for rt, n := range x.routines {
						if fleetOf(rt) == name {
							l += n
						}
					}
				}
			}
			row.Local = fmt.Sprintf("%.0f%%", float64(l)/float64(a.in+a.out)*100)
		}
		d.ByFleet = append(d.ByFleet, row)
	}
	sort.Slice(d.ByModel, func(i, j int) bool { return d.ByModel[i].tokens > d.ByModel[j].tokens })
	sort.Slice(d.ByFleet, func(i, j int) bool { return d.ByFleet[i].tokens > d.ByFleet[j].tokens })
	u.render(w, "models.html", 200, u.pageFor(s, "Models and spend", "Models and spend", r, d))
}

func keysOf[V any](m map[string]V) map[string]bool {
	out := make(map[string]bool, len(m))
	for k := range m {
		out[k] = true
	}
	return out
}

// modelsDay is Now's "Models, last 24h" panel: tokens and spend by alias.
type modelsDay struct {
	Tokens, Cost string
	Rows         []spendRow
	HasCalls     bool
	Unpriced     int
}

func (u *Server) modelsLastDay(r *http.Request) (modelsDay, error) {
	calls, err := u.API.Store.CallsSince(r.Context(), u.clockNow().Add(-24*time.Hour).UTC().Format(time.RFC3339))
	if err != nil {
		return modelsDay{}, err
	}
	info := u.aliases()
	by := map[string]*spendRow{}
	var tok int64
	cost := 0.0
	d := modelsDay{HasCalls: len(calls) > 0}
	for _, c := range calls {
		in, out := callTokens(c)
		row := by[c.Alias]
		if row == nil {
			row = &spendRow{Name: c.Alias}
			if info[c.Alias].Local {
				row.Local = "local"
			}
			by[c.Alias] = row
		}
		row.Requests++
		row.tokens += in + out
		tok += in + out
		if c.CostUSD != nil {
			cost += *c.CostUSD
		} else {
			d.Unpriced++
		}
	}
	costBy := map[string]float64{}
	for _, c := range calls {
		if c.CostUSD != nil {
			costBy[c.Alias] += *c.CostUSD
		}
	}
	for name, row := range by {
		row.Tokens, row.Spend = tokensText(row.tokens), usd2(costBy[name])
		if tok > 0 {
			row.Share = int(row.tokens * 100 / tok)
		}
		d.Rows = append(d.Rows, *row)
	}
	sort.Slice(d.Rows, func(i, j int) bool { return d.Rows[i].tokens > d.Rows[j].tokens })
	d.Tokens, d.Cost = tokensText(tok), usd2(cost)
	return d, nil
}
