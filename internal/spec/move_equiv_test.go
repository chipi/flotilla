package spec

import (
	"encoding/json"
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

// routineFields is the routine with every field a version owns taken out: what routine.yaml holds.
func routineFields(t *testing.T, j *Routine) Routine {
	t.Helper()
	c := movedRoutine(t, j, 1)
	c.Run, c.Explain, c.Steps, c.Skills, c.Harness, c.Prompts, c.Models, c.PromptSHA, c.Limit = Run{}, nil, nil, Skills{}, nil, nil, nil, nil, 0
	c.Stations, c.StationText, c.VersionLimits = nil, nil, nil
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
	// laterDescriptions are routines whose description was reworded after the move; routine.yaml is outside the
	// version folder, so no version changes with it. Every other field must still equal the flat spec.
	laterDescriptions = map[string]string{"platform/model-contract": "2026-11-24: its unknown-model check reads the model list instead of calling the gateway with a fake model"}
	stepFiveRemoved   = map[string]string{"triage/cycle-shadow": "now v2 of triage/cycle, a candidate version (TestTriageCycleIsAStationVersionOfTheSameRoutine)"}
	stepFiveAdded     = map[string]string{
		// RFC-0014 §4.6 (step 2): plumbing.call's execute routines and its check, new routines, not moved ones
		"plumbing/send-create-tasks": "RFC-0014 step 2", "plumbing/send-post-decisions": "RFC-0014 step 2", "plumbing/send-reply": "RFC-0014 step 2",
		"plumbing/send-post": "RFC-0014 step 2", "plumbing/send-retract": "RFC-0014 step 2", "plumbing/check": "RFC-0014 step 2",
		// RFC-0014 §4.5 (step 3): case 3, a meeting into its follow-ups and decisions
		"meetings/notes": "RFC-0014 step 3", "meetings/decisions": "RFC-0014 step 3",
		"knowledge/answer": "RFC-0014 case 1",
	}
	stepFiveVersion = map[string]int{"triage/cycle": 2, "platform/heartbeat": 2} // routine id -> the first version number step 5 added to it
	// stepFiveStations are routines whose v1 became a station version on purpose: the routine's own fields must still be the flat spec's,
	// and the version body is proven by TestTriageCycleIsAStationVersionOfTheSameRoutine and the request-equivalence test.
	stepFiveStations = map[string]string{"triage/cycle": "v1 declares its decide station"}
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
		if stepFiveStations[id] != "" {
			if got := n.State; len(got) != 1 || got[0] != "signal-fleet/results" {
				t.Errorf("%s: its state is %v: a shadow version starts from the cycle's results folder", id, got)
			}
			nr := routineFields(t, n)
			nr.State = nil
			if diff := differingFields(nr, routineFields(t, o)); len(diff) > 0 {
				t.Errorf("%s: the routine's own fields differ from its flat spec: %s", id, strings.Join(diff, "; "))
			}
			continue
		}
		for _, ov := range o.Versions {
			mn, mo := movedRoutine(t, n, ov.Number), movedRoutine(t, o, ov.Number)
			if laterDescriptions[id] != "" {
				mn.Description, mo.Description = "", ""
			}
			if diff := differingFields(mn, mo); len(diff) > 0 {
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
			out = append(out, fmt.Sprintf("%s is %s, flat %s", va.Type().Field(i).Name, short(va.Field(i).Interface()), short(vb.Field(i).Interface())))
		}
	}
	return out
}

func short(v any) string {
	s := fmt.Sprintf("%v", v)
	if len(s) > 160 {
		s = s[:160] + "..."
	}
	return s
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

// triage/cycle is the one example whose v1 changed shape on purpose: its model step moved from a harness: block, prompts: and SF_TRIAGE_MODEL
// to the decide station. Its identity, its skill rules and its run (but the entry point) are the flat spec's, and the station says what the
// harness block said, which is also what the standalone default (fleets/triage/harness-default.json, read without flotillad) says.
func TestTriageCycleIsAStationVersionOfTheSameRoutine(t *testing.T) {
	old := loadByID(t, flatBeforeStep5)["triage/cycle"]
	now := loadByID(t, filepath.Join("..", "..", "fleets"))["triage/cycle"]
	if old == nil || now == nil {
		t.Fatal("triage/cycle missing")
	}
	flat, _ := old.WithVersion(1)
	v1, err := now.WithVersion(1)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(v1.Skills, flat.Skills) {
		t.Errorf("v1's skills.rule is not the flat spec's: %+v, was %+v", v1.Skills, flat.Skills)
	}
	want := flat.Run
	want.Command = strings.Replace(want.Command, "orchestrator.py", "orchestrator_station.py", 1)
	if v1.Run != want {
		t.Errorf("v1's run is %+v, want the flat spec's with orchestrator_station.py: %+v", v1.Run, want)
	}
	raw, err := os.ReadFile(filepath.Join("..", "..", "fleets", "triage", "harness-default.json"))
	if err != nil {
		t.Fatal(err)
	}
	var def struct {
		Tools  []string `json:"tools"`
		Gates  []string `json:"gates"`
		Limits struct {
			MaxToolCalls int `json:"max_tool_calls"`
			MaxSteps     int `json:"max_steps"`
		} `json:"limits"`
		Quirks map[string]any `json:"quirks"`
		Hooks  *HarnessHooks  `json:"hooks"`
	}
	if err := json.Unmarshal(raw, &def); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name  string
		r     *Routine
		model string
	}{{"v1", v1, "triage-flash"}, {"v2", mustVersion(t, now, 2), "triage-pro"}} {
		st := c.r.Stations["decide"]
		if st == nil || len(c.r.Stations) != 1 {
			t.Fatalf("%s: want one station, decide; got %v", c.name, c.r.StationNames())
		}
		if st.Harness != "flotilla" || st.Model != c.model || st.Tools != "all" {
			t.Errorf("%s decide: harness %s model %s tools %s", c.name, st.Harness, st.Model, st.Tools)
		}
		if !reflect.DeepEqual(*st.ToolNames, def.Tools) || !reflect.DeepEqual(*st.Gates, def.Gates) || !reflect.DeepEqual(st.Hooks, def.Hooks) {
			t.Errorf("%s decide: tool_names, gates or hooks differ from harness-default.json", c.name)
		}
		if st.Limits.MaxTurns != def.Limits.MaxSteps || st.Limits.MaxTurns != def.Limits.MaxToolCalls+1 {
			t.Errorf("%s decide: max_turns %d, harness-default.json max_steps %d max_tool_calls %d", c.name, st.Limits.MaxTurns, def.Limits.MaxSteps, def.Limits.MaxToolCalls)
		}
		if !reflect.DeepEqual(st.Skills, []string{"hint-external-transient", "hint-external", "hint-environment", "hint-recoverable"}) {
			t.Errorf("%s decide: skills %v", c.name, st.Skills)
		}
		for _, k := range []string{"reply_echo_chars", "decision_echo_chars", "result_chars"} {
			if got, want := fmt.Sprint(c.r.VersionLimits[k]), fmt.Sprint(def.Quirks[k]); got != want {
				t.Errorf("%s limits.%s is %s, harness-default.json says %s", c.name, k, got, want)
			}
		}
		if c.r.VersionLimits["call_timeout_s"] != 90 {
			t.Errorf("%s limits.call_timeout_s is %v: the call timeout the first fleet's loop has is 90", c.name, c.r.VersionLimits["call_timeout_s"])
		}
	}
	v2 := mustVersion(t, now, 2)
	a, _ := os.ReadFile(filepath.Join(now.Versions[0].Dir, "system.md"))
	b, _ := os.ReadFile(filepath.Join(now.VersionNamed(2).Dir, "system.md"))
	if len(a) == 0 || string(a) != string(b) {
		t.Error("v2's system.md is not v1's (the candidate differs in its model alone)")
	}
	if v2.Run.Command != "python3 orchestrator_station.py --glitchtip --limit 2" {
		t.Errorf("v2's command %q: the candidate cycle-shadow ran errors only, 2 triages a cycle", v2.Run.Command)
	}
	if got := old.Routine; got != "cycle" {
		t.Fatal(got)
	}
}

func mustVersion(t *testing.T, j *Routine, n int) *Routine {
	t.Helper()
	v, err := j.WithVersion(n)
	if err != nil {
		t.Fatal(err)
	}
	return v
}
