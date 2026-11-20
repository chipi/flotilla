package main

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/chipi/flotilla/internal/spec"
	"github.com/chipi/flotilla/internal/store"
	"github.com/chipi/flotilla/internal/versions"
)

// The routines' own fields (budget, trigger, worker, autonomy and the rest of routine.yaml) are in no version's hash, so a deploy changes them
// freely. flotillad keeps a snapshot of them in the store as a record: the start writes it and a moved field is a routine.update event; -check says
// the same before the deploy. Nothing is refused for a field.

// routineFieldsNote closes the line about a moved routine: the comparison informs the operator's go, it decides nothing.
const routineFieldsNote = "(information only, not a gate: the freeze covers the version's keys and files, and the start only records this)"

// fieldsOf is each routine's own fields by id.
func fieldsOf(routines []*spec.Routine) map[string]map[string]string {
	out := map[string]map[string]string{}
	for _, j := range routines {
		out[j.ID()] = j.RoutineFields()
	}
	return out
}

// recordRoutineFields is the start's write of the snapshots and its log of what moved.
func recordRoutineFields(ctx context.Context, st *store.Store, log *slog.Logger, routines []*spec.Routine) error {
	changes, err := st.RecordRoutineFields(ctx, fieldsOf(routines), versions.Actor)
	if err != nil {
		return err
	}
	first := 0
	for _, c := range changes {
		if c.First {
			first++
			continue
		}
		log.Info("routine fields changed", "routine", c.Routine, "changes", strings.Join(c.Changes, "; "))
	}
	if first > 0 {
		log.Info("routine fields recorded for the first time", "routines", first)
	}
	return nil
}

// checkRoutineFields is -check's read of the snapshots: read-only, like checkVersions.
func checkRoutineFields(stateDir string, routines []*spec.Routine, plan versions.Plan) ([]string, error) {
	read, err := store.ReadRoutineFields(filepath.Join(stateDir, "flotilla.db"))
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	return routineFieldLines(read, routines, plan), nil
}

// routineFieldLines is what -check says about the routines' own fields against the stored snapshots, from the diff of each routine's loaded
// fields: a line per field that moved ("routine R: budget {"day_usd":1} -> {"day_usd":2}"), one line that there is no snapshot yet when the store
// has none (the first start after this deploy records them), and for each flat v1 moved to the folder layout a line that its fields are identical to
// the snapshot or what moved. The first lines are the deploy's "routine fields changed" block; the moved ones are information beside the proof.
func routineFieldLines(read store.FieldsRead, routines []*spec.Routine, plan versions.Plan) []string {
	diffs := map[string][]string{}
	var out []string
	for _, j := range routines {
		old, ok := read.Fields[j.ID()]
		if !ok {
			continue
		}
		if d := spec.DiffFields(old, j.RoutineFields()); len(d) > 0 {
			diffs[j.ID()] = d
			for _, l := range d {
				out = append(out, fmt.Sprintf("routine %s: %s", j.ID(), l))
			}
		}
	}
	if len(read.Fields) == 0 {
		out = append(out, "routine fields: no snapshot yet (the first start after this deploy records one for every routine); nothing to compare")
	}
	for _, u := range plan.Updates {
		if len(u.Moved) == 0 {
			continue
		}
		prefix := fmt.Sprintf("versions: %s v%d routine fields:", u.Routine, u.Version)
		switch _, ok := read.Fields[u.Routine]; {
		case !ok:
			out = append(out, prefix+" no snapshot yet to compare with "+routineFieldsNote)
		case len(diffs[u.Routine]) == 0:
			out = append(out, prefix+" identical to the stored snapshot "+routineFieldsNote)
		default:
			out = append(out, prefix+" changed since the stored snapshot: "+strings.Join(diffs[u.Routine], "; ")+" "+routineFieldsNote)
		}
	}
	return out
}
