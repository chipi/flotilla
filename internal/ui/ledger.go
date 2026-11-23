package ui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/chipi/flotilla/internal/api"
	"github.com/chipi/flotilla/internal/store"
)

// The ledger explorer (the operator, 2026-11-23): every record the store keeps, as written, across its three streams, for
// finding out by hand what happened when no other page says. Read only; the roles that may read the ledger in History.

var streamWords = map[string]string{store.StreamPlatform: "Platform", store.StreamCase: "Cases", store.StreamLedger: "Fleet ledgers"}

type ledgerRow struct {
	store.ExploreRow
	When, StreamLabel   string
	KindHref, ActorHref string
	FollowHref          string // every row naming this row's subject
	Open, OpenLabel     string // the subject's own page, when it has one
	RunFollow           string // every row naming this row's run
}

type kindOpt struct {
	Value, Label string
	On           bool
}

type ledgerData struct {
	T                tableQ
	Streams, Periods []pillLink
	Kinds            []kindOpt
	Heads            []thCell
	Rows             []ledgerRow
	Pivots           []pivot
	Clear            string
	From, To         string
	Export           string
}

// ledgerQuery reads a view from the request: stream, kind, actor, ref (follow one thing), q (text), period or from/to.
func (u *Server) ledgerQuery(t tableQ) store.ExploreQuery {
	q := store.ExploreQuery{Kind: t.Get("kind"), Actor: t.Get("actor"), Ref: t.Get("ref"), Text: strings.TrimSpace(t.Get("q")),
		Sort: map[string]string{"when": "at", "kind": "kind", "actor": "actor", "stream": "stream"}[t.Sort], Desc: t.Desc}
	if s := t.Get("stream"); streamWords[s] != "" {
		q.Streams = []string{s}
	}
	now := u.clockNow()
	switch t.Get("period") {
	case "day":
		q.From = now.Add(-24 * time.Hour).UTC().Format(store.TimeFormat)
	case "week":
		q.From = now.AddDate(0, 0, -7).UTC().Format(store.TimeFormat)
	case "month":
		q.From = now.AddDate(0, 0, -30).UTC().Format(store.TimeFormat)
	}
	if d, err := time.ParseInLocation("2006-01-02", t.Get("from"), u.loc()); err == nil {
		q.From = d.UTC().Format(store.TimeFormat)
	}
	if d, err := time.ParseInLocation("2006-01-02", t.Get("to"), u.loc()); err == nil {
		q.To = d.Add(24*time.Hour - time.Nanosecond).UTC().Format(store.TimeFormat)
	}
	return q
}

func (u *Server) ledgerPage(w http.ResponseWriter, r *http.Request, s *store.Session, sid string) {
	ctx := r.Context()
	if err := u.API.Check(ctx, caller(s), api.ActLedgerRead); err != nil {
		http.Error(w, "your role may not read the ledger", http.StatusForbidden)
		return
	}
	t := readTable(r, "when", true, func(c string) bool { return c == "when" || c == "kind" || c == "actor" || c == "stream" })
	q := u.ledgerQuery(t)
	q.Limit, q.Offset = t.Per, t.Offset()
	rows, total, kinds, err := u.API.Store.Explore(ctx, q)
	if err != nil {
		http.Error(w, "ledger failed", 500)
		return
	}
	t.Total = total
	d := ledgerData{T: t, From: t.Get("from"), To: t.Get("to")}
	per := map[string]int{}
	all := 0
	for _, k := range kinds {
		per[k.Stream] += k.N
		all += k.N
	}
	stream := t.Get("stream")
	d.Streams = append(d.Streams, pillLink{Label: fmt.Sprintf("All %d", all), Href: t.With("stream", ""), On: stream == ""})
	for _, s := range []string{store.StreamPlatform, store.StreamCase, store.StreamLedger} {
		if per[s] > 0 || stream == s {
			d.Streams = append(d.Streams, pillLink{Label: fmt.Sprintf("%s %d", streamWords[s], per[s]), Href: t.With("stream", s), On: stream == s})
		}
	}
	period := t.Get("period")
	for _, p := range [][2]string{{"", "All time"}, {"day", "24 hours"}, {"week", "7 days"}, {"month", "30 days"}} {
		d.Periods = append(d.Periods, pillLink{Label: p[1], Href: t.With("period", p[0]), On: period == p[0] && d.From == "" && d.To == ""})
	}
	d.Kinds = append(d.Kinds, kindOpt{Value: "", Label: "Every kind", On: q.Kind == ""})
	for _, k := range kinds {
		d.Kinds = append(d.Kinds, kindOpt{Value: k.Kind, Label: fmt.Sprintf("%s · %s (%d)", streamWords[k.Stream], k.Kind, k.N), On: k.Kind == q.Kind})
	}
	for _, f := range [][2]string{{"ref", "following"}, {"kind", "kind"}, {"actor", "actor"}} {
		if v := t.Get(f[0]); v != "" {
			d.Pivots = append(d.Pivots, pivot{Field: f[1], Value: v, Remove: t.With(f[0], "")})
		}
	}
	if len(d.Pivots) > 0 {
		d.Clear = t.With2("ref", "", "kind", "") // actor stays only through its own ×; Clear all drops the three
		d.Clear = withoutKeys(d.Clear, "actor")
	}
	d.Export = "/ledger.json?" + exportQuery(r.URL.Query())
	for _, x := range rows {
		row := ledgerRow{ExploreRow: x, When: u.clock(x.At), StreamLabel: streamWords[x.Stream],
			KindHref: t.With("kind", x.Kind), ActorHref: t.With("actor", x.Actor)}
		if x.Subject != "" {
			row.FollowHref = ledgerHref(x.Subject)
		}
		switch {
		case x.CaseID != "":
			row.Open, row.OpenLabel = "/cases/"+x.CaseID, "Open the case"
		case x.RunID != "" && x.RunID == x.Subject:
			row.Open, row.OpenLabel = "/runs/"+x.RunID, "Open the run"
		}
		if x.RunID != "" && x.RunID != x.Subject {
			row.RunFollow = ledgerHref(x.RunID)
		}
		d.Rows = append(d.Rows, row)
	}
	d.Heads = []thCell{t.Th("when", "When"), t.Th("stream", "Stream"), t.Th("kind", "Kind"), t.Th("actor", "Actor"), t.Plain("Subject"), t.Plain("Detail")}
	u.render(w, "ledger.html", 200, u.pageFor(s, "Ledger", "Ledger", r, d))
}

// ledgerExport is the current view as JSON, up to 10,000 rows, newest first unless sorted otherwise.
func (u *Server) ledgerExport(w http.ResponseWriter, r *http.Request, s *store.Session, sid string) {
	ctx := r.Context()
	if err := u.API.Check(ctx, caller(s), api.ActLedgerRead); err != nil {
		http.Error(w, "your role may not read the ledger", http.StatusForbidden)
		return
	}
	t := readTable(r, "when", true, func(c string) bool { return c == "when" || c == "kind" || c == "actor" || c == "stream" })
	q := u.ledgerQuery(t)
	q.Limit = 10000
	rows, total, _, err := u.API.Store.Explore(ctx, q)
	if err != nil {
		http.Error(w, "ledger failed", 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", `attachment; filename="flotilla-ledger.json"`)
	_ = json.NewEncoder(w).Encode(map[string]any{"total": total, "rows": rows})
}

// ledgerHref is the explorer following one thing: a case, a run, a fingerprint, a person, an actor.
func ledgerHref(ref string) string { return "/ledger?ref=" + url.QueryEscape(ref) }

func exportQuery(v url.Values) string {
	out := url.Values{}
	for k, vs := range v {
		if k != "page" {
			out[k] = vs
		}
	}
	return out.Encode()
}

func withoutKeys(href string, keys ...string) string {
	p, q, ok := strings.Cut(href, "?")
	if !ok {
		return href
	}
	v, err := url.ParseQuery(q)
	if err != nil {
		return href
	}
	for _, k := range keys {
		v.Del(k)
	}
	if len(v) == 0 {
		return p
	}
	return p + "?" + v.Encode()
}
