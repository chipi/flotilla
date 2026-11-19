package spec

import (
	"fmt"
	"sort"
	"strings"
)

// The examples catalogue (v2 step 5). The image carries flotilla's example fleets twice: at /opt/flotilla/fleets, which an
// installation's own fleets replace, and at /opt/flotilla/examples, which stays. An installation that sets examples_dir in
// flotillad.yaml gets the second copy read as a catalogue: a read-only list for the status API and the UI, so an operator can see what
// a fleet of each kind looks like and copy one into its own fleets repo.
//
// A catalogue is not a set of routines. LoadExamples loads the folder on its own (its own validation, its own uniqueness checks, a
// namespace of its own) and returns a Catalogue, not []*Routine, so the code that takes routines (the scheduler, the version
// reconciler, the shadows, dispatch, the budgets, the brief, the model and worker checks, the metrics) cannot be handed it by accident:
// it does not compile, and TestNoRunningCodeTakesTheCatalogue lists the only places that read it. An example costs nothing by
// construction: no store row, no scheduler entry, no metric series, no worker call.

// ExamplePrefix is put before an example fleet's id, so an example never has the id of a routine the installation runs
// (example-triage/cycle beside its own triage/cycle).
const ExamplePrefix = "example-"

// Example is one routine of the catalogue, as the status API and the CLI list it.
type Example struct {
	ID          string           `json:"id"`
	Fleet       string           `json:"fleet"`
	Routine     string           `json:"routine"`
	Description string           `json:"description"`
	Worker      string           `json:"worker"`
	Trigger     string           `json:"trigger"`
	Detects     []string         `json:"detects,omitempty"`
	Chain       []string         `json:"chain,omitempty"`
	Versions    []ExampleVersion `json:"versions"`
}

// ExampleVersion is one version an example ships: its number, its frozen hash and the stations it declares.
type ExampleVersion struct {
	Number   int      `json:"number"`
	Hash     string   `json:"hash"`
	Stations []string `json:"stations,omitempty"`
	Models   []string `json:"models,omitempty"`
}

// Catalogue is the examples folder, loaded and read-only.
type Catalogue struct {
	dir      string
	fleets   map[string]Fleet
	routines []*Routine // ids and chain links carry ExamplePrefix; never handed out as routines
}

// LoadExamples loads an examples folder (the same layout as a fleets folder) as a catalogue. Every routine and version must load: the
// folder is the image's own copy of fleets/, so one that does not is a broken image.
func LoadExamples(dir string) (*Catalogue, error) {
	rs, err := Load(dir)
	if err != nil {
		return nil, fmt.Errorf("examples %s: %w", dir, err)
	}
	fleets, err := LoadFleets(dir, rs)
	if err != nil {
		return nil, fmt.Errorf("examples %s: %w", dir, err)
	}
	c := &Catalogue{dir: dir, fleets: map[string]Fleet{}}
	for name, f := range fleets {
		f.Fleet = ExamplePrefix + name
		c.fleets[f.Fleet] = f
	}
	for _, j := range rs {
		j.Fleet = ExamplePrefix + j.Fleet
		chain := make([]ChainLink, len(j.Chain))
		for i, l := range j.Chain {
			chain[i] = ChainLink{Routine: ExamplePrefix + l.Routine, Via: l.Via}
		}
		j.Chain = chain
		c.routines = append(c.routines, j)
	}
	sort.Slice(c.routines, func(a, b int) bool { return c.routines[a].ID() < c.routines[b].ID() })
	return c, nil
}

// Dir is the folder it was loaded from.
func (c *Catalogue) Dir() string {
	if c == nil {
		return ""
	}
	return c.dir
}

// Has reports whether id (example-<fleet>/<routine>) is an example of the catalogue.
func (c *Catalogue) Has(id string) bool {
	if c == nil {
		return false
	}
	for _, j := range c.routines {
		if j.ID() == id {
			return true
		}
	}
	return false
}

// Fleets are the catalogue's fleets, keyed by their prefixed id.
func (c *Catalogue) Fleets() map[string]Fleet {
	if c == nil {
		return nil
	}
	out := make(map[string]Fleet, len(c.fleets))
	for k, v := range c.fleets {
		out[k] = v
	}
	return out
}

// Examples lists every routine of the catalogue, by id.
func (c *Catalogue) Examples() []Example {
	if c == nil {
		return nil
	}
	out := make([]Example, 0, len(c.routines))
	for _, j := range c.routines {
		e := Example{ID: j.ID(), Fleet: j.Fleet, Routine: j.Routine, Description: strings.Join(strings.Fields(j.Description), " "), Worker: j.Worker,
			Trigger: TriggerText(j), Detects: j.Detects}
		for _, l := range j.Chain {
			e.Chain = append(e.Chain, l.Routine)
		}
		for _, v := range j.Versions {
			ev := ExampleVersion{Number: v.Number, Hash: v.Hash}
			if vj, err := j.WithVersion(v.Number); err == nil {
				ev.Stations, ev.Models = vj.StationNames(), vj.Models
			}
			e.Versions = append(e.Versions, ev)
		}
		out = append(out, e)
	}
	return out
}

// ForDisplay is a copy of the example's routine for the page that draws its flow (the explain blocks). Display only: it is a routine that
// is not one, with the catalogue's prefixed ids, and nothing that runs routines may be given it.
func (c *Catalogue) ForDisplay(id string) *Routine {
	if c == nil {
		return nil
	}
	for _, j := range c.routines {
		if j.ID() == id {
			cp := *j
			return &cp
		}
	}
	return nil
}

// TriggerText says in words what starts a routine.
func TriggerText(j *Routine) string {
	switch {
	case j.Trigger.Case != nil:
		return "case handed over: " + j.Trigger.Case.Kind
	case j.Trigger.Check != nil:
		return "case check due: " + j.Trigger.Check.Verify
	case j.Trigger.Execute != nil:
		return "plan approved: " + j.Trigger.Execute.Action
	case j.Trigger.Every != "":
		return "every " + j.Trigger.Every
	}
	return "by hand"
}
