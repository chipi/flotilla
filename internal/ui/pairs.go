package ui

import (
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/chipi/flotilla/internal/api"
	"github.com/chipi/flotilla/internal/store"
)

// A shadow version's pairs on the routine page (docs/wip/v2-design.md section 4, build step 2): per subject what the live version and the
// shadow version each decided on the same input, "differ" and "same" as marks, and each row opens the two runs side by side. Mobile
// first: a list of rows that wrap, never a table that scrolls sideways.

const maxPairRows = 100

type pairRow struct {
	Kind, Label, Subject, At string // Kind: differ, same or unpaired
	LiveText, ShadowText     string
	Href                     string
	LiveV, ShadowV           int // the versions compared, for the row on its own
}

type pairsData struct {
	Routine         string
	N, Live         int
	Paired, Differ  int
	Unpaired        int
	Show            string
	Rows            []pairRow
	More            int // rows past the page
	Choices         []pairChoice
	Versions        []pairVersion // the shadow versions that have pairs, to pick another
	NoShadowRunsYet bool
}

type pairChoice struct {
	Label, Href string
	On          bool
}

type pairVersion struct {
	Label, Href string
	On          bool
}

func pairHref(id string, n int, show string) string {
	q := url.Values{"pairs": {strconv.Itoa(n)}}
	if show != "" && show != "all" {
		q.Set("show", show)
	}
	return "/fleets/" + id + "?" + q.Encode() + "#pairs"
}

// pairsFor is the routine page's pairs list, for the shadow version asked for (?pairs=N) or the newest one that has a finished shadow run;
// nil when the routine has none.
func (u *Server) pairsFor(r *http.Request, id string, v *api.VersionsView) (*pairsData, error) {
	var withRuns []int
	for _, x := range v.Versions {
		if x.State != store.VersionShadow {
			continue
		}
		rows, err := u.API.Store.ShadowRuns(r.Context(), store.ShadowRunFilter{Routine: id, Version: x.Version, Status: store.ShadowDone})
		if err != nil {
			return nil, err
		}
		if len(rows) > 0 {
			withRuns = append(withRuns, x.Version)
		}
	}
	if len(withRuns) == 0 {
		return nil, nil
	}
	n := withRuns[len(withRuns)-1]
	if asked, err := strconv.Atoi(r.URL.Query().Get("pairs")); err == nil {
		for _, w := range withRuns {
			if w == asked {
				n = asked
			}
		}
	}
	show := r.URL.Query().Get("show")
	if show != "differ" && show != "unpaired" {
		show = "all"
	}
	all, err := u.API.Store.Pairs(r.Context(), store.PairQuery{Routine: id, Version: n})
	if err != nil {
		return nil, err
	}
	shown, err := api.FilterPairs(all, show)
	if err != nil {
		return nil, err
	}
	d := &pairsData{Routine: id, N: n, Live: all.LiveVersion, Paired: all.Paired, Differ: all.Differ, Unpaired: all.Unpaired, Show: show}
	for _, c := range []struct{ label, show string }{{"all", "all"}, {"differ", "differ"}, {fmt.Sprintf("unpaired %d", all.Unpaired), "unpaired"}} {
		d.Choices = append(d.Choices, pairChoice{Label: c.label, Href: pairHref(id, n, c.show), On: c.show == show})
	}
	if len(withRuns) > 1 {
		for _, w := range withRuns {
			d.Versions = append(d.Versions, pairVersion{Label: fmt.Sprintf("v%d", w), Href: pairHref(id, w, show), On: w == n})
		}
	}
	for i, p := range shown.Pairs {
		if i == maxPairRows {
			d.More = len(shown.Pairs) - maxPairRows
			break
		}
		row := pairRow{LiveV: all.LiveVersion, ShadowV: n, Subject: p.Subject, At: u.clock(p.At), Href: fmt.Sprintf("/fleets/%s/pairs/%d/%s?subject=%s", id, n, url.PathEscape(p.InputKey), url.QueryEscape(p.Subject))}
		switch {
		case p.Live == nil || p.Shadow == nil:
			row.Kind, row.Label = "unpaired", "unpaired"
		case p.Same:
			row.Kind, row.Label = "same", "same"
		default:
			row.Kind, row.Label = "differ", "differ"
		}
		row.LiveText, row.ShadowText = sideText(p.Live), sideText(p.Shadow)
		d.Rows = append(d.Rows, row)
	}
	return d, nil
}

// sideText is what a side decided, or that it did not.
func sideText(s *store.PairSide) string {
	switch {
	case s == nil:
		return "did not decide it"
	case s.Text == "":
		return "(no outcome recorded)"
	}
	return s.Text
}

type pairPage struct {
	Routine, Subject, At string
	N, Live              int
	Kind, Label          string
	InputKey             string
	Sides                [2]pairSideView
	Back                 string
}

type pairSideView struct {
	Who, Role, Chip string // "live v1", "shadow v2"
	Present         bool
	Text, Detail    string
	Hash, FullHash  string
	RunHref, RunID  string
	Action          string
	Params          []string
	Note            string
	Stations        []stationRow
	NoStations      bool
	Texts           []textRow // every rendered text of every station of this side, with whether the other side read the same
}

type stationRow struct {
	Role, Calls, In, Out, Cost string
}

// textRow is one rendered text a side's station read. Mark says how it compares with the other side's text of the same station and name:
// equal (same hash), differs (another hash), or only here.
type textRow struct {
	Label, Name, Short, FullHash, Bytes string
	Body                                string
	HasBody                             bool
	Mark, MarkKind                      string
}

// markTexts sets each side's texts' marks against the other side.
func markTexts(a, b *pairSideView) {
	key := func(t textRow) string { return t.Label + "\x00" + t.Name }
	other := func(rows []textRow) map[string]string {
		m := map[string]string{}
		for _, t := range rows {
			m[key(t)] = t.FullHash
		}
		return m
	}
	oa, ob := other(a.Texts), other(b.Texts)
	set := func(rows []textRow, against map[string]string, bothSides bool) {
		for i := range rows {
			h, ok := against[key(rows[i])]
			switch {
			case !bothSides || !ok:
				rows[i].Mark, rows[i].MarkKind = "only here", "unpaired"
			case h == rows[i].FullHash:
				rows[i].Mark, rows[i].MarkKind = "equal", "same"
			default:
				rows[i].Mark, rows[i].MarkKind = "differs", "differ"
			}
		}
	}
	set(a.Texts, ob, a.Present && b.Present)
	set(b.Texts, oa, a.Present && b.Present)
}

func (u *Server) pairPage(w http.ResponseWriter, r *http.Request, s *store.Session, sid string) {
	id := r.PathValue("fleet") + "/" + r.PathValue("routine")
	n, err := strconv.Atoi(r.PathValue("n"))
	if err != nil || n < 1 {
		http.NotFound(w, r)
		return
	}
	key, subject := r.PathValue("input"), r.URL.Query().Get("subject")
	all, err := u.API.Store.Pairs(r.Context(), store.PairQuery{Routine: id, Version: n})
	if err != nil {
		http.Error(w, "pairs failed", 500)
		return
	}
	for _, p := range all.Pairs {
		if p.InputKey != key || p.Subject != subject {
			continue
		}
		pp := pairPage{Routine: id, Subject: p.Subject, At: u.clock(p.At), N: n, Live: all.LiveVersion, InputKey: p.InputKey,
			Back: "/fleets/" + id + "?pairs=" + strconv.Itoa(n) + "#pairs"}
		switch {
		case p.Live == nil || p.Shadow == nil:
			pp.Kind, pp.Label = "unpaired", "unpaired"
		case p.Same:
			pp.Kind, pp.Label = "same", "same"
		default:
			pp.Kind, pp.Label = "differ", "differ"
		}
		pp.Sides[0] = u.sideView(fmt.Sprintf("live v%d", all.LiveVersion), "live", p.Live)
		pp.Sides[1] = u.sideView(fmt.Sprintf("v%d", n), "shadow", p.Shadow)
		markTexts(&pp.Sides[0], &pp.Sides[1])
		u.render(w, "pair.html", 200, u.pageFor(s, "Pair "+p.Subject, "Fleets", r, pp))
		return
	}
	http.NotFound(w, r)
}

func (u *Server) sideView(who, role string, p *store.PairSide) pairSideView {
	v := pairSideView{Who: who, Role: role, Chip: "chip--neutral", Present: p != nil}
	if p == nil {
		v.Note = "did not decide this subject on this input"
		return v
	}
	v.Chip = map[string]string{"live": "chip--ok", "shadow": "chip--info"}[role]
	v.Text, v.Detail, v.FullHash, v.Hash = sideText(p), p.Detail, p.Hash, p.Hash
	if len(v.Hash) > 7 {
		v.Hash = v.Hash[:7]
	}
	v.RunID, v.RunHref = p.RunID, "/runs/"+url.PathEscape(p.RunID)
	if p.Plan != nil {
		v.Action = p.Plan.Action
		for k, val := range p.Plan.Params {
			v.Params = append(v.Params, k+"="+val)
		}
		sort.Strings(v.Params)
		if p.Plan.Blocked != "" {
			v.Note = "blocked: " + p.Plan.Blocked + " (a shadow's action is recorded and never sent)"
		}
	}
	for _, st := range p.Stations {
		v.Stations = append(v.Stations, stationRow{Role: st.Role, Calls: strconv.Itoa(st.Calls), In: strconv.FormatInt(st.TokensIn, 10), Out: strconv.FormatInt(st.TokensOut, 10),
			Cost: "$" + strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.4f", st.CostUSD), "0"), ".")})
		for _, t := range st.Texts {
			short := t.SHA256
			if len(short) > 7 {
				short = short[:7]
			}
			row := textRow{Label: st.Role, Name: t.Name, Short: short, FullHash: t.SHA256, Bytes: strconv.FormatInt(t.Bytes, 10)}
			if t.Text != nil {
				row.Body, row.HasBody = *t.Text, true
			}
			v.Texts = append(v.Texts, row)
		}
	}
	v.NoStations = len(v.Stations) == 0
	return v
}
