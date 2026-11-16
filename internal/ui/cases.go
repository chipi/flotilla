package ui

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/chipi/flotilla/internal/api"
	"github.com/chipi/flotilla/internal/cases"
	"github.com/chipi/flotilla/internal/store"
)

// touchedByYou are the cases a person moved at some point (approved, rejected, reassigned, closed).
func (u *Server) touchedByYou(ctx context.Context) (map[string]bool, error) {
	t, _, err := u.caseHistory(ctx)
	return t, err
}

// caseHistory is every case event, oldest first, and the cases a person moved.
func (u *Server) caseHistory(ctx context.Context) (map[string]bool, []store.CaseEvent, error) {
	evs, err := u.API.Store.CaseEventsSince(ctx, "", 1000000)
	if err != nil {
		return nil, nil, err
	}
	sort.Slice(evs, func(i, j int) bool { return evs[i].Seq < evs[j].Seq })
	out := map[string]bool{}
	for _, e := range evs {
		if e.By == cases.ByOperator {
			out[e.CaseID] = true
		}
	}
	return out, evs, nil
}

// waitingSeries counts, at the end of each day, the cases whose status then was waiting for a person.
func waitingSeries(evs []store.CaseEvent, ends []time.Time) []float64 {
	out := make([]float64, len(ends))
	byCase := map[string][]store.CaseEvent{}
	for _, e := range evs {
		if e.To != "" {
			byCase[e.CaseID] = append(byCase[e.CaseID], e)
		}
	}
	for _, es := range byCase {
		for i, end := range ends {
			status := ""
			for _, e := range es {
				if t, ok := parseTime(e.At); ok && t.Before(end) {
					status = e.To
				}
			}
			if waiting(status) {
				out[i]++
			}
		}
	}
	return out
}

// closedWithoutYou is the share of cases closed in the last 7 days that no person moved (fleet "" is
// every fleet), with its 14-day series of daily shares.
func closedWithoutYou(rows []caseRow, touched map[string]bool, fleet string, now time.Time) scoreCard {
	ds := days(now, 14)
	idx := map[string]int{}
	for i, d := range ds {
		idx[d] = i
	}
	closed, alone := make([]float64, 14), make([]float64, 14)
	week, weekAlone, prev, prevAlone := 0, 0, 0, 0
	for _, c := range rows {
		if c.ClosedAt == nil || (fleet != "" && c.Fleet != fleet) {
			continue
		}
		t, ok := parseTime(*c.ClosedAt)
		if !ok {
			continue
		}
		if i, ok := idx[t.UTC().Format("2006-01-02")]; ok {
			closed[i]++
			if !touched[c.ID] {
				alone[i]++
			}
		}
		switch age := now.Sub(t); {
		case age < 7*24*time.Hour:
			week++
			if !touched[c.ID] {
				weekAlone++
			}
		case age < 14*24*time.Hour:
			prev++
			if !touched[c.ID] {
				prevAlone++
			}
		}
	}
	share := make([]float64, 14)
	for i := range closed {
		if closed[i] > 0 {
			share[i] = alone[i] / closed[i] * 100
		}
	}
	c := scoreCard{Label: "Closed without you", Unit: "last 7 days", Value: "–", Tone: "neutral", Arrow: "■", Spark: makeSpark(share),
		Delta: "no case closed in the 7 days before"}
	if week > 0 {
		c.Value = fmt.Sprintf("%.0f%%", float64(weekAlone)/float64(week)*100)
	}
	if week > 0 && prev > 0 {
		d := float64(weekAlone)/float64(week)*100 - float64(prevAlone)/float64(prev)*100
		c.Delta = fmt.Sprintf("%+.0f pts vs the 7 days before", d)
		switch {
		case d > 0.5:
			c.Arrow, c.Tone = "▲", "good"
		case d < -0.5:
			c.Arrow, c.Tone = "▼", "bad"
		}
	}
	return c
}

// --- Cases ---

type pivot struct {
	Field, Value, Remove string
}

type casesData struct {
	Cards   []scoreCard
	Tabs    []pillLink
	T       tableQ
	Heads   []thCell
	Rows    []caseRow
	Pivots  []pivot
	Back    string
	Clear   string
	Shown   int
	Total   int
	Empty   string // why there are no cases at all
	Openers []string
}

var caseSorts = map[string]bool{"id": true, "status": true, "title": true, "kind": true, "target": true, "last": true}

func (u *Server) casesPage(w http.ResponseWriter, r *http.Request, s *store.Session, sid string) {
	ctx := r.Context()
	rows, err := u.caseRows(ctx)
	if err != nil {
		http.Error(w, "cases failed", 500)
		return
	}
	touched, evs, err := u.caseHistory(ctx)
	if err != nil {
		http.Error(w, "cases failed", 500)
		return
	}
	t := readTable(r, "last", true, func(c string) bool { return caseSorts[c] })
	d := casesData{T: t, Total: len(rows), Cards: u.caseCards(rows, touched, evs, u.clockNow())}

	groups := map[string]func(caseRow) bool{
		"":       func(caseRow) bool { return true },
		"needs":  func(c caseRow) bool { return u.waitingCase(c.Status, c.Kind) },
		"open":   func(c caseRow) bool { return c.Status != cases.Closed },
		"closed": func(c caseRow) bool { return c.Status == cases.Closed },
	}
	g := t.Get("status")
	if groups[g] == nil {
		g = ""
	}
	for _, p := range [][2]string{{"", "All"}, {"needs", "Waiting for you"}, {"open", "Open"}, {"closed", "Closed"}} {
		n := 0
		for _, c := range rows {
			if groups[p[0]](c) {
				n++
			}
		}
		d.Tabs = append(d.Tabs, pillLink{Label: p[1], Href: t.With("status", p[0]), N: n, HasN: true, On: g == p[0]})
	}

	// Pivots are repeated p=field:value, in the order they were added, so "Back" removes the last.
	ps := r.URL.Query()["p"]
	var pv []pivot
	for i, p := range ps {
		f, v, ok := strings.Cut(p, ":")
		if !ok || (f != "status" && f != "kind" && f != "target") {
			continue
		}
		pv = append(pv, pivot{Field: f, Value: v, Remove: withPivots(t, without(ps, i))})
	}
	d.Pivots = pv
	if len(ps) > 0 {
		d.Back, d.Clear = withPivots(t, ps[:len(ps)-1]), withPivots(t, nil)
	}
	q := strings.ToLower(t.Get("q"))
	var out []caseRow
	for _, c := range rows {
		if !groups[g](c) {
			continue
		}
		keep := true
		for _, p := range pv {
			switch p.Field {
			case "status":
				keep = keep && c.Status == p.Value
			case "kind":
				keep = keep && c.Kind == p.Value
			case "target":
				keep = keep && c.Target == p.Value
			}
		}
		if keep && q != "" {
			keep = strings.Contains(strings.ToLower(c.ID+" "+c.Title+" "+c.Kind+" "+c.Target), q)
		}
		if keep {
			out = append(out, c)
		}
	}
	key := func(c caseRow) string {
		switch t.Sort {
		case "id":
			return c.ID
		case "status":
			return c.Status
		case "title":
			return strings.ToLower(c.Title)
		case "kind":
			return c.Kind
		case "target":
			return c.Target
		}
		return c.Last.At
	}
	sort.SliceStable(out, func(i, j int) bool {
		if t.Desc {
			return key(out[i]) > key(out[j])
		}
		return key(out[i]) < key(out[j])
	})
	d.Shown = len(out)
	d.Rows = window(out, &t)
	d.T = t
	for _, h := range [][2]string{{"id", "Case"}, {"status", "Status"}, {"title", "Problem"}, {"kind", "Kind"}, {"target", "Target"}, {"last", "Last event"}} {
		d.Heads = append(d.Heads, t.Th(h[0], h[1]))
	}
	if len(rows) == 0 {
		d.Empty, d.Openers = u.whyNoCases(ctx)
	}
	u.render(w, "cases.html", 200, u.pageFor(s, "Cases", "Cases", r, d))
}

// pivotHref adds a pivot to the current filters (a tag's link).
func pivotHref(t tableQ, field, value string) string {
	ps := append(append([]string(nil), t.vals["p"]...), field+":"+value)
	return withPivots(t, ps)
}

func without(xs []string, i int) []string {
	out := append([]string(nil), xs[:i]...)
	return append(out, xs[i+1:]...)
}

func withPivots(t tableQ, ps []string) string {
	v := url.Values{}
	for k, vs := range t.vals {
		if k != "p" && k != "page" {
			v[k] = append([]string(nil), vs...)
		}
	}
	for _, p := range ps {
		v.Add("p", p)
	}
	if len(v) == 0 {
		return t.Path
	}
	return t.Path + "?" + v.Encode()
}

// whyNoCases says which routines open cases and whether they run, for Cases' empty state.
func (u *Server) whyNoCases(ctx context.Context) (string, []string) {
	routines, _, err := u.API.Status(ctx)
	if err != nil {
		return "", nil
	}
	_, by := u.specs()
	var openers []string
	running := 0
	for _, x := range routines {
		j := by[x.ID]
		if j == nil || len(j.Detects) == 0 {
			continue
		}
		state := "runs every " + x.Every
		if !x.Enabled {
			state = "disabled in this installation"
		} else if x.Paused {
			state = "paused"
		} else {
			running++
		}
		openers = append(openers, x.ID+" (detects "+strings.Join(j.Detects, ", ")+"): "+state)
	}
	switch {
	case len(openers) == 0:
		return "No case has been opened: no routine in this installation detects problems as cases yet.", nil
	case running == 0:
		return "No case has been opened: every routine that opens cases is disabled or paused, so nothing can open one. The triage fleet files GitHub issues through its ledger, not cases (see History).", openers
	}
	return "No case has been opened yet: the routines that open cases have found nothing.", openers
}

// caseCards are Cases' six score cards.
func (u *Server) caseCards(rows []caseRow, touched map[string]bool, evs []store.CaseEvent, now time.Time) []scoreCard {
	ds := days(now, 14)
	ends := make([]time.Time, 14)
	idx := map[string]int{}
	for i, d := range ds {
		t, _ := time.Parse("2006-01-02", d)
		ends[i] = t.Add(24 * time.Hour)
		idx[d] = i
	}
	open, opened, closed := make([]float64, 14), make([]float64, 14), make([]float64, 14)
	wait := waitingSeries(evs, ends)
	ttc := make([]float64, 14)
	ttcN := make([]float64, 14)
	nOpen, nWait, opened24, closed24 := 0, 0, 0, 0
	var week []time.Duration
	day := now.Add(-24 * time.Hour)
	for _, c := range rows {
		o, ok := parseTime(c.OpenedAt)
		if !ok {
			continue
		}
		var cl time.Time
		if c.ClosedAt != nil {
			cl, _ = parseTime(*c.ClosedAt)
		}
		for i, e := range ends {
			if o.Before(e) && (cl.IsZero() || !cl.Before(e)) {
				open[i]++
			}
		}
		if i, ok := idx[o.UTC().Format("2006-01-02")]; ok {
			opened[i]++
		}
		if o.After(day) {
			opened24++
		}
		if c.Status != cases.Closed {
			nOpen++
		}
		if u.waitingCase(c.Status, c.Kind) {
			nWait++
		}
		if !cl.IsZero() {
			if i, ok := idx[cl.UTC().Format("2006-01-02")]; ok {
				closed[i]++
				ttc[i] += cl.Sub(o).Minutes()
				ttcN[i]++
			}
			if cl.After(day) {
				closed24++
			}
			if now.Sub(cl) < 7*24*time.Hour {
				week = append(week, cl.Sub(o))
			}
		}
	}
	for i := range ttc {
		if ttcN[i] > 0 {
			ttc[i] /= ttcN[i]
		}
	}
	median := "–"
	if len(week) > 0 {
		sort.Slice(week, func(i, j int) bool { return week[i] < week[j] })
		median = span(week[len(week)/2])
	}
	ttcCard := countCard("Time to close", "median, 7 days", ttc, median, lessIsBetter)
	if ttcN[13] == 0 {
		ttcCard.Delta, ttcCard.Arrow, ttcCard.Tone = "no case closed today", "■", "neutral"
	}
	return []scoreCard{
		countCard("Open cases", "now", open, fmt.Sprint(nOpen), lessIsBetter),
		countCard("Waiting for you", "now", wait, fmt.Sprint(nWait), lessIsBetter),
		countCard("Opened", "last 24h", opened, fmt.Sprint(opened24), nil),
		countCard("Closed", "last 24h", closed, fmt.Sprint(closed24), nil),
		ttcCard,
		closedWithoutYou(rows, touched, "", now),
	}
}

// --- Case ---

type timelineEvent struct {
	What, When, Actor, Run, Detail, Dot string
	Fold                                []timelineEvent // a run of events that changed nothing, opened on a tap
	FoldText                            string
}

// foldTimeline keeps every event that changed the case's status, the first and the last, and folds each run
// of two or more events between them that changed nothing (a signal attached again, a run with no decision)
// into one row: how many of each kind, and how many came after the case closed (UI plan step 3).
func foldTimeline(ev []store.CaseEvent, rows []timelineEvent, closedAt string) []timelineEvent {
	var out, group []timelineEvent
	var groupEv []store.CaseEvent
	flush := func() {
		switch {
		case len(group) == 1:
			out = append(out, group[0])
		case len(group) > 1:
			counts, order := map[string]int{}, []string{}
			after := 0
			for _, e := range groupEv {
				w := eventWhat(e)
				if counts[w] == 0 {
					order = append(order, w)
				}
				counts[w]++
				if closedAt != "" && e.At > closedAt {
					after++
				}
			}
			var parts []string
			for _, w := range order {
				parts = append(parts, fmt.Sprintf("%d × %s", counts[w], w))
			}
			text := strings.Join(parts, ", ")
			if after > 0 {
				text += fmt.Sprintf(" (%d after the case closed)", after)
			}
			out = append(out, timelineEvent{What: fmt.Sprintf("%d repeats folded", len(group)), When: group[0].When + " to " + group[len(group)-1].When,
				Dot: "dot", Fold: append([]timelineEvent(nil), group...), FoldText: text})
		}
		group, groupEv = nil, nil
	}
	for i, e := range ev {
		if i == 0 || i == len(ev)-1 || e.To != "" {
			flush()
			out = append(out, rows[i])
			continue
		}
		group, groupEv = append(group, rows[i]), append(groupEv, e)
	}
	flush()
	return out
}

type policyLevel struct{ What, Level string }

type caseData struct {
	EventCount int // the events before folding
	Row        caseRow
	Routine    string
	Opened     string
	Decision   *decision
	Events     []timelineEvent
	Plan       *store.PlanRecord
	Levels     []policyLevel
	Result     string
	Record     string
	CanFix     bool // escalated: an operator may close or reassign it
	CanApply   bool // an approved repo change: an operator records the commit that applied it (ADR-0016)
	PlanCount  int
	Decided    string
	Verdicts   []store.Feedback
	RunsRead   []caseRunRead // the case's runs: what each called (G1) and read (G7), for "What the agent read"
	Mine       string        // the signed-in person's verdict
	Notes      []store.Note
}

func (u *Server) casePage(w http.ResponseWriter, r *http.Request, s *store.Session, sid string) {
	ctx := r.Context()
	c, err := u.API.Store.Case(ctx, r.PathValue("id"))
	if errors.Is(err, store.ErrNoSuchCase) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "case failed", 500)
		return
	}
	ev, err := u.API.Store.CaseEvents(ctx, c.ID)
	if err != nil {
		http.Error(w, "case failed", 500)
		return
	}
	plans, err := u.API.Store.Plans(ctx, c.ID)
	if err != nil {
		http.Error(w, "case failed", 500)
		return
	}
	kinds, _ := u.specs()
	var first, last store.CaseEvent
	if len(ev) > 0 {
		first, last = ev[0], ev[len(ev)-1]
	}
	f := caseFleet(c, first, kinds)
	row := caseRow{Case: c, Fleet: f, FleetName: title(f), Icon: fleetIcon(f), Title: problem(c, first), StatusText: statusLabel(c.Status),
		Chip: caseChip(c.Status), Age: u.ago(c.StatusAt), Last: last, LastWhen: u.clock(last.At), LastWhat: eventShort(last), Opener: first.Actor}
	d := caseData{Row: row, Routine: kinds[c.Kind], Opened: u.clock(c.OpenedAt), PlanCount: len(plans),
		CanFix: c.Status == cases.Escalated && (s.Role == api.RoleOperator || s.Role == api.RoleAdmin)}
	d.CanApply = c.Status == cases.Executing && (s.Role == api.RoleOperator || s.Role == api.RoleAdmin) && u.repoCase(ctx, c.ID) &&
		u.repoExecutor(ctx, c.ID) == "" // a routine carries it out (RFC-0010 §3): nothing for a person to record
	if d.Routine == "" && strings.Contains(first.Actor, "/") {
		d.Routine = first.Actor
	}
	if c.Status == cases.AwaitingApproval {
		dec, ok, err := u.decisionFor(ctx, row, true)
		if err != nil {
			http.Error(w, "case failed", 500)
			return
		}
		if ok {
			d.Decision = &dec
		}
	}
	for i, e := range ev {
		run, rest := splitRun(e.Detail)
		what := eventWhat(e)
		if i == 0 {
			what = "Detected"
		}
		actor := e.Actor
		if e.By != "" && e.By != e.Actor {
			actor += " (" + e.By + ")"
		}
		d.Events = append(d.Events, timelineEvent{What: what, When: u.clock(e.At), Actor: actor, Run: run, Detail: rest, Dot: eventDot(e)})
	}
	closedAt := ""
	if c.ClosedAt != nil {
		closedAt = *c.ClosedAt
	}
	d.EventCount = len(d.Events)
	d.Events = foldTimeline(ev, d.Events, closedAt)
	if len(plans) > 0 {
		p := plans[len(plans)-1]
		d.Plan = &p
		routine, action, host, ok := planLevels(p)
		if ok {
			d.Levels = []policyLevel{{"The routine (" + orDash(d.Routine) + ")", routine}, {"This action's earned level", action},
				{"The host cap on " + hostOf(c.Target), host}}
		}
		fr, _ := u.API.FreezeState(ctx)
		frozen := "off"
		if fr.Frozen {
			frozen = "on"
		}
		d.Levels = append(d.Levels, policyLevel{"Autonomy freeze", frozen})
		d.Result, d.Decided = p.Outcome, u.clock(p.DecidedAt)
		if p.Would != "" {
			d.Result += " (in shadow; it would have been " + p.Would + ")"
		}
		if rec, err := u.API.Store.ActionRecord(ctx, p.Action); err == nil {
			d.Record = fmt.Sprintf("%s: %s, %d verified, %d failed", p.Action, plural(rec.Plans, "plan", "plans"), rec.Verified, rec.Failed)
		}
	}
	// The runs on this case: those named in its timeline and its plans.
	var runIDs []string
	seenRun := map[string]bool{}
	for _, e := range ev {
		if run, _ := splitRun(e.Detail); run != "" && !seenRun[run] {
			seenRun[run] = true
			runIDs = append(runIDs, run)
		}
	}
	for _, p := range plans {
		if !seenRun[p.RunID] {
			seenRun[p.RunID] = true
			runIDs = append(runIDs, p.RunID)
		}
	}
	calls, err := u.API.Store.RunCalls(ctx, runIDs)
	if err != nil {
		http.Error(w, "case failed", 500)
		return
	}
	ld, err := u.API.Store.RunLoaded(ctx, runIDs)
	if err != nil {
		http.Error(w, "case failed", 500)
		return
	}
	for _, id := range runIDs {
		m, tok := modelText(calls[id])
		d.RunsRead = append(d.RunsRead, caseRunRead{Run: id, Models: m, Tokens: tok, Read: ld[id], Untrusted: countUntrusted(ld[id])})
	}
	if d.Verdicts, err = u.API.Store.CaseFeedback(ctx, c.ID); err != nil {
		http.Error(w, "case failed", 500)
		return
	}
	for _, v := range d.Verdicts {
		if v.User == s.User {
			d.Mine = v.Verdict
		}
	}
	notes, err := u.API.Store.Notes(ctx, "")
	if err != nil {
		http.Error(w, "case failed", 500)
		return
	}
	for _, n := range notes {
		if n.FromCase == c.ID {
			d.Notes = append(d.Notes, n)
		}
	}
	u.render(w, "case.html", 200, u.pageFor(s, "Case "+c.ID, "Cases", r, d))
}

// caseAction is the Decision box's answers and an escalated case's way out, through the API's calls.
func (u *Server) caseAction(w http.ResponseWriter, r *http.Request, s *store.Session, sid string) {
	id := r.PathValue("id")
	back := safeBack(r.PostFormValue("back"), "/cases/"+id)
	why := r.PostFormValue("reason")
	var err error
	var ok string
	switch r.PathValue("action") {
	case "approve":
		var hash string
		hash, err = u.API.ApprovePlan(r.Context(), caller(s), "ui", id, r.PostFormValue("plan"), r.PostFormValue("note"))
		ok = "Approved plan " + shortHash(hash) + " on " + id + ": flotillad signs it and sends it to the host."
		if ex := u.repoExecutor(r.Context(), id); ex != "" {
			ok = "Approved plan " + shortHash(hash) + " on " + id + ": " + ex + " opens it as a draft PR; you merge it."
		} else if u.repoCase(r.Context(), id) {
			ok = "Approved plan " + shortHash(hash) + " on " + id + ": apply the change, then record its commit on the case."
		}
	case "reject":
		err, ok = u.API.RejectCase(r.Context(), caller(s), "ui", id, why), "Rejected the plan on "+id+": nothing ran, and the case is escalated."
	case "close":
		err, ok = u.API.CloseCase(r.Context(), caller(s), "ui", id, why), "Closed "+id+"."
	case "reassign":
		err, ok = u.API.ReassignCase(r.Context(), caller(s), "ui", id, why), "Handed "+id+" back to its routine."
	case "right", "wrong":
		err, ok = u.API.SetVerdict(r.Context(), caller(s), "ui", id, r.PathValue("action")), "Recorded: the diagnosis was "+r.PathValue("action")+"."
	case "applied":
		err, ok = u.API.RecordApplied(r.Context(), caller(s), "ui", id, r.PostFormValue("commit")), "Recorded: "+id+" applied, closed as verified."
	case "note":
		var n int64
		n, err = u.API.ProposeNote(r.Context(), caller(s), "ui", id, r.PostFormValue("note"))
		ok = fmt.Sprintf("Note N-%d proposed: it takes effect once it is activated in Knowledge.", n)
	default:
		http.NotFound(w, r)
		return
	}
	done(w, r, back, err, ok)
}

type caseRunRead struct {
	Run, Models, Tokens string
	Read                []store.Loaded
	Untrusted           int
}

// repoExecutor is the routine that carries out the case's newest plan, a repo action, "" when there is
// none (a person applies it, or it is not a repo action).
func (u *Server) repoExecutor(ctx context.Context, id string) string {
	plans, err := u.API.Store.Plans(ctx, id)
	if err != nil || len(plans) == 0 || !u.repoCase(ctx, id) {
		return ""
	}
	return u.executorOf(plans[len(plans)-1].Action)
}

// repoCase is whether the case's newest plan is a repo change (ADR-0016).
func (u *Server) repoCase(ctx context.Context, id string) bool {
	plans, err := u.API.Store.Plans(ctx, id)
	if err != nil || len(plans) == 0 {
		return false
	}
	a, ok := registryAction(u, plans[len(plans)-1].Action)
	return ok && a.Repo()
}
