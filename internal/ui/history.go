package ui

import (
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/chipi/flotilla/internal/api"
	"github.com/chipi/flotilla/internal/cases"
	"github.com/chipi/flotilla/internal/store"
)

// History (mockup v3): everything that already happened, newest first: closed cases, a person's
// decisions, executions, and the triage fleet's ledger (its own type, since it has thousands of rows).

type historyRow struct {
	At, When, Type, TypeClass, Case, CaseHref, Text, Who, Outcome, Fleet string
}

type historyData struct {
	T       tableQ
	Periods []pillLink
	Types   []pillLink
	Heads   []thCell
	Rows    []historyRow
	Fleets  []string
	Fleet   string
	Since   string
}

var historySorts = map[string]bool{"when": true, "type": true, "case": true, "who": true, "outcome": true}

func (u *Server) historyPage(w http.ResponseWriter, r *http.Request, s *store.Session, sid string) {
	ctx := r.Context()
	t := readTable(r, "when", true, func(c string) bool { return historySorts[c] })
	period, typ, fleet, q := t.Get("period"), t.Get("type"), t.Get("fleet"), strings.ToLower(t.Get("q"))
	now := u.clockNow()
	var since time.Time
	switch period {
	case "week":
		since = now.AddDate(0, 0, -7)
	case "month":
		since = now.AddDate(0, 0, -30)
	case "all":
	default:
		period = ""
		last, _ := u.API.Store.LastLook(ctx, s.User)
		var ok bool
		if since, ok = parseTime(last); !ok {
			since = now.Add(-24 * time.Hour)
		}
	}
	from := ""
	if !since.IsZero() {
		from = since.UTC().Format(time.RFC3339)
	}
	var rows []historyRow
	rowsCases, err := u.caseRows(ctx)
	if err != nil {
		http.Error(w, "history failed", 500)
		return
	}
	fleetOfCase := map[string]string{}
	titleOf := map[string]string{}
	for _, c := range rowsCases {
		fleetOfCase[c.ID], titleOf[c.ID] = c.Fleet, c.Title
	}
	if typ != "ledger" {
		evs, err := u.API.Store.CaseEventsSince(ctx, from, 100000)
		if err != nil {
			http.Error(w, "history failed", 500)
			return
		}
		for _, e := range evs {
			row := historyRow{At: e.At, When: u.clock(e.At), Case: e.CaseID, CaseHref: "/cases/" + e.CaseID, Fleet: fleetOfCase[e.CaseID],
				Who: strings.TrimPrefix(e.Actor, "user:")}
			switch {
			case e.By == cases.ByOperator:
				row.Type, row.TypeClass = "decision", "chip type--decision"
				row.Text = eventWhat(e) + ": " + titleOf[e.CaseID]
				_, row.Outcome = splitRun(e.Detail)
				row.Who = "you"
			case e.To == cases.Closed:
				row.Type, row.TypeClass = "case closed", "chip type--closed"
				row.Text = titleOf[e.CaseID]
				_, row.Outcome = splitRun(e.Detail)
			default:
				continue
			}
			if e.By == cases.ByOperator && e.Actor != "user:"+s.User {
				row.Who = strings.TrimPrefix(e.Actor, "user:")
			}
			rows = append(rows, row)
		}
		xs, err := u.API.Store.HostExecutionsSince(ctx, from)
		if err != nil {
			http.Error(w, "history failed", 500)
			return
		}
		for _, x := range xs {
			out := x.Status
			if x.Detail != "" {
				out += ": " + x.Detail
			}
			rows = append(rows, historyRow{At: x.SentAt, When: u.clock(x.SentAt), Type: "execution", TypeClass: "chip type--execution",
				Case: x.CaseID, CaseHref: "/cases/" + x.CaseID, Text: x.Action + " on " + x.Host, Who: "flotilla-host " + x.Host, Outcome: out,
				Fleet: fleetOfCase[x.CaseID]})
		}
	}
	if typ == "ledger" {
		if err := u.API.Check(ctx, caller(s), api.ActLedgerRead); err != nil {
			http.Error(w, "your role may not read the ledger", http.StatusForbidden)
			return
		}
		nss, err := u.API.Store.Namespaces(ctx)
		if err != nil {
			http.Error(w, "history failed", 500)
			return
		}
		for _, ns := range nss {
			ds, err := u.API.Store.RecentDecisions(ctx, ns, 0, 100000)
			if err != nil {
				http.Error(w, "history failed", 500)
				return
			}
			for _, x := range ds {
				at := x.Row["ts"]
				if from != "" && at < from {
					continue
				}
				out := x.Row["disposition"]
				if x.Recurrences > 0 {
					out += " · recurred " + plural(x.Recurrences, "time", "times")
				}
				if x.Row["reason"] != "" {
					out += ": " + x.Row["reason"]
				}
				rows = append(rows, historyRow{At: at, When: u.clock(at), Type: "ledger", TypeClass: "chip type--ledger", Case: x.Row["fingerprint"],
					Text: x.Row["alertname"], Who: ns, Outcome: out, Fleet: fleetOf(ns)})
			}
		}
	}
	fleets := map[string]bool{}
	var out []historyRow
	for _, x := range rows {
		if x.Fleet != "" {
			fleets[x.Fleet] = true
		}
		if typ != "" && typ != "ledger" && x.Type != typ {
			continue
		}
		if fleet != "" && x.Fleet != fleet {
			continue
		}
		if q != "" && !strings.Contains(strings.ToLower(x.Case+" "+x.Text+" "+x.Who+" "+x.Outcome), q) {
			continue
		}
		out = append(out, x)
	}
	key := func(x historyRow) string {
		switch t.Sort {
		case "type":
			return x.Type
		case "case":
			return x.Case
		case "who":
			return x.Who
		case "outcome":
			return x.Outcome
		}
		return x.At
	}
	sort.SliceStable(out, func(i, j int) bool {
		if t.Desc {
			return key(out[i]) > key(out[j])
		}
		return key(out[i]) < key(out[j])
	})
	d := historyData{T: t, Fleets: sortStrings(fleets), Fleet: fleet}
	d.Rows = window(out, &t)
	d.T = t
	for _, p := range [][2]string{{"", "Since you last looked"}, {"week", "7 days"}, {"month", "30 days"}, {"all", "All"}} {
		d.Periods = append(d.Periods, pillLink{Label: p[1], Href: t.With("period", p[0]), On: period == p[0]})
	}
	for _, p := range [][2]string{{"", "Everything"}, {"case closed", "Closed cases"}, {"decision", "Your decisions"}, {"execution", "Executions"}, {"ledger", "Triage ledger"}} {
		d.Types = append(d.Types, pillLink{Label: p[1], Href: t.With("type", p[0]), On: typ == p[0]})
	}
	d.Heads = []thCell{t.Th("when", "When"), t.Th("type", "Type"), t.Th("case", "Case"), t.Plain("What happened"), t.Th("who", "Who"), t.Th("outcome", "Outcome")}
	if !since.IsZero() {
		d.Since = u.clock(from)
	}
	u.render(w, "history.html", 200, u.pageFor(s, "History", "History", r, d))
}
