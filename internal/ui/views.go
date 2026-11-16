package ui

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/chipi/flotilla/internal/api"
	"github.com/chipi/flotilla/internal/cases"
	"github.com/chipi/flotilla/internal/spec"
	"github.com/chipi/flotilla/internal/store"
)

// The view models the pages share: fleets, cases and times the way mockup v3 writes them.

func parseTime(s string) (time.Time, bool) {
	for _, l := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(l, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// clock writes a time the mockup's way, in the installation's zone: "07:40" today, "Sun 21:52" this
// week, "28 Oct" before that.
func (u *Server) clock(s string) string {
	t, ok := parseTime(s)
	if !ok {
		return "–"
	}
	now := u.clockNow()
	t = t.In(u.loc())
	n := now.In(u.loc())
	switch {
	case t.Year() == n.Year() && t.YearDay() == n.YearDay():
		return t.Format("15:04")
	case n.Sub(t) < 6*24*time.Hour && t.Before(n):
		return t.Format("Mon 15:04")
	case t.Year() == n.Year():
		return t.Format("2 Jan")
	}
	return t.Format("2 Jan 2006")
}

func (u *Server) loc() *time.Location {
	if u.API.Sched != nil && u.API.Sched.Location != nil {
		return u.API.Sched.Location
	}
	return time.UTC
}

func (u *Server) clockNow() time.Time {
	if u.API.Sched != nil && u.API.Sched.Now != nil {
		return u.API.Sched.Now()
	}
	return time.Now()
}

// age is how long ago, short: "41m", "3h 10m", "2d 4h".
func age(d time.Duration) string {
	if d < time.Minute {
		return "just now"
	}
	m := int(d.Minutes())
	switch {
	case m < 60:
		return fmt.Sprintf("%dm", m)
	case m < 24*60:
		if m%60 == 0 {
			return fmt.Sprintf("%dh", m/60)
		}
		return fmt.Sprintf("%dh %02dm", m/60, m%60)
	}
	return fmt.Sprintf("%dd %dh", m/(24*60), (m/60)%24)
}

func (u *Server) ago(s string) string {
	t, ok := parseTime(s)
	if !ok {
		return "–"
	}
	return age(u.clockNow().Sub(t))
}

// span writes a duration the way the time-to-close card does: "41 min", "2h 10m".
func span(d time.Duration) string {
	m := int(d.Round(time.Minute).Minutes())
	if m < 60 {
		return fmt.Sprintf("%d min", m)
	}
	return age(d)
}

// fleetOf is the fleet a routine id belongs to ("ops/backup-monitor" -> "ops").
func fleetOf(routine string) string {
	f, _, ok := strings.Cut(routine, "/")
	if !ok {
		return ""
	}
	return f
}

// title writes a fleet's name the way the mockup does ("ops" -> "Ops").
func title(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	r[0] = unicode.ToUpper(r[0])
	return string(r)
}

// fleetIcons are mockup v3's fleet marks on Cases; a fleet without one gets the platform's.
var fleetIcons = map[string]string{
	"triage":    "M12 3l8 4v5c0 5-3.5 8-8 9-4.5-1-8-4-8-9V7l8-4z",
	"ops":       "M14.7 6.3a4 4 0 0 0-5.4 5.4L3 18l3 3 6.3-6.3a4 4 0 0 0 5.4-5.4l-2.6 2.6-2.4-.6-.6-2.4 2.6-2.6z",
	"marketing": "M3 11l15-6v14L3 13v-2zM7 13v5",
	"bugfix":    "M8 6l-2-2M16 6l2-2M6 12H3M21 12h-3M7 18l-2 2M17 18l2 2M12 7a5 5 0 0 1 5 5v2a5 5 0 0 1-10 0v-2a5 5 0 0 1 5-5z",
	"":          "M12 8a4 4 0 1 0 0 8 4 4 0 0 0 0-8zM12 2v3M12 19v3M2 12h3M19 12h3",
}

func fleetIcon(f string) string {
	if p, ok := fleetIcons[f]; ok {
		return p
	}
	return fleetIcons[""]
}

// statusLabel writes a case status as words ("awaiting_approval" -> "waiting for you").
func statusLabel(s string) string {
	switch s {
	case cases.AwaitingApproval:
		return "waiting for you"
	case cases.HandedOver:
		return "handed over"
	}
	return strings.ReplaceAll(s, "_", " ")
}

// caseChip is a case status's voice (mockup v3's Cases).
func caseChip(s string) string {
	switch s {
	case cases.HandedOver, cases.Planned, cases.Executing, cases.Checking:
		return "chip chip--info"
	case cases.AwaitingApproval:
		return "chip chip--wait"
	case cases.Escalated:
		return "chip chip--stop"
	case cases.Closed:
		return "chip chip--ok"
	}
	return "chip chip--neutral"
}

func waiting(status string) bool {
	return status == cases.AwaitingApproval || status == cases.Escalated
}

// waitingCase is waiting, and also a handed-over case of a kind no routine acts on (the seo fleet's
// findings, RFC-0012): nothing but a person moves it.
func (u *Server) waitingCase(status, kind string) bool {
	if waiting(status) {
		return true
	}
	if status != cases.HandedOver || u.API.Sched == nil {
		return false
	}
	for _, j := range u.API.Sched.Routines {
		if j.OnCase() == kind {
			return false
		}
	}
	return true
}

var runPrefix = regexp.MustCompile(`^run ([A-Za-z0-9._:\-]+): ?`)

// splitRun takes the "run ID: " a routine puts before what it reports, so the run can be a link.
func splitRun(detail string) (run, rest string) {
	if m := runPrefix.FindStringSubmatch(detail); m != nil {
		return m[1], strings.TrimSpace(detail[len(m[0]):])
	}
	return "", detail
}

// problem is a case's sentence (G3: detections carry no title yet): the detection's own words, or the
// kind on the target.
func problem(c store.Case, first store.CaseEvent) string {
	if c.Title != "" {
		return c.Title
	}
	if _, rest := splitRun(first.Detail); rest != "" {
		r := []rune(rest)
		r[0] = unicode.ToUpper(r[0])
		return string(r)
	}
	return humanKind(c.Kind) + " on " + c.Target
}

func humanKind(k string) string {
	s := strings.ReplaceAll(k, "_", " ")
	if s == "" {
		return s
	}
	r := []rune(s)
	r[0] = unicode.ToUpper(r[0])
	return string(r)
}

// eventWhat is what a case event did, in words.
func eventWhat(e store.CaseEvent) string {
	if e.To != "" {
		switch e.To {
		case cases.Detected:
			return "Detected"
		case cases.HandedOver:
			if strings.HasPrefix(e.Detail, "reassigned") {
				return "Reassigned"
			}
			return "Handed over"
		case cases.Diagnosed:
			return "Diagnosed"
		case cases.Planned:
			return "Planned"
		case cases.AwaitingApproval:
			return "Asked you"
		case cases.Executing:
			return "Approved, executing"
		case cases.Checking:
			return "Executed, checking"
		case cases.Escalated:
			if strings.HasPrefix(e.Detail, "rejected") {
				return "Rejected, escalated"
			}
			return "Escalated"
		case cases.Closed:
			return "Closed"
		}
		return humanKind(e.To)
	}
	return humanKind(strings.ReplaceAll(e.Kind, ".", ": "))
}

// eventShort is a case's newest event for a table cell: "policy: waiting for you".
func eventShort(e store.CaseEvent) string {
	if e.To != "" {
		who := e.By
		if who == "" {
			who = e.Actor
		}
		return who + ": " + statusLabel(e.To)
	}
	return strings.ReplaceAll(e.Kind, ".", " ")
}

// eventDot is the timeline's colour: grey for flotilla or code, blue for an agent step, orange for
// waiting for you.
func eventDot(e store.CaseEvent) string {
	switch {
	case e.To == cases.AwaitingApproval || e.To == cases.Escalated:
		return "dot dot--you"
	case e.By == cases.ByAct || e.By == cases.ByDiagnose:
		return "dot dot--agent"
	}
	return "dot"
}

// kindRoutines maps each case kind to the routine that acts on it, and each routine id to its spec.
func (u *Server) specs() (map[string]string, map[string]*spec.Routine) {
	kinds, by := map[string]string{}, map[string]*spec.Routine{}
	if u.API.Sched == nil {
		return kinds, by
	}
	for _, j := range u.API.Sched.Routines {
		id := j.Fleet + "/" + j.Routine
		by[id] = j
		if k := j.OnCase(); k != "" {
			kinds[k] = id
		}
	}
	return kinds, by
}

// caseFleet is the fleet of the routine that acts on the case's kind, else of the routine that opened it.
func caseFleet(c store.Case, first store.CaseEvent, kinds map[string]string) string {
	if r, ok := kinds[c.Kind]; ok {
		return fleetOf(r)
	}
	return fleetOf(first.Actor)
}

type caseRow struct {
	store.Case
	Fleet, FleetName, Icon       string
	Title, StatusText, Chip, Age string
	Last                         store.CaseEvent
	LastWhen, LastWhat           string
	Opener                       string
}

// caseRows is every case as Cases, Now and Approvals show it.
func (u *Server) caseRows(ctx context.Context) ([]caseRow, error) {
	cs, err := u.API.Store.Cases(ctx, store.CaseFilter{Limit: 100000})
	if err != nil {
		return nil, err
	}
	ends, err := u.API.Store.CaseEventEnds(ctx)
	if err != nil {
		return nil, err
	}
	kinds, _ := u.specs()
	out := make([]caseRow, 0, len(cs))
	for _, c := range cs {
		e := ends[c.ID]
		f := caseFleet(c, e.First, kinds)
		out = append(out, caseRow{Case: c, Fleet: f, FleetName: title(f), Icon: fleetIcon(f), Title: problem(c, e.First),
			StatusText: statusLabel(c.Status), Chip: caseChip(c.Status), Age: u.ago(c.StatusAt), Last: e.Last,
			LastWhen: u.clock(e.Last.At), LastWhat: eventShort(e.Last), Opener: e.First.Actor})
		if c.Status == cases.Closed && c.Resolution != "" {
			out[len(out)-1].LastWhat = strings.ReplaceAll(c.Resolution, "_", " ")
		}
	}
	return out, nil
}

// whyWaiting is the one line a case card gives for why it needs the operator.
func whyWaiting(c caseRow) string {
	switch c.Status {
	case cases.AwaitingApproval:
		return "A plan waits for your answer. Open it to see what would run."
	case cases.Escalated:
		if c.Last.Detail != "" {
			return c.Last.Detail
		}
		return "Escalated: it needs a person. Reassign it, or close it with a reason."
	}
	return ""
}

// lampOf is a routine's state as a lamp: ok, wait, bad or idle.
func lampOf(x api.RoutineStatus, lastFailed bool) string {
	switch {
	case !x.Enabled || x.Stopped:
		return "idle"
	case x.Stale || lastFailed:
		return "bad"
	case x.Paused:
		return "wait"
	}
	return "ok"
}

func sortStrings(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// shortHash writes a plan hash the mockup's way: "4f1c…9e07".
func shortHash(h string) string {
	h = strings.TrimPrefix(h, "sha256:")
	if len(h) <= 10 {
		return h
	}
	return h[:4] + "…" + h[len(h)-4:]
}

func usd3(v float64) string { return fmt.Sprintf("$%.3f", v) }

func usd2(v float64) string { return fmt.Sprintf("$%.2f", v) }

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// agoText is ago as a phrase: "41m ago", or "just now" (never "just now ago").
func (u *Server) agoText(s string) string {
	a := u.ago(s)
	if a == "just now" || a == "–" {
		return a
	}
	return a + " ago"
}
