package ui

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/chipi/flotilla/internal/api"
	"github.com/chipi/flotilla/internal/hostexec"
	"github.com/chipi/flotilla/internal/spec"
	"github.com/chipi/flotilla/internal/store"
)

// Autonomy and guardrails (mockup v3's Autonomy): the brakes, each action's earned level, the routines'
// own levels, and the guardrails policy applies. Govern owns every autonomy change (the operator,
// 2026-11-14), so the freeze and the routine levels are changed here.

type actionRow struct {
	api.ActionStatus
	Kinds, Rev, Evidence, Record, Up, Down string
	CanUp, AtMax, Ready                    bool
	Pct                                    int // the bar toward the next level (G13)
}

type changeRow struct{ When, Text string }

type guard struct{ What, Value string }

type autonomyData struct {
	Frozen    bool
	FrozenBy  string
	FrozenAt  string
	Line      string
	Changes   []changeRow
	Actions   []actionRow
	Routines  []api.RoutineStatus
	Guards    []guard
	Hosts     []hostRow
	Ceiling   string
	FleetStop map[string]bool
	Recur     []recurRow
}

// recurRow is one kind under "Recurrence rule per kind" (G14).
type recurRow struct{ Kind, Rule string }

var levelUp = map[string]string{spec.AutonomyRecord: spec.AutonomyPropose, spec.AutonomyPropose: spec.AutonomyAuto}
var levelDown = map[string]string{spec.AutonomyAuto: spec.AutonomyPropose, spec.AutonomyPropose: spec.AutonomyRecord}

func (u *Server) autonomyPage(w http.ResponseWriter, r *http.Request, s *store.Session, sid string) {
	ctx := r.Context()
	routines, _, err := u.API.Status(ctx)
	if err != nil {
		http.Error(w, "status failed", 500)
		return
	}
	as, err := u.API.Actions(ctx)
	if err != nil {
		http.Error(w, "actions failed", 500)
		return
	}
	fr, err := u.API.FreezeState(ctx)
	if err != nil {
		http.Error(w, "freeze failed", 500)
		return
	}
	lim := u.API.Limits.WithDefaults()
	now := u.clockNow()
	execs, err := u.API.Store.HostExecutionsSince(ctx, now.Add(-time.Hour).UTC().Format(time.RFC3339))
	if err != nil {
		http.Error(w, "executions failed", 500)
		return
	}
	bad := 0
	for _, x := range execs {
		if x.Status == hostexec.Failed || x.Status == hostexec.Unknown {
			bad++
		}
	}
	breaker := "closed"
	if bad >= lim.BreakerFailures {
		breaker = "open"
	}
	d := autonomyData{Frozen: fr.Frozen, FrozenBy: strings.TrimPrefix(fr.By, "user:"), FrozenAt: u.clock(fr.At), Routines: routines,
		Ceiling: api.HostCeiling}
	if fr.Frozen {
		d.Line = "Autonomy is frozen."
	} else {
		d.Line = fmt.Sprintf("Autonomy is normal. The circuit breaker is %s (%d of %d failures in the last hour). Tightening takes effect at once; loosening asks why and is recorded.",
			breaker, bad, lim.BreakerFailures)
	}
	verdicts, err := u.API.Store.VerdictsByAction(ctx)
	if err != nil {
		http.Error(w, "feedback failed", 500)
		return
	}
	for _, a := range as {
		row := actionRow{ActionStatus: a, Kinds: strings.Join(a.ForKinds, ", "), Up: levelUp[a.Autonomy], Down: levelDown[a.Autonomy]}
		switch a.Reversible {
		case "yes":
			row.Rev = "yes"
		case "no":
			row.Rev = "no"
		default:
			row.Rev = a.Reversible
		}
		row.AtMax = spec.Rank(a.Autonomy) >= spec.Rank(a.Max)
		row.CanUp = row.Up != "" && !row.AtMax && !fr.Frozen
		row.Record = fmt.Sprintf("%s · verified %d · failed %d", plural(a.Record.Plans, "plan", "plans"), a.Record.Verified, a.Record.Failed)
		if v := verdicts[a.Name]; v[0]+v[1] > 0 {
			row.Record += fmt.Sprintf(" · diagnoses right %d, wrong %d", v[0], v[1])
		}
		st, err := u.API.Store.PromotionStreak(ctx, a.Name)
		if err != nil {
			http.Error(w, "evidence failed", 500)
			return
		}
		row.Evidence, row.Pct, row.Ready = evidence(a, row.AtMax, st)
		d.Actions = append(d.Actions, row)
	}
	evs, err := u.API.Store.Events(ctx, 300)
	if err != nil {
		http.Error(w, "events failed", 500)
		return
	}
	for _, e := range evs {
		if e.Kind != "control.autonomy" && e.Kind != "control.frozen" && e.Kind != "version.promote" && e.Kind != "version.rollback" {
			continue
		}
		who := strings.TrimPrefix(e.Actor, "user:")
		if e.Actor == "user:"+s.User {
			who = "You"
		}
		value, why, _ := strings.Cut(e.Detail, ": ")
		var text string
		if strings.HasPrefix(e.Kind, "version.") {
			// the version's own record (hash, evidence, reason) is on the routine page
			routine, ver, _ := strings.Cut(e.Subject, "@")
			text = who + " promoted " + ver + " of " + routine + "."
			if e.Kind == "version.rollback" {
				text = who + " rolled back " + ver + " of " + routine + "."
			}
			d.Changes = append(d.Changes, changeRow{When: u.clock(e.At), Text: text + " Details on its routine page."})
			if len(d.Changes) == 8 {
				break
			}
			continue
		}
		if e.Kind == "control.frozen" {
			if value == "1" {
				text = who + " froze all autonomy."
			} else {
				text = who + " unfroze autonomy."
			}
		} else {
			text = who + " set " + strings.TrimPrefix(e.Subject, "action:") + " to " + value + "."
		}
		if why != "" {
			text += ` Why: "` + why + `".`
		}
		d.Changes = append(d.Changes, changeRow{When: u.clock(e.At), Text: text})
		if len(d.Changes) == 8 {
			break
		}
	}
	execDay := 0
	dayStart := now.In(u.loc())
	dayStart = time.Date(dayStart.Year(), dayStart.Month(), dayStart.Day(), 0, 0, 0, 0, u.loc())
	if xs, err := u.API.Store.HostExecutionsSince(ctx, dayStart.UTC().Format(time.RFC3339)); err == nil {
		execDay = len(xs)
	}
	freeze := "off (Brakes, above)"
	if fr.Frozen {
		freeze = "on, since " + d.FrozenAt
	}
	d.Guards = []guard{
		{"Platform cap on executions", fmt.Sprintf("%d an hour (%d in the last hour, %d today)", lim.PlatformPerHour, len(execs), execDay)},
		{"Per-host cap", fmt.Sprintf("%d an hour on each host", lim.HostPerHour)},
		{"Circuit breaker", fmt.Sprintf("opens at %d failed or unknown results in 1h: every auto drops to propose", lim.BreakerFailures)},
		{"Host incident rule", fmt.Sprintf("%d new cases on one host in %s: auto paused on that host", lim.HostIncidentCases, words(api.IncidentWindow))},
		{"Autonomy freeze", freeze},
		{"Slack approvals", u.slackGuard(ctx)},
	}
	hosts, err := u.hostRows(r)
	if err != nil {
		http.Error(w, "hosts failed", 500)
		return
	}
	for _, h := range hosts {
		if h.RevokedAt == nil {
			d.Hosts = append(d.Hosts, h)
		}
	}
	rec, err := u.API.Store.Recurrences(ctx, now.Add(-30*24*time.Hour).UTC().Format(store.TimeFormat))
	if err != nil {
		http.Error(w, "recurrences failed", 500)
		return
	}
	seen := map[string]bool{}
	var kinds []string
	for _, a := range d.Actions {
		for _, k := range a.ForKinds {
			if !seen[k] {
				seen[k] = true
				kinds = append(kinds, k)
			}
		}
	}
	for k := range rec {
		if !seen[k] {
			seen[k] = true
			kinds = append(kinds, k)
		}
	}
	sort.Strings(kinds)
	days := int(store.RecurrenceWindow.Hours() / 24)
	for _, k := range kinds {
		d.Recur = append(d.Recur, recurRow{Kind: k,
			Rule: fmt.Sprintf("same target within %d days of a close: linked to the earlier case, at most propose after a verified fix; %d in 30 days", days, rec[k])})
	}
	u.render(w, "autonomy.html", 200, u.pageFor(s, "Autonomy", "Autonomy", r, d))
}

// actionAutonomy promotes or demotes an action from the Autonomy page: the same rules as the API's.
func (u *Server) actionAutonomy(w http.ResponseWriter, r *http.Request, s *store.Session, sid string) {
	name, level := r.PathValue("name"), r.PostFormValue("autonomy")
	err := u.API.SetActionAutonomy(r.Context(), caller(s), "ui", name, level, r.PostFormValue("reason"))
	done(w, r, "/autonomy", err, name+" is now "+level+".")
}

// freezeAction freezes at once, or unfreezes with a reason; both from the brakes on Autonomy.
func (u *Server) freezeAction(frozen bool) func(http.ResponseWriter, *http.Request, *store.Session, string) {
	return func(w http.ResponseWriter, r *http.Request, s *store.Session, sid string) {
		ok := "Frozen: every routine runs at record until you unfreeze."
		if !frozen {
			ok = "Unfrozen: each routine runs at its own autonomy again."
		}
		back := safeBack(r.PostFormValue("back"), "/autonomy")
		done(w, r, back, u.API.SetFrozen(r.Context(), caller(s), "ui", frozen, r.PostFormValue("reason")), ok)
	}
}

// routineAction is run, pause, resume, autonomy or rollback on a routine, from a form.
func (u *Server) routineAction(w http.ResponseWriter, r *http.Request, s *store.Session, sid string) {
	id := r.PathValue("fleet") + "/" + r.PathValue("routine")
	back := safeBack(r.PostFormValue("back"), "/fleets/"+r.PathValue("fleet"))
	reason := r.PostFormValue("reason")
	var err error
	var ok string
	switch r.PathValue("action") {
	case "run":
		err, ok = u.API.RunNow(r.Context(), caller(s), "ui", id), "Started "+id+"."
	case "pause":
		err, ok = u.API.SetPaused(r.Context(), caller(s), "ui", id, true, ""), "Paused "+id+"."
	case "resume":
		err, ok = u.API.SetPaused(r.Context(), caller(s), "ui", id, false, reason), "Resumed "+id+"."
	case "autonomy":
		level := r.PostFormValue("autonomy")
		err, ok = u.API.SetAutonomy(r.Context(), caller(s), "ui", id, level, reason), id+" is now at "+level+"."
	case "rollback":
		u.rollbackAction(w, r, s, id, safeBack(r.PostFormValue("back"), "/fleets/"+id+"#versions"))
		return
	default:
		http.NotFound(w, r)
		return
	}
	done(w, r, back, err, ok)
}

func (u *Server) fleetAction(w http.ResponseWriter, r *http.Request, s *store.Session, sid string) {
	f := r.PathValue("fleet")
	back := safeBack(r.PostFormValue("back"), "/fleets/"+f)
	var err error
	var ok string
	switch r.PathValue("action") {
	case "stop":
		err, ok = u.API.SetStopped(r.Context(), caller(s), "ui", f, true, ""), "STOPped fleet "+f+": none of its routines start, and its runs in flight are cancelled."
	case "start":
		err, ok = u.API.SetStopped(r.Context(), caller(s), "ui", f, false, r.PostFormValue("reason")), "Started fleet "+f+"."
	default:
		http.NotFound(w, r)
		return
	}
	done(w, r, back, err, ok)
}

// evidence is the Actions table's line and bar toward the next level (G13): the clean plans in a row
// the store counts, out of store.PromotionNeed, and why the bar is held.
func evidence(a api.ActionStatus, atMax bool, st store.Streak) (string, int, bool) {
	if a.Reversible == "no" && atMax {
		return "irreversible: never auto; CLI approval only", 0, false
	}
	if atMax {
		return fmt.Sprintf("at %s, its highest level; %s", a.Autonomy, plural(a.Record.Failed, "failure", "failures")), 100, false
	}
	what := "approved and verified"
	if a.Autonomy == spec.AutonomyRecord {
		what = "recorded plans you judged right"
	}
	text := fmt.Sprintf("%s: %d of %d", what, st.Clean, store.PromotionNeed)
	pct := st.Clean * 100 / store.PromotionNeed
	switch {
	case st.Held:
		text += fmt.Sprintf("; held: a %s plan on %s, under %d days ago", st.BadWhat, shortDay(st.BadAt), int(store.PromotionHold.Hours()/24))
		if pct >= 100 {
			pct = 80
		}
	case st.Ready():
		text += ", ready"
	}
	return text, pct, st.Ready()
}

func shortDay(at string) string {
	if t, ok := parseTime(at); ok {
		return t.UTC().Format("2006-01-02")
	}
	return at
}
