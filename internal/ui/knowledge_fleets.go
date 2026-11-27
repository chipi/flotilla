package ui

import (
	"context"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/chipi/flotilla/internal/api"
	"github.com/chipi/flotilla/internal/cases"
	"github.com/chipi/flotilla/internal/spec"
	"github.com/chipi/flotilla/internal/store"
)

// A fleet's own knowledge on the Knowledge page (operator fleet plan, "A fleet brings its own knowledge"): one tab per
// collection an installed fleet declares in its fleet.yaml, grouped by fleet. The core reads the items as text and
// knows nothing of what they mean. A fleet that declares nothing adds no tab, and one that is not installed has none.

// knowTab is a Knowledge tab; a fleet's collections carry their fleet as the group.
type knowTab struct {
	pillLink
	Group string
	First bool // the first tab of its group: the fleet's name stands before it
}

// knowUse is where an item was read: the runs whose loaded records cite it.
type knowUse struct {
	Runs                      int
	LatestAt, Run, Case       string
	LatestWhen, RunHref, Href string
}

type knowRow struct {
	spec.KnowledgeItem
	Cells []string // the declared fields' text, in the declared order
	Short string
	Use   knowUse
	Href  string
}

// knowProposal is a fleet's proposal waiting for an answer.
type knowProposal struct {
	Case    store.Case
	P       api.Proposal
	Opened  string
	Evid    []string
	Back    string
	Heading string
}

type knowColl struct {
	Fleet, FleetTitle string
	spec.Knowledge
	Key       string // the tab's value: <fleet>.<collection>
	Rows      []knowRow
	Waiting   []knowProposal
	Heads     []string
	Back      string
	Proposing bool // the fleet's proposals wait on this tab (its first collection that takes them)
}

func collKey(fleet, name string) string { return fleet + "." + name }

// knowledgeColls is every collection of every installed fleet, by fleet then declaration order.
func (u *Server) knowledgeColls() []knowColl {
	names := make([]string, 0, len(u.Fleets))
	for n, f := range u.Fleets {
		if len(f.Knowledge) > 0 {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	var out []knowColl
	for _, n := range names {
		proposing := false
		for _, k := range u.Fleets[n].Knowledge {
			c := knowColl{Fleet: n, FleetTitle: title(n), Knowledge: k, Key: collKey(n, k.Name)}
			if k.Proposals && !proposing {
				c.Proposing, proposing = true, true
			}
			c.Back = "/knowledge?tab=" + url.QueryEscape(c.Key)
			out = append(out, c)
		}
	}
	return out
}

func (u *Server) knowledgeTabs(active string) []knowTab {
	var tabs []knowTab
	prev := ""
	for _, c := range u.knowledgeColls() {
		tabs = append(tabs, knowTab{pillLink: pillLink{Label: c.Title, Href: c.Back, On: active == c.Key}, Group: c.FleetTitle, First: prev != c.Fleet})
		prev = c.Fleet
	}
	return tabs
}

func (u *Server) knowledgeColl(key string) (knowColl, bool) {
	for _, c := range u.knowledgeColls() {
		if c.Key == key {
			return c, true
		}
	}
	return knowColl{}, false
}

// fill adds what the page shows of a collection: its items with their use, and the proposals waiting for a person.
func (u *Server) fill(ctx context.Context, c *knowColl) error {
	use, err := u.knowledgeUse(ctx, c.Fleet, c.Items)
	if err != nil {
		return err
	}
	c.Heads = c.Fields
	for _, it := range c.Items {
		row := knowRow{KnowledgeItem: it, Short: shortHash(it.Hash), Use: use[it.ID], Href: itemHref(c.Fleet, c.Name, it.ID)}
		for _, f := range c.Fields {
			row.Cells = append(row.Cells, it.Fields[f])
		}
		c.Rows = append(c.Rows, row)
	}
	if !c.Proposing {
		return nil
	}
	open, err := u.API.Store.Cases(ctx, store.CaseFilter{Kind: api.ProposalCase, Status: cases.HandedOver, Limit: 200})
	if err != nil {
		return err
	}
	for i := len(open) - 1; i >= 0; i-- { // the oldest first
		cs := open[i]
		p, ok, err := api.ProposalOf(ctx, u.API.Store, cs)
		if err != nil {
			return err
		}
		if !ok || p.Fleet != c.Fleet {
			continue
		}
		c.Waiting = append(c.Waiting, knowProposal{Case: cs, P: p, Opened: u.clock(cs.OpenedAt), Evid: p.Evidence, Back: c.Back,
			Heading: p.Mode + " " + p.Path})
	}
	return nil
}

func itemHref(fleet, coll, id string) string {
	return "/knowledge/" + url.PathEscape(fleet) + "/" + url.PathEscape(coll) + "/" + url.PathEscape(id)
}

// knowledgeUse counts, for each item, the runs of the fleet's routines whose loaded records cite it (by the page's sha256, or a
// skill's name in the fleet's own layer, or a playbook's file) and finds the latest, with the case it planned on.
func (u *Server) knowledgeUse(ctx context.Context, fleet string, items []spec.KnowledgeItem) (map[string]knowUse, error) {
	recs, err := u.API.Store.FleetLoaded(ctx, fleet, 5000)
	if err != nil {
		return nil, err
	}
	out := map[string]knowUse{}
	for _, it := range items {
		runs := map[string]bool{}
		var use knowUse
		for _, l := range recs { // newest first
			if !cites(l, it) || runs[l.RunID] {
				continue
			}
			runs[l.RunID] = true
			if use.Run == "" {
				use.Run, use.LatestAt = l.RunID, l.At
			}
		}
		use.Runs = len(runs)
		if use.Run != "" {
			use.LatestWhen, use.RunHref = u.clock(use.LatestAt), "/runs/"+use.Run
			if id, err := u.API.Store.PlannedCaseOfRun(ctx, use.Run); err == nil && id != "" {
				use.Case, use.Href = id, "/cases/"+id
			}
		}
		out[it.ID] = use
	}
	return out, nil
}

func cites(l store.Loaded, it spec.KnowledgeItem) bool {
	switch {
	case l.SHA256 != "" && l.SHA256 == it.Hash:
		return true
	case l.Kind == "skill" && l.Title == it.ID && strings.HasPrefix(l.Source, "fleet "):
		return true
	case l.Kind == "playbook" && strings.Contains(l.Source, it.File):
		return true
	}
	return false
}

// knowledgeItemPage is one item in full: its declared fields, its hash, where it was read and its text. The text is shown as
// text, escaped: no raw HTML from a file ever reaches the page.
func (u *Server) knowledgeItemPage(w http.ResponseWriter, r *http.Request, s *store.Session, sid string) {
	c, ok := u.knowledgeColl(collKey(r.PathValue("fleet"), r.PathValue("coll")))
	if !ok {
		http.NotFound(w, r)
		return
	}
	var it *spec.KnowledgeItem
	for i := range c.Items {
		if c.Items[i].ID == r.PathValue("id") {
			it = &c.Items[i]
		}
	}
	if it == nil {
		http.NotFound(w, r)
		return
	}
	use, err := u.knowledgeUse(r.Context(), c.Fleet, []spec.KnowledgeItem{*it})
	if err != nil {
		http.Error(w, "knowledge failed", 500)
		return
	}
	row := knowRow{KnowledgeItem: *it, Short: shortHash(it.Hash), Use: use[it.ID]}
	for _, f := range c.Fields {
		row.Cells = append(row.Cells, it.Fields[f])
	}
	d := struct {
		C   knowColl
		Row knowRow
	}{c, row}
	u.render(w, "knowledge_item.html", 200, u.pageFor(s, it.Title, "Knowledge", r, d))
}
