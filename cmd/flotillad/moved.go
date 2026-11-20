package main

import (
	"fmt"
	"strings"

	"github.com/chipi/flotilla/internal/spec"
	"github.com/chipi/flotilla/internal/versions"
)

// movedFromNote closes every -moved-from line: the comparison informs the operator's go, it decides nothing.
const movedFromNote = "(information only, not a gate: the freeze covers the version's keys and files, and the start does not use this)"

// movedFromLines is -check -moved-from's report, one line per flat v1 that moves to the folder layout in this plan: the flat routine in dir (the
// running fleets) set against the folder routine, field by field, with the same comparison the move-equivalence test makes
// (spec.MovedView, spec.DifferingFields). The routine's own fields (budget, trigger, worker, autonomy) are not frozen for any routine,
// so a difference here is told, never refused.
func movedFromLines(dir string, routines []*spec.Routine, plan versions.Plan) []string {
	var moved []versions.Update
	for _, u := range plan.Updates {
		if len(u.Moved) > 0 {
			moved = append(moved, u)
		}
	}
	if len(moved) == 0 {
		return nil
	}
	old := map[string]*spec.Routine{}
	rs, err := spec.LoadVersions(dir)
	if err == nil {
		for _, j := range rs {
			old[j.ID()] = j
		}
	}
	now := map[string]*spec.Routine{}
	for _, j := range routines {
		now[j.ID()] = j
	}
	var out []string
	for _, u := range moved {
		line := func(f string, a ...any) {
			out = append(out, fmt.Sprintf("versions: %s v%d routine fields %s %s", u.Routine, u.Version, fmt.Sprintf(f, a...), movedFromNote))
		}
		switch o, n := old[u.Routine], now[u.Routine]; {
		case err != nil:
			line("not compared: cannot load %s: %v", dir, err)
		case o == nil || n == nil || o.VersionNamed(1) == nil || !o.VersionNamed(1).Legacy:
			line("not compared: no flat routine %s in %s", u.Routine, dir)
		default:
			a, aerr := spec.MovedView(o, 1)
			b, berr := spec.MovedView(n, u.Version)
			if aerr != nil || berr != nil {
				line("not compared: %v %v", aerr, berr)
				continue
			}
			if diff := spec.DifferingFields(b, a); len(diff) > 0 {
				line("differ from the flat spec: %s", strings.Join(diff, "; "))
			} else {
				line("identical to the flat spec")
			}
		}
	}
	return out
}
