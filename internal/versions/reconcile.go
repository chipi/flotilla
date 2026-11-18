// Package versions is what flotillad does with routine versions when it starts, and what -check says
// before a deploy: it compares the versions the fleets folder holds with the ones the store recorded,
// refuses a live or previous version whose folder changed (they are frozen, so rollback is safe), records
// the new ones (the first version of a routine goes live, any other arrives as a shadow), and binds each
// routine to its live version. docs/wip/v2-design.md section 1.
package versions

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/chipi/flotilla/internal/spec"
	"github.com/chipi/flotilla/internal/store"
)

// Actor is who the store records for what a start did (never a person).
const Actor = "flotillad"

// Plan is what comparing the loaded versions with the recorded ones decided, before anything is written.
type Plan struct {
	// Ensure is, per routine, the valid loaded versions to record: the ones not recorded yet and the
	// recorded shadows (whose hash follows their folder). Retired, previous and live ones need no write.
	Ensure map[string][]store.LoadedVersion
	// Live is the version each routine runs: the recorded live one, or for a routine with none recorded
	// the lowest loaded (it goes live at this deploy).
	Live map[string]int
	// states is each loaded version's state as it will be, with "(new)" on one this start records.
	states map[string]map[int]string
	// Updates are a flat spec's live or previous v1 whose hash moved: not frozen, so each is re-recorded at the start
	// (an event, a line in -check and in the deploy).
	Updates []Update
	// Notes are problems the freeze tolerates: a retired version whose folder is gone or does not load,
	// a recorded routine with no spec.
	Notes []string
}

// States is the routine's loaded versions as "v1:live,v2:shadow(new)", for -check.
func (p Plan) States(routine string) string {
	m := p.states[routine]
	nums := make([]int, 0, len(m))
	for n := range m {
		nums = append(nums, n)
	}
	sort.Ints(nums)
	parts := make([]string, len(nums))
	for i, n := range nums {
		parts[i] = fmt.Sprintf("v%d:%s", n, m[n])
	}
	return strings.Join(parts, ",")
}

func short(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

// Check compares what is loaded with what is recorded. It returns a Plan, and an error naming every
// problem that must stop a start or a deploy, one line each, "versions: <routine> v<n> ...":
//   - a live or previous version whose folder is missing (deleted or renumbered), does not load, or has
//     a different hash than the one recorded when it went live (the files that differ are named, and the
//     way out: a new number, or `flotilla versions rerecord`). A flat spec's v1 is the exception: it is
//     not frozen, its changed hash is a plan Update (re-recorded at the start);
//   - a shadow whose folder is missing or does not load;
//   - a version not recorded yet that does not load.
//
// A retired version may be missing or invalid: it only keeps its history.
func Check(routines []*spec.Routine, stored []store.RoutineVersion) (Plan, error) {
	p := Plan{Ensure: map[string][]store.LoadedVersion{}, Live: map[string]int{}, states: map[string]map[int]string{}}
	byRoutine := map[string]map[int]store.RoutineVersion{}
	for _, r := range stored {
		if byRoutine[r.Routine] == nil {
			byRoutine[r.Routine] = map[int]store.RoutineVersion{}
		}
		byRoutine[r.Routine][r.Version] = r
	}
	var errs []error
	bad := func(f string, a ...any) { errs = append(errs, fmt.Errorf("versions: "+f, a...)) }
	known := map[string]bool{}
	for _, j := range routines {
		id := j.ID()
		known[id] = true
		rows := byRoutine[id]
		p.states[id] = map[int]string{}
		loaded := map[int]*spec.Version{}
		for _, v := range j.Versions {
			loaded[v.Number] = v
		}
		nums := make([]int, 0, len(rows))
		for n := range rows {
			nums = append(nums, n)
		}
		sort.Ints(nums)
		for _, n := range nums {
			row, v := rows[n], loaded[n]
			switch row.State {
			case store.VersionRetired:
				if v == nil {
					p.Notes = append(p.Notes, fmt.Sprintf("%s v%d is retired and its folder is gone", id, n))
				} else if !v.Valid() {
					p.Notes = append(p.Notes, fmt.Sprintf("%s v%d is retired and does not load: %v", id, n, v.Err))
				}
				if v != nil && v.Valid() {
					p.states[id][n] = store.VersionRetired
				}
			case store.VersionLive, store.VersionPrevious:
				switch {
				case v == nil:
					bad("%s v%d is %s and its folder is missing (deleted or renumbered): restore v%d/ as it was; a live or previous version never changes", id, n, row.State, n)
				case !v.Valid():
					bad("%s v%d is %s and does not load: %v: restore it as it was, or fix it and `flotilla versions rerecord %s v%d --reason R` if the change is deliberate", id, n, row.State, v.Err, id, n)
				case v.Hash != row.Hash && v.Legacy && store.RecordedLegacy(row):
					// a flat spec's v1 is not frozen: the change is recorded, with an event and a line in -check and the deploy
					p.Updates = append(p.Updates, Update{Routine: id, Version: n, OldHash: row.Hash, NewHash: v.Hash, Changed: spec.ChangedFiles(row.Manifest, v.Manifest)})
					p.Ensure[id] = append(p.Ensure[id], lv(v))
					p.states[id][n] = row.State
				case v.Hash != row.Hash:
					bad("%s v%d is %s and its folder changed (recorded %s, found %s): %s; ship the change as v%d (a shadow or retired folder may change), or `flotilla versions rerecord %s v%d --reason R` if this change is deliberate",
						id, n, row.State, short(row.Hash), short(v.Hash), strings.Join(spec.ChangedFiles(row.Manifest, v.Manifest), ", "), nextNumber(rows, j), id, n)
				default:
					p.states[id][n] = row.State
				}
			case store.VersionShadow:
				switch {
				case v == nil:
					bad("%s v%d is a shadow and its folder is missing: retire it first (`flotilla retire %s v%d --reason R`) or restore the folder", id, n, id, n)
				case !v.Valid():
					bad("%s v%d is a shadow and does not load: %v: fix it, or retire it (`flotilla retire %s v%d --reason R`)", id, n, v.Err, id, n)
				default:
					p.states[id][n] = store.VersionShadow
					p.Ensure[id] = append(p.Ensure[id], lv(v))
				}
			}
			if row.State == store.VersionLive {
				p.Live[id] = n
			}
		}
		var fresh []*spec.Version
		for _, v := range j.Versions {
			if _, ok := rows[v.Number]; ok {
				continue
			}
			if !v.Valid() {
				bad("%s v%d is not recorded yet and does not load: %v: fix it, or take v%d/ out of the fleets folder", id, v.Number, v.Err, v.Number)
				continue
			}
			fresh = append(fresh, v)
		}
		for i, v := range fresh {
			state := store.VersionShadow
			if len(rows) == 0 && i == 0 {
				state = store.VersionLive // the first version of a routine goes live at deploy
				p.Live[id] = v.Number
			}
			p.states[id][v.Number] = state + "(new)"
			p.Ensure[id] = append(p.Ensure[id], lv(v))
		}
	}
	var orphans []string
	for id := range byRoutine {
		if !known[id] {
			orphans = append(orphans, id)
		}
	}
	sort.Strings(orphans)
	for _, id := range orphans {
		p.Notes = append(p.Notes, fmt.Sprintf("%s has recorded versions and no spec", id))
	}
	return p, errors.Join(errs...)
}

func lv(v *spec.Version) store.LoadedVersion {
	return store.LoadedVersion{Version: v.Number, Hash: v.Hash, Manifest: v.Manifest, Legacy: v.Legacy}
}

// Reconcile is flotillad's start: Check against the store, record what is new, and return the routines
// each bound to the version that is live. It fails, writing nothing, when Check does.
func Reconcile(ctx context.Context, st *store.Store, routines []*spec.Routine) ([]*spec.Routine, Plan, error) {
	stored, err := st.AllVersions(ctx)
	if err != nil {
		return nil, Plan{}, err
	}
	plan, err := Check(routines, stored)
	if err != nil {
		return nil, plan, err
	}
	for _, j := range routines {
		if _, err := st.EnsureVersions(ctx, j.ID(), plan.Ensure[j.ID()], Actor); err != nil {
			return nil, plan, fmt.Errorf("versions: %s: %w", j.ID(), err)
		}
	}
	live, err := st.AllVersions(ctx)
	if err != nil {
		return nil, plan, err
	}
	liveOf := map[string]int{}
	for _, r := range live {
		if r.State == store.VersionLive {
			liveOf[r.Routine] = r.Version
		}
	}
	out := make([]*spec.Routine, 0, len(routines))
	for _, j := range routines {
		n, ok := liveOf[j.ID()]
		if !ok {
			return nil, plan, fmt.Errorf("versions: %s has no live version recorded", j.ID())
		}
		b, err := j.WithVersion(n)
		if err != nil {
			return nil, plan, fmt.Errorf("versions: %w", err)
		}
		out = append(out, b)
	}
	return out, plan, nil
}

// Bound is each routine bound to the version -check would run it at (the live one recorded, or the one that
// goes live at the deploy): a routine with none loadable is left as loaded.
func Bound(routines []*spec.Routine, p Plan) []*spec.Routine {
	out := make([]*spec.Routine, len(routines))
	for i, j := range routines {
		out[i] = j
		if n, ok := p.Live[j.ID()]; ok {
			if b, err := j.WithVersion(n); err == nil {
				out[i] = b
			}
		}
	}
	return out
}

// Each is every valid version of every routine, each as the routine bound to it: what the skills and
// prompts checks must cover, since any version that is not retired can run.
func Each(routines []*spec.Routine) []*spec.Routine {
	var out []*spec.Routine
	for _, j := range routines {
		for _, v := range j.Versions {
			if v.Valid() {
				if b, err := j.WithVersion(v.Number); err == nil {
					out = append(out, b)
				}
			}
		}
	}
	return out
}

// Runnable are the loaded versions that can run, as their own bound copies: the live, previous and shadow
// ones, as the plan has them. Retired versions only keep history and are not in it.
func (p Plan) Runnable(routines []*spec.Routine) []*spec.Routine { return p.bound(routines, false) }

// RetiredLoaded are the retired versions that still load, as bound copies: -check reports what is wrong
// with their skills and prompts as a note, never as a refusal.
func (p Plan) RetiredLoaded(routines []*spec.Routine) []*spec.Routine { return p.bound(routines, true) }

func (p Plan) bound(routines []*spec.Routine, retired bool) []*spec.Routine {
	var out []*spec.Routine
	for _, j := range routines {
		for _, v := range j.Versions {
			st, ok := p.states[j.ID()][v.Number]
			if !ok || (strings.TrimSuffix(st, "(new)") == store.VersionRetired) != retired {
				continue
			}
			if b, err := j.WithVersion(v.Number); err == nil {
				out = append(out, b)
			}
		}
	}
	return out
}

// Update is a legacy (flat) v1 whose recorded hash moved: what flotillad re-records instead of refusing.
type Update struct {
	Routine          string
	Version          int
	OldHash, NewHash string
	Changed          []string // spec.ChangedFiles: "prompt-file:playbooks/triage.md (changed)"
}

// Line is how -check and the deploy say it: "triage/cycle v1 (flat) re-recorded: prompt-file:playbooks/triage.md (changed)".
func (u Update) Line() string {
	return fmt.Sprintf("%s v%d (flat) re-recorded: %s", u.Routine, u.Version, strings.Join(u.Changed, ", "))
}

// nextNumber is the number a new version of the routine would take: one above the highest recorded or loaded.
func nextNumber(rows map[int]store.RoutineVersion, j *spec.Routine) int {
	max := 0
	for n := range rows {
		if n > max {
			max = n
		}
	}
	for _, v := range j.Versions {
		if v.Number > max {
			max = v.Number
		}
	}
	return max + 1
}
