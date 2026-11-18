package versions

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chipi/flotilla/internal/spec"
	"github.com/chipi/flotilla/internal/store"
)

const routineYAML = `fleet: triage
routine: cycle
description: one pass
enabled: true
worker: triage
trigger: {every: 10m}
autonomy: record
budget: {day_usd: 2}
freshness: 35m
`

func versionYAML(cmd string) string {
	return "run: {command: \"" + cmd + "\", workdir: signal-fleet/mvp, timeout: 20m}\nprompts:\n  system: system.md\n"
}

func write(t *testing.T, dir, rel, body string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// a fleets folder with the routine triage/cycle holding the given versions, each with a command
func fleets(t *testing.T, cmds map[string]string) string {
	t.Helper()
	d := t.TempDir()
	write(t, d, "triage/cycle/routine.yaml", routineYAML)
	for v, cmd := range cmds {
		write(t, d, "triage/cycle/"+v+"/spec.yaml", versionYAML(cmd))
		write(t, d, "triage/cycle/"+v+"/system.md", "text of "+v+"\n")
	}
	return d
}

func load(t *testing.T, d string) []*spec.Routine {
	t.Helper()
	rs, err := spec.LoadVersions(d)
	if err != nil {
		t.Fatal(err)
	}
	return rs
}

func open(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "f.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func rows(t *testing.T, s *store.Store) string {
	t.Helper()
	vs, err := s.AllVersions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, v := range vs {
		out = append(out, v.Routine+" v"+string(rune('0'+v.Version))+"="+v.State)
	}
	return strings.Join(out, ", ")
}

func hashOf(t *testing.T, s *store.Store, n int) string {
	t.Helper()
	vs, _ := s.Versions(context.Background(), "triage/cycle")
	for _, v := range vs {
		if v.Version == n {
			return v.Hash
		}
	}
	t.Fatalf("no v%d", n)
	return ""
}

func TestFreshStoreFirstVersionLiveOthersShadow(t *testing.T) {
	s := open(t)
	rs := load(t, fleets(t, map[string]string{"v1": "python3 a.py", "v2": "python3 b.py"}))
	bound, plan, err := Reconcile(context.Background(), s, rs)
	if err != nil {
		t.Fatal(err)
	}
	if got := rows(t, s); got != "triage/cycle v1=live, triage/cycle v2=shadow" {
		t.Fatalf("rows %s", got)
	}
	if len(bound) != 1 || bound[0].Version != 1 || bound[0].Run.Command != "python3 a.py" {
		t.Fatalf("bound %+v", bound[0])
	}
	if plan.States("triage/cycle") != "v1:live(new),v2:shadow(new)" || plan.Live["triage/cycle"] != 1 {
		t.Fatalf("plan %s %v", plan.States("triage/cycle"), plan.Live)
	}
}

func TestRestartAgainstTheSameFolders(t *testing.T) {
	s := open(t)
	d := fleets(t, map[string]string{"v1": "python3 a.py", "v2": "python3 b.py"})
	if _, _, err := Reconcile(context.Background(), s, load(t, d)); err != nil {
		t.Fatal(err)
	}
	bound, plan, err := Reconcile(context.Background(), s, load(t, d))
	if err != nil {
		t.Fatal(err)
	}
	if bound[0].Version != 1 || plan.States("triage/cycle") != "v1:live,v2:shadow" {
		t.Fatalf("restart: %s", plan.States("triage/cycle"))
	}
	ev, _ := s.Events(context.Background(), 100)
	n := 0
	for _, e := range ev {
		if strings.HasPrefix(e.Kind, "version.") {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("%d version events after two starts, want the 2 of the first", n)
	}
}

func renumber(t *testing.T, d, from, to string) {
	t.Helper()
	if err := os.Rename(filepath.Join(d, "triage/cycle", from), filepath.Join(d, "triage/cycle", to)); err != nil {
		t.Fatal(err)
	}
}

func breakSpec(t *testing.T, d, v string) {
	t.Helper()
	write(t, d, "triage/cycle/"+v+"/spec.yaml", "run: {command: x, timeout: 1s}\n")
}

// The freeze: each way a live or previous folder can differ from what was recorded fails the start, and the
// message names the routine, the version, the state and what changed. Nothing is written on a failure.
// "fresh": v1 live, v2 and v3 shadows. "promoted": v2 promoted, so v1 previous, v2 live, v3 shadow.
func TestFreezeRefusals(t *testing.T) {
	ctx := context.Background()
	for name, c := range map[string]struct {
		promoted bool
		edit     func(t *testing.T, d string)
		want     string
	}{
		"a live folder's text changed": {false, func(t *testing.T, d string) { write(t, d, "triage/cycle/v1/system.md", "edited\n") },
			"versions: triage/cycle v1 is live and its folder changed (recorded "},
		"the changed file is named": {false, func(t *testing.T, d string) { write(t, d, "triage/cycle/v1/system.md", "edited\n") },
			"): system.md (changed)"},
		"a live folder's command": {false, func(t *testing.T, d string) {
			write(t, d, "triage/cycle/v1/spec.yaml", versionYAML("python3 other.py"))
		},
			"): spec.yaml (changed)"},
		"a file added to a live folder": {false, func(t *testing.T, d string) { write(t, d, "triage/cycle/v1/extra.md", "x") },
			"): extra.md (added)"},
		"a live folder deleted": {false, func(t *testing.T, d string) { _ = os.RemoveAll(filepath.Join(d, "triage/cycle/v1")) },
			"triage/cycle v1 is live and its folder is missing"},
		"a live folder renumbered": {false, func(t *testing.T, d string) { renumber(t, d, "v1", "v5") },
			"triage/cycle v1 is live and its folder is missing"},
		"a live folder that stops loading": {false, func(t *testing.T, d string) { breakSpec(t, d, "v1") },
			"triage/cycle v1 is live and does not load"},
		"a shadow that stops loading": {false, func(t *testing.T, d string) { breakSpec(t, d, "v3") },
			"triage/cycle v3 is a shadow and does not load"},
		"a shadow folder gone": {false, func(t *testing.T, d string) { _ = os.RemoveAll(filepath.Join(d, "triage/cycle/v3")) },
			"triage/cycle v3 is a shadow and its folder is missing: retire it first"},
		"a new version that does not load": {false, func(t *testing.T, d string) { breakSpec(t, d, "v4") },
			"triage/cycle v4 is not recorded yet and does not load"},
		"a previous folder changed": {true, func(t *testing.T, d string) { write(t, d, "triage/cycle/v1/system.md", "edited\n") },
			"triage/cycle v1 is previous and its folder changed"},
		"a previous folder deleted": {true, func(t *testing.T, d string) { _ = os.RemoveAll(filepath.Join(d, "triage/cycle/v1")) },
			"triage/cycle v1 is previous and its folder is missing"},
		"a previous folder renumbered": {true, func(t *testing.T, d string) { renumber(t, d, "v1", "v7") },
			"triage/cycle v1 is previous and its folder is missing"},
		"the live folder after a promote": {true, func(t *testing.T, d string) { write(t, d, "triage/cycle/v2/system.md", "edited\n") },
			"triage/cycle v2 is live and its folder changed"},
	} {
		t.Run(name, func(t *testing.T) {
			s := open(t)
			d := fleets(t, map[string]string{"v1": "python3 a.py", "v2": "python3 b.py", "v3": "python3 c.py"})
			if _, _, err := Reconcile(ctx, s, load(t, d)); err != nil {
				t.Fatal(err)
			}
			if c.promoted {
				if err := s.Promote(ctx, store.PromoteArgs{Routine: "triage/cycle", Version: 2, Hash: hashOf(t, s, 2), Autonomy: "record", Current: "record",
					Actor: "user:ada", Via: "cli", Reason: "x"}); err != nil {
					t.Fatal(err)
				}
			}
			before := rows(t, s)
			c.edit(t, d)
			rs, err := spec.LoadVersions(d)
			if err != nil {
				t.Fatalf("setup: the edited tree must still load: %v", err)
			}
			if _, _, err = Reconcile(ctx, s, rs); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want an error with %q, got %v", c.want, err)
			}
			if rows(t, s) != before {
				t.Fatalf("a failed start wrote: %s -> %s", before, rows(t, s))
			}
		})
	}
}

// A retired version keeps its history and nothing else: its folder may be deleted or broken, and the start goes
// on, saying so. A shadow may change: its recorded hash follows it.
func TestRetiredTolerated(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	d := fleets(t, map[string]string{"v1": "python3 a.py", "v2": "python3 b.py", "v3": "python3 c.py"})
	if _, _, err := Reconcile(ctx, s, load(t, d)); err != nil {
		t.Fatal(err)
	}
	if err := s.Retire(ctx, "triage/cycle", 3, "user:ada", "cli", "not needed"); err != nil {
		t.Fatal(err)
	}
	_ = os.RemoveAll(filepath.Join(d, "triage/cycle/v3"))
	_, plan, err := Reconcile(ctx, s, load(t, d))
	if err != nil || len(plan.Notes) != 1 || !strings.Contains(plan.Notes[0], "triage/cycle v3 is retired and its folder is gone") {
		t.Fatalf("a retired folder gone: %v %v", err, plan.Notes)
	}
	write(t, d, "triage/cycle/v3/spec.yaml", "run: {command: x, timeout: 1s}\n")
	_, plan, err = Reconcile(ctx, s, load(t, d))
	if err != nil || len(plan.Notes) != 1 || !strings.Contains(plan.Notes[0], "retired and does not load") {
		t.Fatalf("a retired folder broken: %v %v", err, plan.Notes)
	}
	write(t, d, "triage/cycle/v3/spec.yaml", versionYAML("python3 changed.py"))
	write(t, d, "triage/cycle/v3/system.md", "x")
	if _, plan, err = Reconcile(ctx, s, load(t, d)); err != nil || len(plan.Notes) != 0 {
		t.Fatalf("a retired folder edited: %v %v", err, plan.Notes)
	}
	h0 := hashOf(t, s, 2)
	write(t, d, "triage/cycle/v2/system.md", "edited shadow\n")
	if _, _, err = Reconcile(ctx, s, load(t, d)); err != nil {
		t.Fatalf("a shadow may change: %v", err)
	}
	if hashOf(t, s, 2) == h0 {
		t.Fatal("the shadow's recorded hash must follow its folder")
	}
	// a recorded routine with no spec is a note, not a failure (the caller refuses a start with no live version)
	stored, _ := s.AllVersions(ctx)
	plan, err = Check(nil, stored)
	if err != nil || len(plan.Notes) != 1 || !strings.Contains(plan.Notes[0], "triage/cycle has recorded versions and no spec") {
		t.Fatalf("no spec for a recorded routine: %v %v", err, plan.Notes)
	}
}

// routine.yaml is an ordinary deploy: its bytes are not frozen.
func TestRoutineLevelEditsAreOrdinaryDeploys(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	d := fleets(t, map[string]string{"v1": "python3 a.py"})
	if _, _, err := Reconcile(ctx, s, load(t, d)); err != nil {
		t.Fatal(err)
	}
	write(t, d, "triage/cycle/routine.yaml", strings.Replace(routineYAML, "day_usd: 2", "day_usd: 9", 1))
	bound, _, err := Reconcile(ctx, s, load(t, d))
	if err != nil || bound[0].Budget.DayUSD != 9 {
		t.Fatalf("routine.yaml edit: %v %+v", err, bound)
	}
}

// A flat spec is a routine with one legacy v1: a fresh store records it live and its Hash is untouched. A change to
// what v1 owns is not frozen: it is re-recorded (see TestFlatSpecChangeIsReRecorded); a routine-level change is not
// even a change of the version.
func TestFlatSpecIsItsOwnLiveV1(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	d := t.TempDir()
	flat := routineYAML + "run: {command: \"python3 x.py\", workdir: signal-fleet/mvp, timeout: 20m}\n"
	write(t, d, "triage/cycle.yaml", flat)
	rs := load(t, d)
	hash := rs[0].Hash
	bound, plan, err := Reconcile(ctx, s, rs)
	if err != nil || rows(t, s) != "triage/cycle v1=live" || bound[0].Hash != hash || plan.States("triage/cycle") != "v1:live(new)" {
		t.Fatalf("%v %s %s %s", err, rows(t, s), bound[0].Hash, plan.States("triage/cycle"))
	}
	write(t, d, "triage/cycle.yaml", strings.Replace(flat, "day_usd: 2", "day_usd: 3", 1))
	if _, _, err := Reconcile(ctx, s, load(t, d)); err != nil {
		t.Fatalf("a budget change in a flat spec: %v", err)
	}
}

// Reconcile on the fleets this repo ships: every routine gets one live v1 and keeps its flat hash.
func TestRepoFleetsReconcile(t *testing.T) {
	rs, err := spec.LoadVersions(filepath.Join("..", "..", "fleets"))
	if err != nil {
		t.Fatal(err)
	}
	s := open(t)
	bound, _, err := Reconcile(context.Background(), s, rs)
	if err != nil {
		t.Fatal(err)
	}
	vs, _ := s.AllVersions(context.Background())
	if len(vs) != len(rs) || len(bound) != len(rs) {
		t.Fatalf("%d rows, %d bound, %d routines", len(vs), len(bound), len(rs))
	}
	for _, v := range vs {
		if v.State != store.VersionLive || v.Version != 1 || v.Hash == "" || len(v.Manifest) == 0 {
			t.Fatalf("row %+v", v)
		}
	}
	for i := range rs {
		if bound[i].Hash != rs[i].Hash {
			t.Errorf("%s: the bound routine's Hash %s differs from the loaded %s", rs[i].ID(), bound[i].Hash, rs[i].Hash)
		}
	}
}

// -check's read-only look: a missing store and a store older than the version table read as no rows, and say so.
func TestReadVersionsReadOnly(t *testing.T) {
	d := t.TempDir()
	if r, err := store.ReadVersions(filepath.Join(d, "none.db")); err != nil || r.Exists || len(r.Rows) != 0 {
		t.Fatalf("no store: %+v %v", r, err)
	}
	p := filepath.Join(d, "f.db")
	s, err := store.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := Reconcile(context.Background(), s, load(t, fleets(t, map[string]string{"v1": "python3 a.py"}))); err != nil {
		t.Fatal(err)
	}
	s.Close()
	r, err := store.ReadVersions(p)
	if err != nil || !r.Exists || r.Schema < store.VersionsSchema || len(r.Rows) != 1 || r.Rows[0].State != store.VersionLive {
		t.Fatalf("a current store: %+v %v", r, err)
	}
}

// A legacy (flat) v1 is not frozen: a changed live v1 is re-recorded at the start with a version.update event, and
// the plan says so in a line -check and the deploy print. Routine.Hash keeps moving with the spec's bytes as before.
func TestFlatSpecChangeIsReRecorded(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	d := t.TempDir()
	flat := routineYAML + "run: {command: \"python3 x.py\", workdir: signal-fleet/mvp, timeout: 20m}\nprompts:\n  system: playbooks/triage.md\n"
	write(t, d, "triage/cycle.yaml", flat)
	write(t, d, "triage/playbooks/triage.md", "one\n")
	if _, _, err := Reconcile(ctx, s, load(t, d)); err != nil {
		t.Fatal(err)
	}
	was, _, _ := s.LiveVersion(ctx, "triage/cycle")

	// a playbook edit: the ordinary change of a flat routine
	write(t, d, "triage/playbooks/triage.md", "two\n")
	rs := load(t, d)
	stored, _ := s.AllVersions(ctx)
	plan, err := Check(rs, stored)
	if err != nil || len(plan.Updates) != 1 || plan.Updates[0].Line() != "triage/cycle v1 (flat) re-recorded: prompt-file:playbooks/triage.md (changed)" {
		t.Fatalf("check: %v %+v", err, plan.Updates)
	}
	if plan.States("triage/cycle") != "v1:live" {
		t.Fatalf("a re-recorded v1 stays live: %s", plan.States("triage/cycle"))
	}
	if now, _, _ := s.LiveVersion(ctx, "triage/cycle"); now.Hash != was.Hash {
		t.Fatal("Check is read-only: it recorded something")
	}
	bound, plan, err := Reconcile(ctx, s, rs)
	if err != nil || bound[0].Version != 1 || len(plan.Updates) != 1 {
		t.Fatalf("reconcile: %v %+v", err, plan)
	}
	now, _, _ := s.LiveVersion(ctx, "triage/cycle")
	if now.Hash == was.Hash || now.Hash != rs[0].VersionHash || now.State != store.VersionLive || now.LiveAt != was.LiveAt {
		t.Fatalf("recorded %+v, was %+v", now, was)
	}
	e, _ := s.LatestEvent(ctx, "version.update", "triage/cycle@v1")
	if e == nil || e.Actor != Actor || !strings.Contains(e.Detail, was.Hash+" -> "+now.Hash) || !strings.Contains(e.Detail, "prompt-file:playbooks/triage.md (changed)") {
		t.Fatalf("event %+v", e)
	}
	// recorded once: the next start finds nothing to do
	stored, _ = s.AllVersions(ctx)
	if plan, err = Check(load(t, d), stored); err != nil || len(plan.Updates) != 0 {
		t.Fatalf("second check: %v %+v", err, plan.Updates)
	}
	// a changed command is a change of the version too
	write(t, d, "triage/cycle.yaml", strings.Replace(flat, "python3 x.py", "python3 y.py", 1))
	stored, _ = s.AllVersions(ctx)
	if plan, err = Check(load(t, d), stored); err != nil || len(plan.Updates) != 1 || plan.Updates[0].Line() != "triage/cycle v1 (flat) re-recorded: run (changed)" {
		t.Fatalf("command: %v %+v", err, plan.Updates)
	}
}

// A folder version stays frozen, and the refusal says what to do about it: ship the next number, or re-record.
func TestFolderFreezeMessageNamesTheWayOut(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	d := fleets(t, map[string]string{"v1": "python3 a.py", "v2": "python3 b.py"})
	if _, _, err := Reconcile(ctx, s, load(t, d)); err != nil {
		t.Fatal(err)
	}
	write(t, d, "triage/cycle/v1/system.md", "edited\n")
	_, _, err := Reconcile(ctx, s, load(t, d))
	if err == nil {
		t.Fatal("a changed live folder must refuse")
	}
	for _, want := range []string{"versions: triage/cycle v1 is live and its folder changed", "system.md (changed)",
		"ship the change as v3 (a shadow or retired folder may change)", "`flotilla versions rerecord triage/cycle v1 --reason R` if this change is deliberate"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal lacks %q: %v", want, err)
		}
	}
}

// Moving a live flat routine to the folder layout changes its recipe: refused, until the operator re-records it.
// Then the start passes, v1 stays live, and the event log says who moved the hash and why.
func TestRerecordUnblocksAFolderMove(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	d := t.TempDir()
	flat := routineYAML + "run: {command: \"python3 a.py\", workdir: signal-fleet/mvp, timeout: 20m}\nprompts:\n  system: system.md\n"
	write(t, d, "triage/cycle.yaml", flat)
	write(t, d, "triage/system.md", "the text\n")
	if _, _, err := Reconcile(ctx, s, load(t, d)); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(d, "triage/cycle.yaml")); err != nil {
		t.Fatal(err)
	}
	moved := fleets(t, map[string]string{"v1": "python3 a.py"})
	rs := load(t, moved)
	_, _, err := Reconcile(ctx, s, rs)
	if err == nil || !strings.Contains(err.Error(), "triage/cycle v1 is live and its folder changed") {
		t.Fatalf("the move is refused: %v", err)
	}
	v := rs[0].VersionNamed(1)
	res, err := s.Rerecord(ctx, store.RerecordArgs{Routine: "triage/cycle", Version: 1, Hash: v.Hash, Manifest: v.Manifest, Actor: "operator", Via: "api", Reason: "step 6: moved to the folder layout"})
	if err != nil || res.NewHash != v.Hash || res.OldHash == v.Hash || len(res.Changed) == 0 {
		t.Fatalf("rerecord: %v %+v", err, res)
	}
	bound, plan, err := Reconcile(ctx, s, rs)
	if err != nil || bound[0].Version != 1 || plan.States("triage/cycle") != "v1:live" || len(plan.Updates) != 0 {
		t.Fatalf("after the re-record: %v %s %+v", err, plan.States("triage/cycle"), plan.Updates)
	}
	e, _ := s.LatestEvent(ctx, "version.rerecord", "triage/cycle@v1")
	if e == nil || e.Actor != "operator" || e.Via != "api" || !strings.Contains(e.Detail, res.OldHash+" -> "+res.NewHash) || !strings.Contains(e.Detail, "step 6: moved to the folder layout") {
		t.Fatalf("event %+v", e)
	}
}

// Reconcile's refusals write nothing, for any routine: two routines, one with a new shadow to record and one whose
// live folder changed, leave the store as it was.
func TestAFailedCheckWritesNothingForAnyRoutine(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	d := fleets(t, map[string]string{"v1": "python3 a.py"})
	write(t, d, "ops/backup/routine.yaml", strings.Replace(strings.Replace(routineYAML, "fleet: triage", "fleet: ops", 1), "routine: cycle", "routine: backup", 1))
	write(t, d, "ops/backup/v1/spec.yaml", versionYAML("python3 b.py"))
	write(t, d, "ops/backup/v1/system.md", "b\n")
	if _, _, err := Reconcile(ctx, s, load(t, d)); err != nil {
		t.Fatal(err)
	}
	before := rows(t, s)
	write(t, d, "triage/cycle/v2/spec.yaml", versionYAML("python3 two.py")) // a new shadow for triage/cycle: would be recorded
	write(t, d, "triage/cycle/v2/system.md", "two\n")
	write(t, d, "ops/backup/v1/system.md", "edited\n") // ops/backup's live folder changed: refused
	events0, _ := s.Events(ctx, 1000)
	if _, _, err := Reconcile(ctx, s, load(t, d)); err == nil || !strings.Contains(err.Error(), "ops/backup v1 is live and its folder changed") {
		t.Fatalf("the freeze must refuse: %v", err)
	}
	if after := rows(t, s); after != before {
		t.Fatalf("a refused check wrote: %q -> %q", before, after)
	}
	if events1, _ := s.Events(ctx, 1000); len(events1) != len(events0) {
		t.Fatalf("a refused check wrote events: %d -> %d", len(events0), len(events1))
	}
}

// A first deploy of a folder that holds only v3 goes live with v3 (the lowest loaded is the first version, whatever
// its number).
func TestOnlyV3PresentGoesLive(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	d := fleets(t, map[string]string{"v3": "python3 c.py"})
	bound, plan, err := Reconcile(ctx, s, load(t, d))
	if err != nil || bound[0].Version != 3 || rows(t, s) != "triage/cycle v3=live" || plan.States("triage/cycle") != "v3:live(new)" {
		t.Fatalf("%v %s %s", err, rows(t, s), plan.States("triage/cycle"))
	}
}

// Recorded versions and none live is a store this code did not make: Reconcile refuses to guess a version to run.
func TestRowsButNoLiveIsAnError(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "f.db")
	s, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	d := fleets(t, map[string]string{"v1": "python3 a.py", "v2": "python3 b.py"})
	if _, _, err := Reconcile(ctx, s, load(t, d)); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE routine_versions SET state = 'retired' WHERE state = 'live'`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Reconcile(ctx, s, load(t, d)); err == nil || !strings.Contains(err.Error(), "triage/cycle has no live version recorded") {
		t.Fatalf("rows and no live version: %v", err)
	}
}

// A routine recorded in the store and gone from the fleets folder is a note, and the start goes on with the rest.
func TestARecordedRoutineRemovedFromFleetsIsANote(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	d := fleets(t, map[string]string{"v1": "python3 a.py"})
	write(t, d, "ops/backup/routine.yaml", strings.Replace(strings.Replace(routineYAML, "fleet: triage", "fleet: ops", 1), "routine: cycle", "routine: backup", 1))
	write(t, d, "ops/backup/v1/spec.yaml", versionYAML("python3 b.py"))
	write(t, d, "ops/backup/v1/system.md", "b\n")
	if _, _, err := Reconcile(ctx, s, load(t, d)); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(d, "ops")); err != nil {
		t.Fatal(err)
	}
	bound, plan, err := Reconcile(ctx, s, load(t, d))
	if err != nil || len(bound) != 1 || bound[0].ID() != "triage/cycle" {
		t.Fatalf("the start goes on: %v %+v", err, bound)
	}
	if len(plan.Notes) != 1 || !strings.Contains(plan.Notes[0], "ops/backup has recorded versions and no spec") {
		t.Fatalf("notes %v", plan.Notes)
	}
	if !strings.Contains(rows(t, s), "ops/backup v1=live") {
		t.Fatalf("its history stays: %s", rows(t, s))
	}
}

// Each is every version that loads, bound; Bound is each routine as its live version; Runnable leaves out what is
// retired, and RetiredLoaded is what it leaves out.
func TestEachBoundRunnableAndRetired(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	d := fleets(t, map[string]string{"v1": "python3 a.py", "v2": "python3 b.py", "v3": "python3 c.py"})
	write(t, d, "triage/cycle/v4/spec.yaml", "run: {command: x, timeout: 1s}\n") // does not load
	rs := load(t, d)
	if _, _, err := Reconcile(ctx, s, rs); err == nil {
		t.Fatal("a new version that does not load is refused")
	}
	if err := os.RemoveAll(filepath.Join(d, "triage/cycle/v4")); err != nil {
		t.Fatal(err)
	}
	write(t, d, "triage/cycle/v4/spec.yaml", "run: {command: x, timeout: 1s}\n")
	rs = load(t, d)
	var cmds []string
	for _, b := range Each(rs) {
		cmds = append(cmds, b.Run.Command)
	}
	if strings.Join(cmds, ",") != "python3 a.py,python3 b.py,python3 c.py" {
		t.Fatalf("Each is every version that loads: %v", cmds)
	}
	if err := os.RemoveAll(filepath.Join(d, "triage/cycle/v4")); err != nil {
		t.Fatal(err)
	}
	rs = load(t, d)
	bound, plan, err := Reconcile(ctx, s, rs)
	if err != nil || bound[0].Version != 1 {
		t.Fatal(err)
	}
	if err := s.Promote(ctx, store.PromoteArgs{Routine: "triage/cycle", Version: 2, Hash: rs[0].VersionNamed(2).Hash, Autonomy: "record", Current: "record", Actor: "a", Reason: "r"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Promote(ctx, store.PromoteArgs{Routine: "triage/cycle", Version: 3, Hash: rs[0].VersionNamed(3).Hash, Autonomy: "record", Current: "record", Actor: "a", Reason: "r"}); err != nil {
		t.Fatal(err)
	}
	stored, _ := s.AllVersions(ctx) // v1 retired, v2 previous, v3 live
	plan, err = Check(rs, stored)
	if err != nil {
		t.Fatal(err)
	}
	if b := Bound(rs, plan); len(b) != 1 || b[0].Version != 3 || b[0].Run.Command != "python3 c.py" {
		t.Fatalf("Bound is the live version: %+v", b)
	}
	var run, retired []string
	for _, b := range plan.Runnable(rs) {
		run = append(run, b.Run.Command)
	}
	for _, b := range plan.RetiredLoaded(rs) {
		retired = append(retired, b.Run.Command)
	}
	if strings.Join(run, ",") != "python3 b.py,python3 c.py" || strings.Join(retired, ",") != "python3 a.py" {
		t.Fatalf("runnable %v, retired %v", run, retired)
	}
}
