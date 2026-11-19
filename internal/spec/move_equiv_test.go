package spec

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// flatBeforeStep5 is every shipped example fleet as it was before step 5 moved the flat routines into folders
// (testdata/flat-before-step5, copied verbatim from HEAD at ca4ef36, with the files the specs read).
var flatBeforeStep5 = filepath.Join("testdata", "flat-before-step5")

// movedRoutine is the routine with what a move may legitimately change taken out: where it was loaded from, its
// recorded hashes, the version list and the notes about the old autonomy word. A prompt's path is written relative
// to the fleet's folder, so it names the file's new place; its role and its content hash must be the same.
func movedRoutine(t *testing.T, j *Routine, version int) Routine {
	t.Helper()
	vj, err := j.WithVersion(version)
	if err != nil {
		t.Fatal(err)
	}
	c := *vj
	c.Path, c.Hash, c.VersionHash, c.Versions, c.Version, c.Notes = "", "", "", nil, 0, nil
	if c.Prompts != nil {
		p := map[string]string{}
		for role, f := range c.Prompts {
			p[role] = path.Base(f)
		}
		c.Prompts = p
	}
	return c
}

func loadByID(t *testing.T, dir string) map[string]*Routine {
	t.Helper()
	rs, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]*Routine{}
	for _, j := range rs {
		m[j.ID()] = j
	}
	return m
}

// stepFiveChanges is what step 5 changed on purpose, so the test does not hide it: a routine that was folded into
// another, and a version a routine gained. Each is named with its reason; anything else that differs fails.
var (
	stepFiveRemoved = map[string]string{}
	stepFiveAdded   = map[string]string{}
	stepFiveVersion = map[string]int{} // routine id -> the first version number step 5 added to it
)

// Every example routine that moved to the folder layout is the same routine: each field of the Routine, of its
// version's body and of the prompts' hashes is equal to what its flat spec had. A routine that did not move is
// byte-for-byte what it was. Only the hashes, the manifest and the legacy mark may differ (ADR-0020 section 3:
// the one re-record a move costs).
func TestMovedRoutinesAreEquivalentToTheirFlatSpecs(t *testing.T) {
	old := loadByID(t, flatBeforeStep5)
	now := loadByID(t, filepath.Join("..", "..", "fleets"))
	if len(old) < 30 {
		t.Fatalf("only %d routines in the pre-step-5 copy", len(old))
	}
	var ids []string
	for id := range old {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	moved := 0
	for _, id := range ids {
		o, n := old[id], now[id]
		if n == nil {
			if stepFiveRemoved[id] == "" {
				t.Errorf("%s: gone, and not named in stepFiveRemoved", id)
			}
			continue
		}
		if on, nn := versionNumbers(o), versionNumbers(n); len(nn) < len(on) || !reflect.DeepEqual(on, nn[:len(on)]) {
			t.Errorf("%s: versions %v became %v", id, on, nn)
		}
		if o.Versions[0].Legacy && n.Versions[0].Legacy {
			a, _ := os.ReadFile(o.Path)
			b, _ := os.ReadFile(n.Path)
			if string(a) != string(b) {
				t.Errorf("%s: still a flat spec, but its bytes changed (its hash is a golden)", id)
			}
			continue
		}
		if o.Versions[0].Legacy {
			moved++
		}
		for _, ov := range o.Versions {
			if diff := differingFields(movedRoutine(t, n, ov.Number), movedRoutine(t, o, ov.Number)); len(diff) > 0 {
				t.Errorf("%s v%d: the moved routine is not equal to its flat spec: %s", id, ov.Number, strings.Join(diff, "; "))
			}
			nv := n.VersionNamed(ov.Number)
			if nv == nil || nv.Legacy {
				t.Errorf("%s v%d: version missing or still legacy", id, ov.Number)
			} else if !reflect.DeepEqual(promptHashes(nv), promptHashes(ov)) {
				t.Errorf("%s v%d: prompt content changed: %v, was %v", id, ov.Number, promptHashes(nv), promptHashes(ov))
			}
		}
		if len(n.Notes) > 0 {
			t.Errorf("%s: a folder routine still says %v", id, n.Notes)
		}
	}
	for id := range now {
		if old[id] == nil && stepFiveAdded[id] == "" {
			t.Errorf("%s: new, and not named in stepFiveAdded", id)
		}
	}
	for id, first := range stepFiveVersion {
		if now[id] == nil || now[id].VersionNamed(first) == nil {
			t.Errorf("%s: stepFiveVersion says v%d was added; it is not there", id, first)
		}
	}
	if moved == 0 {
		t.Fatal("no routine moved: the test compares nothing")
	}
	t.Logf("%d routines moved from a flat spec to a folder, all equal", moved)
}

// differingFields names each field of two routines that are not deeply equal, with both values.
func differingFields(a, b Routine) []string {
	var out []string
	va, vb := reflect.ValueOf(a), reflect.ValueOf(b)
	for i := 0; i < va.NumField(); i++ {
		if !reflect.DeepEqual(va.Field(i).Interface(), vb.Field(i).Interface()) {
			out = append(out, fmt.Sprintf("%s is %v, flat %v", va.Type().Field(i).Name, va.Field(i).Interface(), vb.Field(i).Interface()))
		}
	}
	return out
}

func versionNumbers(j *Routine) []int {
	var out []int
	for _, v := range j.Versions {
		out = append(out, v.Number)
	}
	return out
}

// promptHashes is the content hash of each prompt file a version declares, by role.
func promptHashes(v *Version) map[string]string {
	m := map[string]string{}
	for k, s := range v.PromptSHA {
		m[k] = s
	}
	return m
}

// Test the harness itself against a hand-made pair, so a comparison that compares nothing is caught here and not by a
// moved routine that happens to be equal.
func TestMoveEquivalenceNoticesADifference(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	flat := "fleet: jobs\nroutine: r\ndescription: d\nenabled: true\nworker: w\ntrigger: {every: 10m}\nrun:\n  command: echo\n  workdir: .\n  timeout: 1m\nautonomy: record\nbudget:\n  day_usd: 0\nfreshness: 25m\n"
	write(t, a, "jobs/r.yaml", flat)
	write(t, b, "jobs/r/routine.yaml", "fleet: jobs\nroutine: r\ndescription: d\nenabled: true\nworker: w\ntrigger: {every: 10m}\nautonomy: record\nbudget:\n  day_usd: 0\nfreshness: 25m\n")
	write(t, b, "jobs/r/v1/spec.yaml", "run:\n  command: echo\n  workdir: .\n  timeout: 1m\n")
	oa, ob := loadByID(t, a)["jobs/r"], loadByID(t, b)["jobs/r"]
	if diff := differingFields(movedRoutine(t, oa, 1), movedRoutine(t, ob, 1)); len(diff) > 0 {
		t.Fatalf("the same routine in both layouts compares unequal: %v", diff)
	}
	write(t, b, "jobs/r/routine.yaml", strings.Replace("fleet: jobs\nroutine: r\ndescription: d\nenabled: true\nworker: w\ntrigger: {every: 10m}\nautonomy: record\nbudget:\n  day_usd: 0\nfreshness: 25m\n", "day_usd: 0", "day_usd: 1", 1))
	ob = loadByID(t, b)["jobs/r"]
	if diff := differingFields(movedRoutine(t, oa, 1), movedRoutine(t, ob, 1)); len(diff) != 1 || !strings.HasPrefix(diff[0], "Budget ") {
		t.Fatalf("a changed budget must differ in Budget alone: %v", diff)
	}
}
