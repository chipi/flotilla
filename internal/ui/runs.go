package ui

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/chipi/flotilla/internal/store"
)

type runRow struct {
	store.Run
	Fleet, Start, Took, Cost, Chip string
	Cases                          []string
	Model, Tokens                  string // "triage-pro ×2", "1.8k"; "" when the run made no recorded call
	Calls                          []store.ModelCall
}

// modelText is a run's calls for its table cell: each alias with its count, failed calls named.
func modelText(calls []store.ModelCall) (string, string) {
	if len(calls) == 0 {
		return "", ""
	}
	n, failed := map[string]int{}, 0
	var order []string
	var tok int64
	for _, c := range calls {
		if n[c.Alias] == 0 {
			order = append(order, c.Alias)
		}
		n[c.Alias]++
		if c.Error != "" {
			failed++
		}
		in, out := callTokens(c)
		tok += in + out
	}
	parts := make([]string, len(order))
	for i, a := range order {
		parts[i] = a
		if n[a] > 1 {
			parts[i] += fmt.Sprintf(" ×%d", n[a])
		}
	}
	s := strings.Join(parts, ", ")
	if failed > 0 {
		s += fmt.Sprintf(" (%d failed)", failed)
	}
	return s, tokensText(tok)
}

type runsData struct {
	T        tableQ
	Tabs     []pillLink
	Heads    []thCell
	Rows     []runRow
	Routines []string
	Routine  string
}

// runChip is a run status's voice (mockup v3's Runs).
func runChip(status string) string {
	switch status {
	case store.StatusOK:
		return "chip chip--ok"
	case store.StatusError, store.StatusTimeout:
		return "chip chip--stop"
	case store.StatusInterrupted:
		return "chip chip--wait"
	case store.StatusRunning:
		return "chip chip--info"
	}
	return "chip chip--neutral"
}

func (u *Server) runRows(r *http.Request, runs []store.Run) ([]runRow, error) {
	ids := make([]string, len(runs))
	for i, x := range runs {
		ids[i] = x.ID
	}
	cs, err := u.API.Store.RunCases(r.Context(), ids)
	if err != nil {
		return nil, err
	}
	calls, err := u.API.Store.RunCalls(r.Context(), ids)
	if err != nil {
		return nil, err
	}
	out := make([]runRow, len(runs))
	for i, x := range runs {
		took := "–"
		if x.DurationS != nil {
			took = secsText(*x.DurationS)
		}
		out[i] = runRow{Run: x, Fleet: fleetOf(x.Routine), Start: u.clock(x.StartedAt), Took: took, Cost: usd3(x.SpendUSD),
			Chip: runChip(x.Status), Cases: cs[x.ID], Calls: calls[x.ID]}
		out[i].Model, out[i].Tokens = modelText(calls[x.ID])
	}
	return out, nil
}

func (u *Server) runsPage(w http.ResponseWriter, r *http.Request, s *store.Session, sid string) {
	t := readTable(r, "started", true, store.RunSorts)
	f := t.Get("f")
	q := store.RunQuery{Routine: t.Get("routine"), Status: t.Get("st"), Q: t.Get("q"), Sort: t.Sort, Desc: t.Desc,
		Offset: t.Offset(), Limit: t.Per, Problems: f == "problems", Spend: f == "spend"}
	runs, total, err := u.API.Store.RunsPage(r.Context(), q)
	if err != nil {
		http.Error(w, "runs failed", 500)
		return
	}
	t.Total = total
	d := runsData{T: t, Routine: q.Routine}
	if d.Rows, err = u.runRows(r, runs); err != nil {
		http.Error(w, "runs failed", 500)
		return
	}
	for _, p := range [][2]string{{"", "All"}, {"problems", "Problems"}, {"spend", "With model spend"}} {
		d.Tabs = append(d.Tabs, pillLink{Label: p[1], Href: t.With("f", p[0]), On: f == p[0]})
	}
	_, by := u.specs()
	d.Routines = sortedKeys(by)
	d.Heads = []thCell{t.Th("run", "Run"), t.Th("routine", "Routine"), t.Th("trigger", "Trigger"), t.Th("started", "Started"),
		t.Th("took", "Took"), t.Th("status", "Status"), t.Plain("Cases"), t.Plain("Model"), t.Th("cost", "Cost")}
	u.render(w, "runs.html", 200, u.pageFor(s, "Runs", "Runs", r, d))
}

type runData struct {
	Row    runRow
	Ended  string
	Detail string
}

func (u *Server) runPage(w http.ResponseWriter, r *http.Request, s *store.Session, sid string) {
	run, err := u.API.Store.Run(r.Context(), r.PathValue("id"))
	if err != nil {
		http.Error(w, "run failed", 500)
		return
	}
	if run == nil {
		http.NotFound(w, r)
		return
	}
	rows, err := u.runRows(r, []store.Run{*run})
	if err != nil {
		http.Error(w, "run failed", 500)
		return
	}
	d := runData{Row: rows[0], Ended: "still running", Detail: strings.TrimRight(run.Detail, "\n")}
	if run.EndedAt != nil {
		d.Ended = u.clock(*run.EndedAt)
	}
	if t, ok := parseTime(run.StartedAt); ok {
		d.Row.Start = t.In(u.loc()).Format("15:04:05")
		if u.clock(run.StartedAt) != t.In(u.loc()).Format("15:04") {
			d.Row.Start = u.clock(run.StartedAt) + ", " + t.In(u.loc()).Format("15:04:05")
		}
	}
	u.render(w, "run.html", 200, u.pageFor(s, "Run "+run.ID, "Runs", r, d))
}
