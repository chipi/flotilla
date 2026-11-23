package ui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/chipi/flotilla/internal/cases"
	"github.com/chipi/flotilla/internal/store"
)

// The morning brief (mockup v3's Brief, G11): written once a day at brief_at by flotillad from its own
// record (the facts Now shows), kept as written, with an archive. A person marks an item "more like
// this" or "less"; the marks are counted per kind. The mockup's brief is written by a model routine
// whose playbook the marks reshape: that summary and those proposed changes are not built and are
// named in place.

const briefWriter = "flotillad, from its own record (no model)"

// certWarn is how soon a host agent certificate's expiry goes into the brief's Watch section.
const certWarn = 14 * 24 * time.Hour

// composeBrief writes the brief for the day of now.
func (u *Server) composeBrief(ctx context.Context, now time.Time) (store.Brief, error) {
	n := now.In(u.loc())
	b := store.Brief{Day: n.Format("2006-01-02"), WrittenAt: now.UTC().Format(store.TimeFormat), WrittenBy: briefWriter}
	rows, err := u.caseRows(ctx)
	if err != nil {
		return b, err
	}
	fs, routines, workers, err := u.fleetSums(ctx, rows)
	if err != nil {
		return b, err
	}
	wn, err := u.wrongNow(ctx, now, routines, workers)
	if err != nil {
		return b, err
	}

	var needs, handled []store.BriefItem
	verified, closed := 0, 0
	dayAgo := now.Add(-24 * time.Hour)
	var closedRows []caseRow
	for _, c := range rows {
		if u.waitingCase(c.Status, c.Kind) {
			needs = append(needs, store.BriefItem{Text: c.Title + ": " + statusLabel(c.Status) + ".", Case: c.ID, Href: "/cases/" + c.ID, Kind: "needs:" + c.Kind})
		}
		if c.Status == cases.Closed && c.ClosedAt != nil {
			if t, ok := parseTime(*c.ClosedAt); ok && !t.Before(dayAgo) {
				closedRows = append(closedRows, c)
			}
		}
	}
	sort.Slice(closedRows, func(i, j int) bool { return *closedRows[i].ClosedAt > *closedRows[j].ClosedAt })
	for _, c := range closedRows {
		closed++
		if c.Resolution == cases.Verified {
			verified++
		}
		if len(handled) < 10 {
			handled = append(handled, store.BriefItem{Text: c.Title + " (" + strings.ReplaceAll(c.Resolution, "_", " ") + ").",
				Case: c.ID, Href: "/cases/" + c.ID, Kind: "handled:" + c.Kind})
		}
	}
	if closed > len(handled) {
		handled = append(handled, store.BriefItem{Text: fmt.Sprintf("And %d more in History.", closed-len(handled)), Href: "/history", Kind: "handled:more"})
	}

	var watch []store.BriefItem
	for _, w := range wn.Items {
		kind := "wrong:other"
		switch {
		case strings.Contains(w.What, " is stale"):
			kind = "wrong:stale"
		case strings.Contains(w.What, " failed "):
			kind = "wrong:failed_runs"
		case strings.Contains(w.What, "host agent not connected"):
			kind = "wrong:host_down"
		case strings.Contains(w.What, "worker "):
			kind = "wrong:worker"
		case strings.Contains(w.What, " is paused"), strings.Contains(w.What, "STOPped"), strings.Contains(w.What, "frozen"):
			kind = "wrong:brakes"
		}
		watch = append(watch, store.BriefItem{Text: w.What + ": " + w.Detail + ".", Href: w.Href, Kind: kind})
	}
	hosts, err := u.API.Store.Hosts(ctx)
	if err != nil {
		return b, err
	}
	for _, h := range hosts {
		if h.RevokedAt != nil || h.CertNotAfter == "" {
			continue
		}
		if t, ok := parseTime(h.CertNotAfter); ok && t.Sub(now) < certWarn {
			watch = append(watch, store.BriefItem{Text: fmt.Sprintf("%s's host agent certificate expires in %s; renewal needs the agent connected.",
				h.Name, age(t.Sub(now))), Href: "/hosts/" + h.Name + "?tab=agent", Kind: "watch:certificate"})
		}
	}

	spend := 0.0
	for _, f := range fs {
		spend += f.Spend24
	}
	switch len(needs) {
	case 0:
		b.Headline = "Nothing needs you"
	case 1:
		b.Headline = "1 thing needs you"
	default:
		b.Headline = fmt.Sprintf("%d things need you", len(needs))
	}
	if len(watch) > 0 {
		b.Headline += fmt.Sprintf("; %s to watch", plural(len(watch), "thing", "things"))
	}
	b.Headline += "."
	b.Summary = fmt.Sprintf("In the last 24 hours %s closed without you (%d verified after a fix). %s Runs cost %s.",
		plural(closed, "case was", "cases were"), verified, map[bool]string{true: "Nothing needs you.", false: plural(len(needs), "case needs", "cases need") + " you."}[len(needs) == 0],
		usd2(spend))
	trusted, err := u.trustItems(ctx, routines)
	if err != nil {
		return b, err
	}
	for _, sec := range []store.BriefSection{{Title: "Needs you", Items: needs}, {Title: "Trust", Items: trusted}, {Title: "Handled overnight", Items: handled}, {Title: "Watch", Items: watch}} {
		if len(sec.Items) > 0 {
			b.Sections = append(b.Sections, sec)
		}
	}
	mix, err := u.decisionItems(ctx, fs, now)
	if err != nil {
		return b, err
	}
	if len(mix) > 0 {
		b.Sections = append(b.Sections, store.BriefSection{Title: "Decisions", Items: mix})
	}
	more, err := u.summarySections(ctx, dayAgo)
	if err != nil {
		return b, err
	}
	b.Sections = append(b.Sections, more...)
	return b, nil
}

// summarySections are the sections monitors fill (RFC-0012 §4): for each routine whose spec names a
// brief_section, the summary lines of its newest run, when that run ended since since; a weekly routine
// so appears in one brief a week. Sections keep the order of their first routine by id.
func (u *Server) summarySections(ctx context.Context, since time.Time) ([]store.BriefSection, error) {
	if u.API.Sched == nil {
		return nil, nil
	}
	var ids []string
	byID := map[string]string{}
	for _, j := range u.API.Sched.Routines() {
		if j.BriefSection != "" {
			ids = append(ids, j.ID())
			byID[j.ID()] = j.BriefSection
		}
	}
	sort.Strings(ids)
	var out []store.BriefSection
	at := map[string]int{}
	for _, id := range ids {
		e, err := u.API.Store.LatestEvent(ctx, store.SummaryKind, id)
		if err != nil {
			return nil, err
		}
		if e == nil {
			continue
		}
		if t, ok := parseTime(e.At); !ok || t.Before(since) {
			continue
		}
		var s store.Summary
		if json.Unmarshal([]byte(e.Detail), &s) != nil || len(s.Lines) == 0 {
			continue
		}
		title := byID[id]
		i, ok := at[title]
		if !ok {
			i = len(out)
			at[title] = i
			out = append(out, store.BriefSection{Title: title})
		}
		for _, ln := range s.Lines {
			out[i].Items = append(out[i].Items, store.BriefItem{Text: ln, Href: "/runs/" + s.Run, Kind: "summary:" + id})
		}
	}
	return out, nil
}

// WriteBriefIfDue writes today's brief once it is past brief_at and today has none. flotillad calls it
// every minute.
func (u *Server) WriteBriefIfDue(ctx context.Context) error {
	now := u.clockNow()
	n := now.In(u.loc())
	h, m := u.briefAt()
	if n.Hour()*60+n.Minute() < h*60+m {
		return nil
	}
	if b, err := u.API.Store.BriefOn(ctx, n.Format("2006-01-02")); err != nil || b != nil {
		return err
	}
	b, err := u.composeBrief(ctx, now)
	if err != nil {
		return err
	}
	if err := u.API.Store.SaveBrief(ctx, b); err != nil && !errors.Is(err, store.ErrBriefExists) {
		return err
	}
	u.Log.Info("brief written", "day", b.Day, "headline", b.Headline)
	return nil
}

// briefAt is flotillad.yaml brief_at ("07:00", the installation's timezone).
func (u *Server) briefAt() (int, int) {
	if t, err := time.Parse("15:04", u.BriefAt); err == nil {
		return t.Hour(), t.Minute()
	}
	return 7, 0
}

type briefLine struct {
	store.BriefItem
	N    int
	Mark string
}

type briefSec struct {
	Title string
	Lines []briefLine
}

type briefData struct {
	B        *store.Brief
	Date     string
	Written  string
	Sections []briefSec
	Earlier  []briefLink
	Next     string
	Archive  bool
	All      []briefLink
	Marks    []string // this week's marks per kind
}

type briefLink struct{ Href, Date, Headline string }

func (u *Server) briefPage(w http.ResponseWriter, r *http.Request, s *store.Session, sid string) {
	ctx := r.Context()
	all, err := u.API.Store.Briefs(ctx, 400)
	if err != nil {
		http.Error(w, "brief failed", 500)
		return
	}
	d := briefData{Archive: r.URL.Path == "/brief/archive"}
	day := r.URL.Query().Get("day")
	for i := range all {
		x := &all[i]
		link := briefLink{Href: "/brief?day=" + x.Day, Date: u.briefDate(x.Day), Headline: x.Headline}
		d.All = append(d.All, link)
		if (day == "" && d.B == nil) || x.Day == day {
			d.B = x
			continue
		}
		if d.B != nil && len(d.Earlier) < 2 && x.Day < d.B.Day {
			d.Earlier = append(d.Earlier, link)
		}
	}
	h, m := u.briefAt()
	d.Next = fmt.Sprintf("%02d:%02d", h, m)
	if d.B != nil {
		d.Date = u.briefDate(d.B.Day)
		d.Written = u.clock(d.B.WrittenAt)
		marks, err := u.API.Store.BriefMarks(ctx, d.B.Day, s.User)
		if err != nil {
			http.Error(w, "brief failed", 500)
			return
		}
		n := 0
		for _, sec := range d.B.Sections {
			bs := briefSec{Title: sec.Title}
			for _, it := range sec.Items {
				bs.Lines = append(bs.Lines, briefLine{BriefItem: it, N: n, Mark: marks[n]})
				n++
			}
			d.Sections = append(d.Sections, bs)
		}
	}
	week, err := u.API.Store.MarksByKind(ctx, u.clockNow().Add(-7*24*time.Hour).UTC().Format(store.TimeFormat))
	if err != nil {
		http.Error(w, "brief failed", 500)
		return
	}
	for _, k := range sortedKeys(week) {
		d.Marks = append(d.Marks, fmt.Sprintf("%s: %d more, %d less", k, week[k][0], week[k][1]))
	}
	title := "Brief"
	if d.Archive {
		title = "Earlier briefs"
	}
	u.render(w, "brief.html", 200, u.pageFor(s, title, "Brief", r, d))
}

func (u *Server) briefDate(day string) string {
	if t, err := time.ParseInLocation("2006-01-02", day, u.loc()); err == nil {
		return t.Format("Monday 2 January")
	}
	return day
}

// briefMark keeps a person's "more like this" or "less" on one item.
func (u *Server) briefMark(w http.ResponseWriter, r *http.Request, s *store.Session, sid string) {
	day := r.PathValue("day")
	item, err := strconv.Atoi(r.PostFormValue("item"))
	if err == nil {
		err = u.API.Store.MarkBriefItem(r.Context(), day, item, s.User, r.PostFormValue("mark"))
	}
	done(w, r, "/brief?day="+day, err, "Noted: the brief's kinds keep your marks.")
}
