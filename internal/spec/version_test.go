package spec

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// routine.yaml and v1/spec.yaml for the folder layout: good's routine-level and version-level halves.
const (
	folderRoutine = `fleet: triage
routine: cycle
description: one pass
enabled: true
worker: triage
trigger: {every: 10m}
autonomy: shadow
budget: {day_usd: 2}
freshness: 35m
`
	folderVersion = `run: {command: "python3 x.py", workdir: signal-fleet/mvp, timeout: 20m}
prompts:
  system: system.md
`
)

func writeFolder(t *testing.T, d string) {
	t.Helper()
	write(t, d, "triage/cycle/routine.yaml", folderRoutine)
	write(t, d, "triage/cycle/v1/spec.yaml", folderVersion)
	write(t, d, "triage/cycle/v1/system.md", "the system text\n")
}

// Every flat spec shipped here keeps the hash it had before routine versions existed: that hash is
// runs.spec_hash and the deploy's before/after proof. The values in testdata were generated from the
// code at main@a9dc277; a legitimate edit of a spec or of a prompt it declares changes its line. A routine
// that moved to the folder layout (platform/heartbeat, e2e/versioned) has a new recipe, so it leaves this file
// for golden_folder_hashes.txt: its Routine.Hash (what runs.spec_hash records) and its v1's frozen hash. Both
// are recorded when the routine goes live, and a store that recorded the flat recipe refuses the folder one
// (internal/versions: the freeze); moving a routine that is already live needs step 6's controlled re-record.
func TestFlatSpecHashesAreGolden(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "golden_hashes.txt"))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{}
	for _, l := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		f := strings.Fields(l)
		want[f[0]] = f[1]
	}
	got, folder := map[string]string{}, map[string]string{}
	for _, d := range []string{filepath.Join("..", "..", "fleets"), filepath.Join("..", "..", "deploy", "e2e", "fleets")} {
		routines, err := Load(d)
		if err != nil {
			t.Fatal(err)
		}
		for _, j := range routines {
			if !j.Versions[0].Legacy {
				folder[j.ID()] = j.Hash + " " + j.VersionHash
				continue
			}
			got[j.ID()] = j.Hash
			if len(j.Versions) != 1 || !j.Versions[0].Legacy || j.Version != 1 || j.VersionHash == "" {
				t.Errorf("%s: a flat spec is v1 (legacy), got %d versions, bound v%d", j.ID(), len(j.Versions), j.Version)
			}
		}
	}
	var ids []string
	for id := range want {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if got[id] != want[id] {
			t.Errorf("%s: Routine.Hash %q, golden %q", id, got[id], want[id])
		}
	}
	if len(got) != len(want) {
		t.Errorf("%d flat specs loaded, %d in the golden file: add a line for a new routine", len(got), len(want))
	}
	raw, err = os.ReadFile(filepath.Join("testdata", "golden_folder_hashes.txt"))
	if err != nil {
		t.Fatal(err)
	}
	wantFolder := map[string]string{}
	for _, l := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		f := strings.SplitN(l, " ", 2)
		wantFolder[f[0]] = f[1]
	}
	for id, h := range folder {
		if wantFolder[id] != h {
			t.Errorf("%s (folder layout): Hash and v1 VersionHash %q, golden %q", id, h, wantFolder[id])
		}
	}
	if len(folder) != len(wantFolder) {
		t.Errorf("%d folder routines loaded, %d in golden_folder_hashes.txt", len(folder), len(wantFolder))
	}
}

// The legacy v1's hash recipe is pinned on a fixed spec (an independent computation of it is in the want comment), so a new field of Version cannot move every
// recorded hash without a test saying so.
func TestLegacyVersionHashRecipe(t *testing.T) {
	d := t.TempDir()
	write(t, d, "triage/cycle.yaml", good)
	routines, err := Load(d)
	if err != nil {
		t.Fatal(err)
	}
	// sha256 over "run\x00" + sha256("command: python3 x.py\ntimeout: 20m\nworkdir: signal-fleet/mvp\n") + "\n": the recipe is the
	// version-owned keys' own YAML, re-encoded with sorted keys, never the Go structs
	const want = "cfe909770ba8d771f2ef30b51cb2646af3112aa635f5de720d45dd3e705dede6"
	if got := routines[0].VersionHash; got != want {
		t.Fatalf("legacy v1 hash %s, pinned %s", got, want)
	}
	if got := routines[0].Versions[0].Manifest; len(got) != 1 || got["run"] == "" {
		t.Fatalf("manifest %v", got)
	}
	// a routine-level edit of a flat spec moves Hash but not the version it owns
	write(t, d, "triage/cycle.yaml", strings.Replace(good, "day_usd: 2", "day_usd: 3", 1))
	again, err := Load(d)
	if err != nil {
		t.Fatal(err)
	}
	if again[0].VersionHash != routines[0].VersionHash || again[0].Hash == routines[0].Hash {
		t.Fatal("a budget change is the routine's: Hash moves, the version's hash does not")
	}
	write(t, d, "triage/cycle.yaml", strings.Replace(good, "python3 x.py", "python3 y.py", 1))
	again, _ = Load(d)
	if again[0].VersionHash == routines[0].VersionHash {
		t.Fatal("a changed command is the version's")
	}
}

func TestLegacyVersionHashesPromptsInPlace(t *testing.T) {
	d := t.TempDir()
	write(t, d, "triage/cycle.yaml", good+"prompts:\n  system: playbooks/triage.md\n")
	write(t, d, "triage/playbooks/triage.md", "one\n")
	a, err := Load(d)
	if err != nil {
		t.Fatal(err)
	}
	write(t, d, "triage/playbooks/triage.md", "two\n")
	b, _ := Load(d)
	if a[0].VersionHash == b[0].VersionHash {
		t.Fatal("the prompt file is read in place: its bytes are in the hash")
	}
	if ch := ChangedFiles(a[0].Versions[0].Manifest, b[0].Versions[0].Manifest); len(ch) != 1 || ch[0] != "prompt-file:playbooks/triage.md (changed)" {
		t.Fatalf("changed: %v", ch)
	}
}

func TestFolderLayout(t *testing.T) {
	d := t.TempDir()
	writeFolder(t, d)
	write(t, d, "triage/cycle/v2/spec.yaml", strings.Replace(folderVersion, "x.py", "x2.py", 1))
	write(t, d, "triage/cycle/v2/system.md", "the second system text\n")
	routines, err := Load(d)
	if err != nil {
		t.Fatal(err)
	}
	if len(routines) != 1 {
		t.Fatalf("%d routines", len(routines))
	}
	j := routines[0]
	if j.ID() != "triage/cycle" || j.Every.String() != "10m0s" || j.Limit.String() != "20m0s" || len(j.Hash) != 12 {
		t.Fatalf("got %+v", j)
	}
	if len(j.Versions) != 2 || j.Version != 1 || j.Run.Command != "python3 x.py" || j.Versions[0].Legacy {
		t.Fatalf("bound to the lowest version: %+v", j.Versions)
	}
	if j.Prompts["system"] != "cycle/v1/system.md" || j.PromptSHA["system"] != shaHex([]byte("the system text\n")) {
		t.Fatalf("a prompt stays relative to the fleet's folder: %v %v", j.Prompts, j.PromptSHA)
	}
	two, err := j.WithVersion(2)
	if err != nil {
		t.Fatal(err)
	}
	if two.Run.Command != "python3 x2.py" || two.Version != 2 || two.VersionHash == j.VersionHash || two.Hash == j.Hash || two.Prompts["system"] != "cycle/v2/system.md" {
		t.Fatalf("v2: %+v", two)
	}
	if j.Version != 1 || j.Run.Command != "python3 x.py" {
		t.Fatal("WithVersion must not move the routine it was asked on")
	}
	if two.Autonomy != j.Autonomy || two.Worker != j.Worker || two.Limit != j.Limit {
		t.Fatal("the routine's own fields are every version's")
	}
	if _, err := j.WithVersion(3); err == nil {
		t.Fatal("no v3")
	}
}

func TestFolderVersionHash(t *testing.T) {
	d := t.TempDir()
	writeFolder(t, d)
	hashOf := func(dir string) string {
		rs, err := Load(dir)
		if err != nil {
			t.Fatal(err)
		}
		return rs[0].VersionHash
	}
	base := hashOf(d)
	if hashOf(d) != base {
		t.Fatal("hash is not stable")
	}
	write(t, d, "triage/cycle/routine.yaml", strings.Replace(folderRoutine, "day_usd: 2", "day_usd: 5", 1))
	if hashOf(d) != base {
		t.Fatal("routine.yaml is the routine's, not the version's: an ordinary deploy")
	}
	for name, edit := range map[string]func(dd string){
		"one byte":   func(dd string) { write(t, dd, "triage/cycle/v1/system.md", "the system texT\n") },
		"added file": func(dd string) { write(t, dd, "triage/cycle/v1/extra.md", "x") },
		"renamed file": func(dd string) {
			if err := os.Rename(filepath.Join(dd, "triage/cycle/v1/system.md"), filepath.Join(dd, "triage/cycle/v1/other.md")); err != nil {
				t.Fatal(err)
			}
			write(t, dd, "triage/cycle/v1/spec.yaml", strings.Replace(folderVersion, "system.md", "other.md", 1))
		},
	} {
		t.Run(name, func(t *testing.T) {
			dd := t.TempDir()
			writeFolder(t, dd)
			edit(dd)
			if hashOf(dd) == base {
				t.Fatalf("%s must change the version's hash", name)
			}
		})
	}
}

func TestFolderChangedFilesNamed(t *testing.T) {
	d := t.TempDir()
	writeFolder(t, d)
	a, _ := Load(d)
	write(t, d, "triage/cycle/v1/system.md", "changed\n")
	write(t, d, "triage/cycle/v1/new.md", "n")
	b, _ := Load(d)
	got := strings.Join(ChangedFiles(a[0].Versions[0].Manifest, b[0].Versions[0].Manifest), ", ")
	if got != "new.md (added), system.md (changed)" {
		t.Fatalf("got %q", got)
	}
}

func TestFolderRules(t *testing.T) {
	for name, c := range map[string]struct {
		edit func(t *testing.T, d string)
		want string
	}{
		"both layouts for one routine": {func(t *testing.T, d string) { write(t, d, "triage/cycle.yaml", good) }, "also a flat spec"},
		"routine.yaml carries a version field": {func(t *testing.T, d string) {
			write(t, d, "triage/cycle/routine.yaml", folderRoutine+"run: {command: x}\n")
		}, "field run not found"},
		"spec.yaml carries a routine field": {func(t *testing.T, d string) {
			write(t, d, "triage/cycle/v1/spec.yaml", folderVersion+"autonomy: auto\n")
		}, "field autonomy not found"},
		"a bad version folder name": {func(t *testing.T, d string) { write(t, d, "triage/cycle/v01/spec.yaml", folderVersion) }, "neither routine.yaml nor a version folder"},
		"a stray file":              {func(t *testing.T, d string) { write(t, d, "triage/cycle/notes.md", "x") }, "neither routine.yaml nor a version folder"},
		"no version":                {func(t *testing.T, d string) { _ = os.RemoveAll(filepath.Join(d, "triage/cycle/v1")) }, "no version folder"},
		"folder name is the routine": {func(t *testing.T, d string) {
			write(t, d, "triage/cycle/routine.yaml", strings.Replace(folderRoutine, "routine: cycle", "routine: other", 1))
		}, `routine "other", but the folder is cycle/`},
		"fleet name is the folder": {func(t *testing.T, d string) {
			write(t, d, "triage/cycle/routine.yaml", strings.Replace(folderRoutine, "fleet: triage", "fleet: ops", 1))
		}, `fleet "ops", but the folder is under triage/`},
		"a prompt leaving the version": {func(t *testing.T, d string) {
			write(t, d, "triage/cycle/v1/spec.yaml", strings.Replace(folderVersion, "system.md", "../v2/system.md", 1))
		}, "not a .md or .txt path inside"},
		"an absolute prompt": {func(t *testing.T, d string) {
			write(t, d, "triage/cycle/v1/spec.yaml", strings.Replace(folderVersion, "system.md", "/etc/system.md", 1))
		}, "not a .md or .txt path inside"},
		"a symlink in a version": {func(t *testing.T, d string) {
			if err := os.Symlink("/etc/hostname", filepath.Join(d, "triage/cycle/v1/link.md")); err != nil {
				t.Fatal(err)
			}
		}, "not a regular file"},
		"an invalid version": {func(t *testing.T, d string) {
			write(t, d, "triage/cycle/v1/spec.yaml", strings.Replace(folderVersion, "timeout: 20m", "timeout: 1s", 1))
		}, "run.timeout"},
		"two routines one case kind": {func(t *testing.T, d string) {
			c := "trigger: {case: {kind: code_bug}}\n"
			write(t, d, "triage/cycle/routine.yaml", strings.Replace(folderRoutine, "trigger: {every: 10m}\n", c, 1))
			write(t, d, "triage/other.yaml", strings.Replace(strings.Replace(good, "routine: cycle", "routine: other", 1), "trigger: {every: 10m}\n", c, 1)+"")
		}, "already have an acting routine"},
	} {
		t.Run(name, func(t *testing.T) {
			d := t.TempDir()
			writeFolder(t, d)
			c.edit(t, d)
			if _, err := Load(d); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want an error with %q, got %v", c.want, err)
			}
		})
	}
}

// LoadVersions keeps an invalid version so the caller can tell a retired one from a live one; Load
// refuses the same tree, as it refuses any invalid spec.
func TestInvalidVersionKept(t *testing.T) {
	d := t.TempDir()
	writeFolder(t, d)
	write(t, d, "triage/cycle/v2/spec.yaml", "run: {command: x, timeout: 1s}\n")
	if _, err := Load(d); err == nil {
		t.Fatal("Load fails on an invalid version")
	}
	rs, err := LoadVersions(d)
	if err != nil {
		t.Fatal(err)
	}
	j := rs[0]
	if j.Version != 1 || len(j.Versions) != 2 || !j.Versions[0].Valid() || j.Versions[1].Valid() || j.Versions[1].Err == nil {
		t.Fatalf("versions %+v", j.Versions)
	}
	if _, err := j.WithVersion(2); err == nil {
		t.Fatal("an invalid version cannot be bound")
	}
}

// FLEETD_STAGE is the contract the triage code and the ops routines read: autonomy record gives
// "shadow", and the rename must not move it. Only a new version of a routine may read "record".
func TestFleetdStageTable(t *testing.T) {
	for in, want := range map[string]string{AutonomyRecord: "shadow", AutonomyPropose: "propose", AutonomyAuto: "live"} {
		if got := FleetdStage(in); got != want {
			t.Errorf("FleetdStage(%q) = %q, want %q", in, got, want)
		}
	}
}

// The old word still loads, as record, with a note; the spec's bytes (and so its hash) are untouched.
func TestShadowIsTheDeprecatedNameOfRecord(t *testing.T) {
	for _, name := range []string{"flat", "folder"} {
		t.Run(name, func(t *testing.T) {
			d := t.TempDir()
			if name == "flat" {
				write(t, d, "triage/cycle.yaml", good+"max_autonomy: propose\n")
			} else {
				writeFolder(t, d)
				write(t, d, "triage/cycle/routine.yaml", folderRoutine+"max_autonomy: shadow\n")
			}
			rs, err := Load(d)
			if err != nil {
				t.Fatal(err)
			}
			j := rs[0]
			if j.Autonomy != AutonomyRecord {
				t.Fatalf("autonomy %q", j.Autonomy)
			}
			want := 1
			if name == "folder" {
				want = 2
				if j.MaxAutonomy != AutonomyRecord {
					t.Fatalf("max_autonomy %q", j.MaxAutonomy)
				}
			}
			if len(j.Notes) != want || !strings.Contains(j.Notes[0], "write record") {
				t.Fatalf("notes %v", j.Notes)
			}
		})
	}
	d := t.TempDir()
	write(t, d, "triage/cycle.yaml", strings.Replace(good, "autonomy: shadow", "autonomy: record", 1))
	rs, err := Load(d)
	if err != nil || rs[0].Autonomy != AutonomyRecord || len(rs[0].Notes) != 0 {
		t.Fatalf("record is the name: %v %v", err, rs)
	}
}

// The lowest autonomy level is record. A new use of the old identifiers would bring the word shadow
// back into a place that now belongs to versions; the compiler cannot catch it, so this does.
func TestOldAutonomyIdentifiersStayGone(t *testing.T) {
	old := regexp.MustCompile(`AutonomyShadow|ShadowPlan|policy\.Shadow`)
	for _, root := range []string{"../../cmd", "../../internal"} {
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "spec/version_test.go") {
				return err
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			if m := old.Find(b); m != nil {
				t.Errorf("%s uses %s: the level is spec.AutonomyRecord, the outcome policy.Record, the resolution cases.PlanRecorded", p, m)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestAcceptance(t *testing.T) {
	// [pairs] when a routine declares nothing; the golden test above keeps every flat spec's hash
	d := t.TempDir()
	writeFolder(t, d)
	rs, err := Load(d)
	if err != nil || len(rs[0].Acceptance) != 0 || strings.Join(rs[0].Accepts(), ",") != "pairs" {
		t.Fatalf("default: %v %v", rs, err)
	}
	// routine.yaml declares it
	write(t, d, "triage/cycle/routine.yaml", folderRoutine+"acceptance: [replay, scenario_eval]\n")
	if rs, err = Load(d); err != nil || strings.Join(rs[0].Accepts(), ",") != "replay,scenario_eval" {
		t.Fatalf("folder: %v %v", rs, err)
	}
	// a flat spec declares it too, and the routine's own hash moves only because its bytes did
	flat := t.TempDir()
	write(t, flat, "triage/cycle.yaml", good)
	before, _ := Load(flat)
	write(t, flat, "triage/cycle.yaml", good+"acceptance: [pairs, replay]\n")
	after, err := Load(flat)
	if err != nil || strings.Join(after[0].Accepts(), ",") != "pairs,replay" || after[0].Hash == before[0].Hash {
		t.Fatalf("flat: %v %v", after, err)
	}
	for _, bad := range []string{"acceptance: [vibes]\n", "acceptance: [pairs, pairs]\n"} {
		write(t, flat, "triage/cycle.yaml", good+bad)
		if _, err := Load(flat); err == nil || !strings.Contains(err.Error(), "acceptance") {
			t.Fatalf("%q: %v", bad, err)
		}
	}
}

// A routine's interval and freshness are the routine's, whichever version is bound or broken: a retired v1
// that no longer parses must not leave Every at 0 (time.NewTicker(0) panics in the scheduler).
func TestRoutineTimingSurvivesABrokenFirstVersion(t *testing.T) {
	d := t.TempDir()
	writeFolder(t, d)
	write(t, d, "triage/cycle/v1/spec.yaml", "run: [this is: not, a version\n")
	write(t, d, "triage/cycle/v2/spec.yaml", folderVersion)
	write(t, d, "triage/cycle/v2/system.md", "two\n")
	rs, err := LoadVersions(d)
	if err != nil {
		t.Fatal(err)
	}
	j := rs[0]
	if j.Versions[0].Valid() || j.Version != 2 {
		t.Fatalf("v1 should be broken and v2 bound: %+v", j.Versions)
	}
	if j.Every.String() != "10m0s" || j.Fresh.String() != "35m0s" {
		t.Fatalf("Every %s, Fresh %s: the routine's own", j.Every, j.Fresh)
	}
}

// A bad routine-level field is the routine's error, once, not every version's.
func TestRoutineFieldErrorsFailTheRoutineOnce(t *testing.T) {
	d := t.TempDir()
	writeFolder(t, d)
	write(t, d, "triage/cycle/routine.yaml", strings.Replace(folderRoutine, "every: 10m", "every: soon", 1))
	_, err := LoadVersions(d)
	if err == nil || strings.Count(err.Error(), "trigger.every") != 1 {
		t.Fatalf("want one trigger.every error, got %v", err)
	}
}

// The legacy hash reads the spec's own YAML: a comment, the order of keys and the flow or block style of a
// mapping do not move it; a changed value does.
func TestLegacyVersionHashIsTheYAMLNotTheStructs(t *testing.T) {
	d := t.TempDir()
	write(t, d, "triage/cycle.yaml", good)
	a, err := Load(d)
	if err != nil {
		t.Fatal(err)
	}
	reshaped := strings.Replace(good, `run: {command: "python3 x.py", workdir: signal-fleet/mvp, timeout: 20m}`,
		"# the run, as a block\nrun:\n  timeout: 20m\n  workdir: signal-fleet/mvp\n  command: python3 x.py\n", 1)
	write(t, d, "triage/cycle.yaml", reshaped)
	b, err := Load(d)
	if err != nil {
		t.Fatal(err)
	}
	if a[0].VersionHash != b[0].VersionHash {
		t.Fatal("comments, key order and mapping style are not the version's content")
	}
	write(t, d, "triage/cycle.yaml", strings.Replace(reshaped, "timeout: 20m", "timeout: 21m", 1))
	c, _ := Load(d)
	if c[0].VersionHash == a[0].VersionHash {
		t.Fatal("a changed value is")
	}
}

// The same guard outside Go, where the compiler cannot help. The old word is still the stage a routine reads
// (FLEETD_STAGE) and triage's own disposition stage, so none of those is a hit; what is: the word as an autonomy
// level in a template's options, in a Python list of levels or an autonomy comparison, or in a spec that is not a
// flat spec on the golden list (a flat spec's alias `autonomy: shadow` stays because its bytes are its golden hash;
// a routine.yaml or a version never writes it).
func TestOldAutonomyWordStaysOutOfTemplatesPythonAndNewSpecs(t *testing.T) {
	root := filepath.Join("..", "..")
	skip := func(p string) bool {
		for _, dir := range []string{".git", ".claude", ".venv", "web", "site", "node_modules", ".impeccable", "docs"} {
			if d := filepath.Join(root, dir); p == d || strings.HasPrefix(p, d+string(filepath.Separator)) {
				return true
			}
		}
		return false
	}
	tmpl := regexp.MustCompile(`(value|selected)="shadow"|>shadow<`)
	py := regexp.MustCompile(`['"]shadow['"][^\n]{0,40}['"]propose['"]|['"]propose['"][^\n]{0,40}['"]shadow['"]|autonomy[\w"']*\s*(==|!=|in|:|=)\s*[\(\[{]?['"]shadow['"]`)
	alias := regexp.MustCompile(`(?m)^(max_)?autonomy:\s*shadow\b`)
	raw, err := os.ReadFile(filepath.Join("testdata", "golden_hashes.txt"))
	if err != nil {
		t.Fatal(err)
	}
	golden := map[string]bool{}
	for _, l := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		golden[strings.Fields(l)[0]] = true
	}
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skip(p) {
				return filepath.SkipDir
			}
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		slash := filepath.ToSlash(rel)
		switch {
		case strings.HasSuffix(p, ".html") && strings.Contains(slash, "templates/"):
			b, _ := os.ReadFile(p)
			if m := tmpl.Find(b); m != nil {
				t.Errorf("%s offers shadow as an autonomy level (%q): the level is record", slash, m)
			}
		case strings.HasSuffix(p, ".py") && !strings.HasSuffix(slash, "deploy/e2e/old_store.py"): // old_store.py is the schema-25 store, on purpose
			b, _ := os.ReadFile(p)
			if m := py.Find(b); m != nil {
				t.Errorf("%s uses shadow as an autonomy level (%q): the level is record (FLEETD_STAGE and triage's stage stay shadow)", slash, m)
			}
		case strings.HasSuffix(p, ".yaml") && (strings.HasPrefix(slash, "fleets/") || strings.HasPrefix(slash, "deploy/e2e/fleets/")):
			b, _ := os.ReadFile(p)
			if !alias.Match(b) {
				return nil
			}
			parts := strings.Split(strings.TrimPrefix(strings.TrimPrefix(slash, "deploy/e2e/"), "fleets/"), "/")
			if len(parts) != 2 || !golden[parts[0]+"/"+strings.TrimSuffix(parts[1], ".yaml")] {
				t.Errorf("%s writes the old autonomy word: only a flat spec on golden_hashes.txt keeps it (its bytes are its hash); write record", slash)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Loader edges of the folder layout, each pinned: what a version folder may hold and how it is named.
func TestFolderLoaderEdges(t *testing.T) {
	t.Run("a version folder without spec.yaml loads as a broken version, not a crash", func(t *testing.T) {
		d := t.TempDir()
		writeFolder(t, d)
		write(t, d, "triage/cycle/v2/system.md", "text only\n")
		rs, err := LoadVersions(d)
		if err != nil {
			t.Fatal(err)
		}
		if v := rs[0].VersionNamed(2); v == nil || v.Valid() || !strings.Contains(v.Err.Error(), "spec.yaml") {
			t.Fatalf("v2: %+v", v)
		}
		if rs[0].Version != 1 || rs[0].Every.String() != "10m0s" {
			t.Fatalf("v1 stays bound: %d %s", rs[0].Version, rs[0].Every)
		}
	})
	t.Run("a file in a subdirectory is part of the version", func(t *testing.T) {
		d := t.TempDir()
		writeFolder(t, d)
		a, _ := Load(d)
		write(t, d, "triage/cycle/v1/sub/deeper/note.md", "x")
		b, err := Load(d)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := b[0].Versions[0].Manifest["sub/deeper/note.md"]; !ok || a[0].VersionHash == b[0].VersionHash {
			t.Fatalf("manifest %v", b[0].Versions[0].Manifest)
		}
	})
	t.Run("hidden files: ignored beside routine.yaml, part of the version inside it", func(t *testing.T) {
		d := t.TempDir()
		writeFolder(t, d)
		a, _ := Load(d)
		write(t, d, "triage/cycle/.DS_Store", "junk") // the routine's folder: not a version, not an error
		if _, err := Load(d); err != nil {
			t.Fatalf("a dotfile beside routine.yaml: %v", err)
		}
		// inside v1/ a frozen version must not change without the hash saying so, so a hidden file counts
		write(t, d, "triage/cycle/v1/.DS_Store", "junk")
		b, err := Load(d)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := b[0].Versions[0].Manifest[".DS_Store"]; !ok || a[0].VersionHash == b[0].VersionHash {
			t.Fatalf("a hidden file in a version is hashed: %v", b[0].Versions[0].Manifest)
		}
	})
	t.Run("v0, v01 and a number too large are not versions", func(t *testing.T) {
		for _, name := range []string{"v0", "v01", "v99999999999999999999", "V1", "v1.bak"} {
			d := t.TempDir()
			writeFolder(t, d)
			write(t, d, "triage/cycle/"+name+"/spec.yaml", folderVersion)
			if _, err := Load(d); err == nil || !strings.Contains(err.Error(), name) {
				t.Errorf("%s: want an error naming it, got %v", name, err)
			}
		}
	})
	t.Run("the hash does not depend on the order files were made in", func(t *testing.T) {
		hash := func(order []string) string {
			d := t.TempDir()
			write(t, d, "triage/cycle/routine.yaml", folderRoutine)
			for _, f := range order {
				body := "text " + f + "\n"
				if f == "spec.yaml" {
					body = strings.Replace(folderVersion, "system.md", "a.md", 1)
				}
				write(t, d, "triage/cycle/v1/"+f, body)
			}
			rs, err := Load(d)
			if err != nil {
				t.Fatal(err)
			}
			return rs[0].VersionHash
		}
		if a, b := hash([]string{"spec.yaml", "a.md", "z/b.md", "c.md"}), hash([]string{"c.md", "z/b.md", "a.md", "spec.yaml"}); a != b {
			t.Fatalf("%s != %s", a, b)
		}
	})
}

// A routine may name its shadow worker, the state its shadows start from and the repository it works on; each is checked, and a
// version's spec.yaml may not carry them (they are the routine's).
func TestShadowKeysOfAFolderRoutine(t *testing.T) {
	d := t.TempDir()
	write(t, d, "triage/cycle/routine.yaml", folderRoutine+"shadow_worker: triage-shadow\nstate: [signal-fleet/results, notes]\nrepo: {name: example/shop, branch: main}\n")
	write(t, d, "triage/cycle/v1/spec.yaml", folderVersion)
	write(t, d, "triage/cycle/v1/system.md", "x\n")
	rs, err := Load(d)
	if err != nil || len(rs) != 1 {
		t.Fatalf("%v %v", rs, err)
	}
	j := rs[0]
	if j.ShadowWorkerName() != "triage-shadow" || len(j.State) != 2 || j.State[0] != "signal-fleet/results" || j.Repo.Name != "example/shop" || j.Repo.Branch != "main" {
		t.Fatalf("%+v", j)
	}
	if got := (&Routine{Worker: "triage"}).ShadowWorkerName(); got != "triage-shadow" {
		t.Fatalf("the default shadow worker is <worker>-shadow: %q", got)
	}
}

func TestShadowKeysAreChecked(t *testing.T) {
	for name, c := range map[string]struct{ extra, want string }{
		"a shadow worker name":      {"shadow_worker: Bad_Name\n", "shadow_worker"},
		"a state path out of HOME":  {"state: [../x]\n", "state"},
		"an absolute state path":    {"state: [/etc]\n", "state"},
		"a trailing slash":          {"state: [a/]\n", "state"},
		"an empty state path":       {"state: ['']\n", "state"},
		"a state path twice":        {"state: [a, a]\n", "listed twice"},
		"a repo that is not o/n":    {"repo: {name: shop, branch: main}\n", "repo.name"},
		"a repo with no branch":     {"repo: {name: example/shop}\n", "repo.branch"},
		"a branch that climbs":      {"repo: {name: example/shop, branch: a/../b}\n", "repo.branch"},
		"an unknown key under repo": {"repo: {name: example/shop, branch: main, ref: x}\n", "ref"},
	} {
		t.Run(name, func(t *testing.T) {
			d := t.TempDir()
			write(t, d, "triage/cycle/routine.yaml", folderRoutine+c.extra)
			write(t, d, "triage/cycle/v1/spec.yaml", folderVersion)
			write(t, d, "triage/cycle/v1/system.md", "x\n")
			if _, err := Load(d); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want an error with %q, got %v", c.want, err)
			}
		})
	}
	// a version's spec.yaml cannot carry them
	d := t.TempDir()
	write(t, d, "triage/cycle/routine.yaml", folderRoutine)
	write(t, d, "triage/cycle/v1/spec.yaml", folderVersion+"state: [a]\n")
	write(t, d, "triage/cycle/v1/system.md", "x\n")
	rs, err := Load(d)
	if err == nil && (len(rs) == 0 || rs[0].Versions[0].Err == nil) {
		t.Fatal("a version's spec.yaml took a routine's key")
	}
}
