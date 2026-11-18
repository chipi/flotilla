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
// code at main@a9dc277; a legitimate edit of a spec or of a prompt it declares changes its line.
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
	got := map[string]string{}
	for _, d := range []string{filepath.Join("..", "..", "fleets"), filepath.Join("..", "..", "deploy", "e2e", "fleets")} {
		routines, err := Load(d)
		if err != nil {
			t.Fatal(err)
		}
		for _, j := range routines {
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
}

// The legacy v1's hash recipe is pinned on a fixed spec, so a new field of Version cannot move every
// recorded hash without a test saying so.
func TestLegacyVersionHashRecipe(t *testing.T) {
	d := t.TempDir()
	write(t, d, "triage/cycle.yaml", good)
	routines, err := Load(d)
	if err != nil {
		t.Fatal(err)
	}
	const want = "634db2c3804da10a59bd2873d4042ab92dd2e8d26611f86280fb24f3d816debc"
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
