package spec

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// The routine page draws a routine from its spec (the UI plan's step 3): one block per part of the spec,
// the steps its code takes that the spec does not show, and the routines that come after it. Each block
// is explained by the spec itself, so the explanation is reviewed in git with the routine it describes.

// Step is something the routine's code does that no other spec field shows, drawn in its lane.
type Step struct {
	ID      string `yaml:"id" json:"id"`
	Lane    string `yaml:"lane" json:"lane"`   // when, where, sort, agent, gates, outcome
	Who     string `yaml:"who" json:"who"`     // code, agent, tool, you: who acts in this step
	Title   string `yaml:"title" json:"title"` // the block's name
	Short   string `yaml:"short" json:"short"` // one line under the name
	Explain string `yaml:"explain" json:"explain"`
	Count   string `yaml:"count" json:"count"` // what the store counts for it, see Counts
}

// ChainLink is a routine that comes after this one; Via says what carries the work there (default: the
// next routine's trigger).
type ChainLink struct {
	Routine string `yaml:"routine" json:"routine"`
	Via     string `yaml:"via" json:"via"`
}

var (
	Lanes = []string{"when", "where", "sort", "agent", "gates", "outcome"}
	Whos  = []string{"code", "agent", "tool", "you"}
	// Counts the routine page can show on a block, from the store: its runs, the ledger's recurrences and
	// decisions, decisions by disposition, a gate's firings, and the probes used per decision.
	countRe   = regexp.MustCompile(`^(runs|recurrences|decided|probes|disposition:[a-z]+|gate:[a-z_]+)$`)
	stepIDRe  = regexp.MustCompile(`^[a-z][a-z0-9_]{0,39}$`)
	routineRe = regexp.MustCompile(`^[a-z0-9-]+/[a-z0-9-]+$`)
)

const explainMax = 600

// ExplainKeys are the keys explain may use: the spec parts the routine page draws (only those this spec
// sets), a gate.<name> per harness gate, and each step's id.
func (j *Routine) ExplainKeys() []string {
	keys := []string{"trigger", "worker", "run", "autonomy", "budget"}
	if j.Freshness != "" {
		keys = append(keys, "freshness")
	}
	if len(j.Skills.Rule) > 0 {
		keys = append(keys, "skills")
	}
	if len(j.Prompts) > 0 {
		keys = append(keys, "prompts")
	}
	if len(j.Models) > 0 {
		keys = append(keys, "models")
	}
	if len(j.Detects) > 0 {
		keys = append(keys, "detects")
	}
	if h := j.Harness; h != nil {
		keys = append(keys, "harness.protocol", "harness.limits", "harness.tools", "harness.quirks")
		if h.Hooks != nil {
			keys = append(keys, "harness.hooks")
		}
		for _, g := range h.Gates {
			keys = append(keys, "gate."+g)
		}
	}
	// a station version: a text per station, per gate its stations run (each once) and for the texts its stations read
	seen := map[string]bool{}
	for _, n := range j.StationNames() {
		keys = append(keys, "station."+n)
		if g := j.Stations[n].Gates; g != nil {
			for _, name := range *g {
				if !seen[name] {
					seen[name], keys = true, append(keys, "gate."+name)
				}
			}
		}
	}
	if len(j.StationText) > 0 && len(j.Prompts) == 0 {
		keys = append(keys, "prompts")
	}
	for _, s := range j.Steps {
		keys = append(keys, s.ID)
	}
	return keys
}

func in(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

func (j *Routine) validateExplain(bad func(string, ...any)) {
	seen := map[string]bool{}
	for _, s := range j.Steps {
		switch {
		case !stepIDRe.MatchString(s.ID):
			bad("steps: id %q (lower-case, digits, _)", s.ID)
		case seen[s.ID]:
			bad("steps: id %q named twice", s.ID)
		case in([]string{"trigger", "worker", "run", "autonomy", "budget", "freshness", "skills", "prompts", "models", "detects"}, s.ID):
			bad("steps: id %q is a spec part's own key", s.ID)
		}
		seen[s.ID] = true
		if !in(Lanes, s.Lane) {
			bad("steps[%s].lane %q: one of %s", s.ID, s.Lane, strings.Join(Lanes, ", "))
		}
		if !in(Whos, s.Who) {
			bad("steps[%s].who %q: one of %s", s.ID, s.Who, strings.Join(Whos, ", "))
		}
		if strings.TrimSpace(s.Title) == "" || strings.TrimSpace(s.Explain) == "" {
			bad("steps[%s]: a title and an explain are required", s.ID)
		}
		if len(s.Explain) > explainMax || len(s.Short) > 120 {
			bad("steps[%s]: explain at most %d characters, short at most 120", s.ID, explainMax)
		}
		if s.Count != "" && !countRe.MatchString(s.Count) {
			bad("steps[%s].count %q: runs, recurrences, decided, probes, disposition:<name> or gate:<name>", s.ID, s.Count)
		}
		if strings.HasPrefix(s.Count, "gate:") && !in(j.GateNames(), strings.TrimPrefix(s.Count, "gate:")) {
			bad("steps[%s].count %q: not one of the harness's gates", s.ID, s.Count)
		}
	}
	keys := j.ExplainKeys()
	names := make([]string, 0, len(j.Explain))
	for k := range j.Explain {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		v := strings.TrimSpace(j.Explain[k])
		if !in(keys, k) {
			bad("explain: %q explains no block of this spec (one of %s)", k, strings.Join(keys, ", "))
		}
		if v == "" || len(v) > explainMax {
			bad("explain.%s: a text of 1 to %d characters", k, explainMax)
		}
	}
	for i, c := range j.Chain {
		if !routineRe.MatchString(c.Routine) {
			bad("chain[%d].routine %q: a fleet/routine id", i, c.Routine)
		}
		if c.Routine == j.ID() {
			bad("chain[%d]: a routine does not follow itself", i)
		}
		if len(c.Via) > 120 {
			bad("chain[%d].via: at most 120 characters", i)
		}
	}
}

// checkChains: every chain link names a routine that exists (Load's cross-spec check).
func checkChains(routines []*Routine) []error {
	ids := map[string]bool{}
	for _, j := range routines {
		ids[j.ID()] = true
	}
	var errs []error
	for _, j := range routines {
		for i, c := range j.Chain {
			if routineRe.MatchString(c.Routine) && !ids[c.Routine] {
				errs = append(errs, fmt.Errorf("%s: chain[%d].routine %s does not exist", j.Path, i, c.Routine))
			}
		}
	}
	return errs
}

// GateNames are the gates the version runs: its harness block's, or its stations' (each once, station order).
func (j *Routine) GateNames() []string {
	var out []string
	if j.Harness != nil {
		out = append(out, j.Harness.Gates...)
	}
	for _, n := range j.StationNames() {
		if g := j.Stations[n].Gates; g != nil {
			for _, name := range *g {
				if !in(out, name) {
					out = append(out, name)
				}
			}
		}
	}
	return out
}
