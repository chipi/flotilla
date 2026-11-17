package ui

import (
	"fmt"
	"net/http"
	"sort"
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
	Row                 runRow
	Ended               string
	Detail              string
	Trace               traceView
	Printed, PrintedErr string
	Read                []store.Loaded // what the run read into a model's context (G7)
	Untrusted           int
	CallsInSpans        bool          // every model call sits on its model.chat span: no separate table
	Decisions           []runDecision // the decisions the run wrote to the ledger, in time order
	Flow                *runFlowView  // the run role by role; nil when no call carries a role
}

type runDecision struct {
	Off, Title, Fingerprint, Disposition, Chip, Certainty, Probes, Gates, Model, Reason string
}

// toolNames is the tool names a call recorded (a JSON array) as "bash, read"; "" when none or unreadable.
func toolNames(raw string) string {
	tcs := parseTools(raw)
	names := make([]string, len(tcs))
	for i, t := range tcs {
		names[i] = t.Name
	}
	return strings.Join(names, ", ")
}

// callLine is a model call as one line on its span: alias, its role, its turn, the check it served, tokens, cost,
// how the reply ended (only when it was not a plain stop), result.
func callLine(c store.ModelCall) string {
	var parts []string
	parts = append(parts, c.Alias)
	if c.Role != "" {
		parts = append(parts, c.Role)
	}
	if c.Step != nil {
		parts = append(parts, fmt.Sprintf("turn %d", *c.Step))
	}
	if c.Check != "" {
		r := "pass"
		if c.CheckOK != nil && !*c.CheckOK {
			r = "fail"
		}
		parts = append(parts, "check "+c.Check+" "+r)
	}
	if c.TokensIn != nil && c.TokensOut != nil {
		parts = append(parts, fmt.Sprintf("%d → %d tokens", *c.TokensIn, *c.TokensOut))
	}
	if c.CostUSD != nil {
		parts = append(parts, fmt.Sprintf("$%.6f", *c.CostUSD))
	}
	switch c.FinishReason {
	case "", "stop":
	case "length":
		parts = append(parts, "cut at the token cap")
	default:
		parts = append(parts, "ended: "+c.FinishReason)
	}
	if t := toolNames(c.Tools); t != "" {
		parts = append(parts, "tools "+t)
	}
	if c.Error != "" {
		parts = append(parts, c.Error)
	}
	return strings.Join(parts, " · ")
}

// callsOnSpans puts each model call on its model.chat span, in time order, when the two lists are the
// same length; otherwise it changes nothing and the page keeps the calls table.
func callsOnSpans(t *traceView, calls []store.ModelCall) bool {
	var idx []int
	for i, sp := range t.Spans {
		if sp.Kind == "model" {
			idx = append(idx, i)
		}
	}
	if len(calls) == 0 || len(idx) != len(calls) {
		return false
	}
	sorted := append([]store.ModelCall(nil), calls...)
	sort.SliceStable(sorted, func(a, b int) bool { return sorted[a].At < sorted[b].At })
	for k, i := range idx {
		t.Spans[i].Call = callLine(sorted[k])
		t.Spans[i].CallBad = sorted[k].Error != "" || (sorted[k].CheckOK != nil && !*sorted[k].CheckOK)
	}
	return true
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
	d := runData{Row: rows[0], Ended: "still running", Detail: strings.TrimRight(run.Detail, "\n"),
		Trace: u.runTrace(r.Context(), run.TraceID, run.ID, r.URL.Query().Get("span"))}
	if lines, err := u.logsQuery(r.Context(), "_time:30d run_id:"+logsQuote(run.ID), 400); err != nil {
		d.PrintedErr = err.Error()
	} else {
		for i, j := 0, len(lines)-1; i < j; i, j = i+1, j-1 {
			lines[i], lines[j] = lines[j], lines[i]
		}
		d.Printed = logsText(lines, false)
	}
	ld, err := u.API.Store.RunLoaded(r.Context(), []string{run.ID})
	if err != nil {
		http.Error(w, "run failed", 500)
		return
	}
	d.Read = ld[run.ID]
	d.Untrusted = countUntrusted(d.Read)
	d.CallsInSpans = callsOnSpans(&d.Trace, d.Row.Calls)
	if fl := buildFlow(d.Row.Calls); fl != nil {
		d.Flow = u.runFlowView(fl, *run, d.Row.Calls, r.URL.Query())
	}
	decs, err := u.API.Store.DecisionsOfRun(r.Context(), run.ID)
	if err != nil {
		http.Error(w, "run failed", 500)
		return
	}
	t0, _ := parseTime(run.StartedAt)
	for _, x := range decs {
		off := ""
		if t, ok := parseTime(x.TS); ok && !t0.IsZero() {
			off = fmt.Sprintf("+%.0f s", t.Sub(t0).Seconds())
		}
		var fired []string
		for _, g := range mixGates(x.Gates) {
			if g[0] != "operational" && g[1] != "passed" && g[1] != "n/a" {
				fired = append(fired, g[0]+": "+g[1])
			}
		}
		gates := "none fired"
		if len(fired) > 0 {
			gates = strings.Join(fired, " · ")
		}
		chip := map[string]string{"file": "chip--info", "escalate": "chip--wait", "cleanup": "chip--ok"}[x.Disposition]
		if chip == "" {
			chip = "chip--neutral"
		}
		d.Decisions = append(d.Decisions, runDecision{Off: off, Title: x.Alertname, Fingerprint: x.Fingerprint, Disposition: x.Disposition, Chip: chip,
			Certainty: x.Certainty, Probes: x.NProbes, Gates: gates, Model: x.Model, Reason: x.Reason})
	}
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

func countUntrusted(ls []store.Loaded) int {
	n := 0
	for _, l := range ls {
		if l.Trust == store.TrustUntrusted {
			n++
		}
	}
	return n
}
