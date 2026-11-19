package ui

import (
	"net/http"
	"sort"
	"strings"

	"github.com/chipi/flotilla/internal/spec"
	"github.com/chipi/flotilla/internal/store"
)

// Examples: the examples catalogue (flotillad.yaml examples_dir), drawn read-only. It takes nothing from the store: an example is not a routine of
// this installation, so it has no runs, no cost, no stale flag and no routine page, and the page says so. The flow of each is the spec's own blocks
// (specBlocks), without counts.

type exampleRoutine struct {
	spec.Example
	Short string
	Lanes []rLane
}

type exampleFleet struct {
	ID, Name, Purpose string
	Routines          []exampleRoutine
}

type examplesData struct {
	Dir     string
	Fleets  []exampleFleet
	Total   int
	Present bool
}

func (u *Server) examplesPage(w http.ResponseWriter, r *http.Request, s *store.Session, sid string) {
	c := u.API.Examples
	d := examplesData{Present: c != nil, Dir: c.Dir()}
	by := map[string]*exampleFleet{}
	fleets := c.Fleets()
	for _, e := range c.Examples() {
		f := by[e.Fleet]
		if f == nil {
			fd := fleets[e.Fleet]
			f = &exampleFleet{ID: e.Fleet, Name: title(strings.TrimPrefix(e.Fleet, spec.ExamplePrefix)), Purpose: fd.Description}
			by[e.Fleet] = f
		}
		er := exampleRoutine{Example: e, Short: firstSentence(e.Description, 160)}
		if j := c.ForDisplay(e.ID); j != nil {
			er.Lanes = specBlocks(j, mixCounts{}, func(string) string { return "" })
		}
		f.Routines = append(f.Routines, er)
		d.Total++
	}
	for _, f := range by {
		d.Fleets = append(d.Fleets, *f)
	}
	sort.Slice(d.Fleets, func(i, j int) bool { return d.Fleets[i].ID < d.Fleets[j].ID })
	u.render(w, "examples.html", 200, u.pageFor(s, "Examples", "Fleets", r, d))
}
