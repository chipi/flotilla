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
	"github.com/chipi/flotilla/internal/cases"
	"github.com/chipi/flotilla/internal/hostexec"
	"github.com/chipi/flotilla/internal/store"
)

// The pages, built to mockup v3 (docs/wip/ui-mockup-parity.md). Every action goes through the
// api.Server functions the API uses, with the signed-in person as the caller, so the table and every
// rule apply the same way.

func caller(s *store.Session) api.Caller {
	return api.Caller{Name: "user:" + s.User, Role: s.Role, User: true}
}

// waitingCount is what the badge, Now's headline and Approvals count: cases a person must answer.
func (u *Server) waitingCount(ctx context.Context) int {
	n := 0
	for _, st := range []string{cases.AwaitingApproval, cases.Escalated} {
		cs, err := u.API.Store.Cases(ctx, store.CaseFilter{Status: st, Limit: 10000})
		if err == nil {
			n += len(cs)
		}
	}
	return n
}

func (u *Server) pageFor(s *store.Session, title, active string, r *http.Request, data any) page {
	p := page{Title: title, User: s.User, Role: s.Role, CSRF: s.CSRF, Active: active,
		Nav:    nav(active, u.waitingCount(r.Context())),
		CanAct: s.Role == api.RoleOperator || s.Role == api.RoleAdmin, IsAdmin: s.Role == api.RoleAdmin,
		Msg: r.URL.Query().Get("msg"), Error: r.URL.Query().Get("err"), Data: data, Version: u.API.Version,
		Initial: strings.ToUpper(firstRune(s.User))}
	fr, err := u.API.FreezeState(r.Context())
	if err != nil {
		u.Log.Error("reading the freeze", "err", err)
		p.Error = "flotillad could not read whether the freeze holds; check `flotilla status`"
	}
	p.Freeze = fr
	return p
}

func firstRune(s string) string {
	for _, r := range s {
		return string(r)
	}
	return "?"
}

// done redirects back with the action's outcome, as a short message on the page.
func done(w http.ResponseWriter, r *http.Request, back string, err error, ok string) {
	q := url.Values{}
	if err != nil {
		if p := api.AsProblem(err); p != nil {
			q.Set("err", p.Msg)
		} else {
			q.Set("err", "that did not work: "+err.Error())
		}
	} else {
		q.Set("msg", ok)
	}
	sep := "?"
	if strings.Contains(back, "?") {
		sep = "&"
	}
	http.Redirect(w, r, back+sep+q.Encode(), http.StatusSeeOther)
}

// safeBack keeps redirects on this site's own pages.
func safeBack(v, fallback string) string {
	if strings.HasPrefix(v, "/") && !strings.HasPrefix(v, "//") && !strings.ContainsAny(v, "\\\r\n") {
		return v
	}
	return fallback
}

// --- Now ---

type wrongItem struct{ What, Detail, Go, Href string }

type handledRow struct {
	Time, Text, Href, Outcome string
	OK                        bool
}

type hostNode struct {
	Name, Roles, Note string
	Up                bool
}

type nowData struct {
	DateLine, Headline string
	Wrong              []wrongItem
	Normal             string
	First              *decision
	Needs              []caseRow
	PlansLink          string
	Handled            []handledRow
	HandledTotal       int
	HandledWhen        string
	Hosts              []hostNode
	Spend24            string
	Fleets             []fleetSum
	Models             modelsDay
}

func (u *Server) now(w http.ResponseWriter, r *http.Request, s *store.Session, sid string) {
	ctx := r.Context()
	now := u.clockNow()
	since, err := u.API.Store.SeenNow(ctx, s.User)
	if err != nil {
		u.Log.Error("seen now", "err", err)
	}
	sinceT, known := parseTime(since)
	if !known {
		sinceT = now.Add(-24 * time.Hour)
	}
	rows, err := u.caseRows(ctx)
	if err != nil {
		http.Error(w, "cases failed", 500)
		return
	}
	fs, routines, workers, err := u.fleetSums(ctx, rows)
	if err != nil {
		http.Error(w, "status failed", 500)
		return
	}
	d := nowData{}
	n := now.In(u.loc())
	d.DateLine = n.Format("Monday 2 January, 15:04")
	if known {
		d.DateLine += " · since you looked at " + u.clock(since)
	} else {
		d.DateLine += " · the last 24 hours"
	}

	// Needs you: the first waiting plan as a decision, then the rest as cards.
	plans := 0
	for _, c := range rows {
		if !waiting(c.Status) {
			continue
		}
		if c.Status == cases.AwaitingApproval {
			plans++
			if d.First == nil {
				dec, ok, err := u.decisionFor(ctx, c, false)
				if err != nil {
					http.Error(w, "case failed", 500)
					return
				}
				if ok {
					d.First = &dec
					continue
				}
			}
		}
		d.Needs = append(d.Needs, c)
	}
	switch {
	case plans > 1 && d.First != nil:
		d.PlansLink = fmt.Sprintf("All %d plans in Approvals", plans)
	case plans > 0:
		d.PlansLink = "Approvals"
	}

	// Handled since the last look.
	var handled []caseRow
	for _, c := range rows {
		if c.ClosedAt != nil {
			if t, ok := parseTime(*c.ClosedAt); ok && !t.Before(sinceT) {
				handled = append(handled, c)
			}
		}
	}
	sort.Slice(handled, func(i, j int) bool { return *handled[i].ClosedAt > *handled[j].ClosedAt })
	d.HandledTotal = len(handled)
	for i, c := range handled {
		if i == 5 {
			break
		}
		res := strings.ReplaceAll(c.Resolution, "_", " ")
		d.Handled = append(d.Handled, handledRow{Time: u.clock(*c.ClosedAt), Text: c.Title + " (" + c.ID + ")", Href: "/cases/" + c.ID,
			Outcome: res, OK: c.Resolution == cases.Verified})
	}
	waitingN := 0
	for _, c := range rows {
		if waiting(c.Status) {
			waitingN++
		}
	}
	when := "since you looked"
	if !known {
		when = "in the last 24 hours"
	}
	d.HandledWhen = when
	if d.HandledTotal == 0 {
		d.Headline = "No case handled " + when + ". "
	} else {
		d.Headline = plural(d.HandledTotal, "case", "cases") + " handled " + when + ". "
	}
	switch waitingN {
	case 0:
		d.Headline += "Nothing needs you."
	case 1:
		d.Headline += "1 needs you."
	default:
		d.Headline += fmt.Sprintf("%d need you.", waitingN)
	}

	// Wrong right now, not a decision.
	fr, _ := u.API.FreezeState(ctx)
	if fr.Frozen {
		d.Wrong = append(d.Wrong, wrongItem{What: "Autonomy is frozen", Detail: "since " + u.clock(fr.At) + " by " + fr.By + ": every routine runs at shadow",
			Go: "Autonomy", Href: "/autonomy"})
	}
	stopped := map[string]bool{}
	for _, x := range routines {
		if x.Stopped && !stopped[x.Fleet] {
			stopped[x.Fleet] = true
			d.Wrong = append(d.Wrong, wrongItem{What: "Fleet " + title(x.Fleet) + " is STOPped", Detail: "none of its routines start",
				Go: "Fleets", Href: "/fleets/" + x.Fleet})
		}
	}
	for _, x := range routines {
		if !x.Enabled || x.Stopped {
			continue
		}
		href := "/fleets/" + x.Fleet + "?sel=" + url.QueryEscape(x.ID)
		if x.Stale {
			last := "it has never run ok"
			if x.LastSuccess != "" {
				last = "last ok " + u.clock(x.LastSuccess) + ", " + u.agoText(x.LastSuccess)
			}
			d.Wrong = append(d.Wrong, wrongItem{What: x.ID + " is stale", Detail: last + "; it runs every " + x.Every, Go: "Fleet", Href: href})
		}
		if x.Paused {
			d.Wrong = append(d.Wrong, wrongItem{What: x.ID + " is paused", Detail: "it starts no runs until it is resumed", Go: "Fleet", Href: href})
		}
	}
	for _, name := range sortedKeys(workers) {
		if m, ok := workers[name].(map[string]any); ok && m["ok"] != true {
			detail := "its health check fails"
			if e, ok := m["error"].(string); ok && e != "" {
				detail += ": " + e
			}
			d.Wrong = append(d.Wrong, wrongItem{What: "worker " + name + " is not healthy", Detail: detail, Go: "Fleets", Href: "/fleets"})
		}
	}
	hosts, err := u.API.Store.Hosts(ctx)
	if err != nil {
		http.Error(w, "hosts failed", 500)
		return
	}
	dayStart := time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, u.loc())
	execs, err := u.API.Store.HostExecutionsSince(ctx, dayStart.UTC().Format(time.RFC3339))
	if err != nil {
		http.Error(w, "executions failed", 500)
		return
	}
	perHost := map[string]int{}
	hourAgo := now.Add(-time.Hour)
	execHour, badHour := 0, 0
	for _, x := range execs {
		perHost[x.Host]++
		if t, ok := parseTime(x.SentAt); ok && t.After(hourAgo) {
			execHour++
			if x.Status == hostexec.Failed || x.Status == hostexec.Unknown {
				badHour++
			}
		}
	}
	for _, h := range hosts {
		if h.RevokedAt != nil || h.EnrolledAt == nil {
			continue
		}
		node := hostNode{Name: h.Name, Roles: "host agent " + orDash(h.Version), Up: h.Connected,
			Note: plural(perHost[h.Name], "execution today", "executions today")}
		if !h.Connected {
			node.Note = "down " + u.ago(h.LastContact)
			d.Wrong = append(d.Wrong, wrongItem{What: h.Name + " host agent not connected", Go: "Hosts", Href: "/hosts/" + h.Name,
				Detail: "for " + u.ago(h.LastContact) + "; nothing can run on " + h.Name + " until it reconnects"})
		}
		d.Hosts = append(d.Hosts, node)
	}
	failed, _, err := u.API.Store.RunsPage(ctx, store.RunQuery{Problems: true, Since: now.Add(-24 * time.Hour).UTC().Format(time.RFC3339), Limit: 10000, Desc: true})
	if err != nil {
		http.Error(w, "runs failed", 500)
		return
	}
	type fail struct {
		n            int
		last, status string
	}
	byRoutine, order := map[string]*fail{}, []string{}
	for _, x := range failed {
		if x.Status == store.StatusSkipped {
			continue
		}
		f := byRoutine[x.Routine]
		if f == nil {
			f = &fail{last: x.StartedAt, status: x.Status}
			byRoutine[x.Routine] = f
			order = append(order, x.Routine)
		}
		f.n++
	}
	for _, id := range order {
		f := byRoutine[id]
		d.Wrong = append(d.Wrong, wrongItem{What: id + " failed " + plural(f.n, "time", "times"), Go: "Runs",
			Href:   "/runs?f=problems&routine=" + url.QueryEscape(id),
			Detail: "in the last 24 hours; the last at " + u.clock(f.last) + " (" + f.status + ")"})
	}
	lim := u.API.Limits.WithDefaults()
	spend := 0.0
	for _, f := range fs {
		spend += f.Spend24
	}
	d.Spend24 = usd2(spend)
	breaker := "closed"
	if badHour >= lim.BreakerFailures {
		breaker = "open"
	}
	lead := "Everything else is normal"
	if len(d.Wrong) == 0 {
		lead = "Nothing is wrong right now"
	}
	d.Normal = fmt.Sprintf("%s: the circuit breaker is %s (%d of %d failures in the last hour), %d of %d executions are used this hour, and runs cost %s in 24 hours.",
		lead, breaker, badHour, lim.BreakerFailures, execHour, lim.PlatformPerHour, d.Spend24)
	d.Fleets = fs
	if d.Models, err = u.modelsLastDay(r); err != nil {
		http.Error(w, "models failed", 500)
		return
	}
	u.render(w, "now.html", 200, u.pageFor(s, "Now", "Now", r, d))
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func orDash(s string) string {
	if s == "" {
		return "–"
	}
	return s
}

// --- Approvals ---

type decidedRow struct {
	When, ID, Decision, From, Detail string
}

type approvalsData struct {
	Summary  string
	Queue    []decision
	Items    []decision
	Waiting  int
	Recent   []decidedRow
	SinceAt  string
	Escalate int
}

func (u *Server) approvalsPage(w http.ResponseWriter, r *http.Request, s *store.Session, sid string) {
	ctx := r.Context()
	rows, err := u.caseRows(ctx)
	if err != nil {
		http.Error(w, "cases failed", 500)
		return
	}
	d := approvalsData{}
	for _, c := range rows {
		switch c.Status {
		case cases.AwaitingApproval:
			dec, ok, err := u.decisionFor(ctx, c, false)
			if err != nil {
				http.Error(w, "case failed", 500)
				return
			}
			if ok {
				d.Items = append(d.Items, dec)
			}
		case cases.Escalated:
			d.Escalate++
		}
	}
	d.Waiting = len(d.Items)
	switch {
	case d.Waiting == 0 && d.Escalate == 0:
		d.Summary = "Nothing is waiting."
	case d.Waiting == 0:
		d.Summary = "No plan waits for you. " + plural(d.Escalate, "escalated case still needs", "escalated cases still need") + " you on Now."
	default:
		d.Summary = plural(d.Waiting, "plan waits", "plans wait") + " for you. Each answer binds to the plan shown; a changed plan needs a new answer."
	}
	since, _ := u.API.Store.LastLook(ctx, s.User)
	t, ok := parseTime(since)
	if !ok {
		t = u.clockNow().Add(-24 * time.Hour)
		d.SinceAt = "in the last 24 hours"
	} else {
		d.SinceAt = "since you last looked (" + u.clock(since) + ")"
	}
	evs, err := u.API.Store.CaseEventsSince(ctx, t.UTC().Format(time.RFC3339), 200)
	if err != nil {
		http.Error(w, "history failed", 500)
		return
	}
	for _, e := range evs {
		if e.By != cases.ByOperator {
			continue
		}
		d.Recent = append(d.Recent, decidedRow{When: u.clock(e.At), ID: e.CaseID, Decision: eventWhat(e),
			From: strings.TrimPrefix(e.Actor, "user:"), Detail: e.Detail})
	}
	u.render(w, "approvals.html", 200, u.pageFor(s, "Approvals", "Approvals", r, d))
}
