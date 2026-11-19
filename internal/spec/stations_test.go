package spec

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chipi/flotilla/fleets"
)

const stationsGood = `run: {command: "python3 x.py", workdir: signal-fleet/mvp, timeout: 20m}
stations:
  decide:
    harness: pi
    model: fix-flash
    prompts: {task: task.md, system: system.md}
    tools: read
    limits: {max_turns: 20, timeout: 15m, max_output_tokens: 16000}
  plan:
    harness: flotilla
    model: plan-flash
    prompts: {task: task.md}
    tools: all
    tool_names: [probe]
    gates: []
limits: {max_probes: 3}
`

func stationsTree(t *testing.T, version string) string {
	t.Helper()
	d := t.TempDir()
	write(t, d, "triage/cycle/routine.yaml", folderRoutine)
	write(t, d, "triage/cycle/v1/spec.yaml", version)
	write(t, d, "triage/cycle/v1/task.md", "the task {{x}}\n")
	write(t, d, "triage/cycle/v1/system.md", "the system text\n")
	return d
}

func TestStationsLoadAndDeriveTheModels(t *testing.T) {
	rs, err := Load(stationsTree(t, stationsGood))
	if err != nil {
		t.Fatal(err)
	}
	j := rs[0]
	if got := strings.Join(j.Models, ","); got != "fix-flash,plan-flash" {
		t.Fatalf("models are derived from the stations, sorted: %q", got)
	}
	if got := strings.Join(j.StationNames(), ","); got != "decide,plan" {
		t.Fatalf("stations %q", got)
	}
	tx := j.StationText["decide"]["task"]
	if tx.Path != "cycle/v1/task.md" || len(tx.SHA256) != 64 {
		t.Fatalf("text %+v", tx)
	}
	if j.VersionLimits["max_probes"] != 3 || j.Harness != nil || len(j.Prompts) != 0 {
		t.Fatalf("limits %v harness %v prompts %v", j.VersionLimits, j.Harness, j.Prompts)
	}
	// the texts are in the version's hash: a changed text is a changed version
	d := stationsTree(t, stationsGood)
	write(t, d, "triage/cycle/v1/task.md", "another task\n")
	rs2, err := Load(d)
	if err != nil || rs2[0].VersionHash == j.VersionHash {
		t.Fatalf("the hash covers the station texts: %v", err)
	}
}

func TestStationsAreRefused(t *testing.T) {
	cases := map[string]struct{ version, want string }{
		"top-level harness":    {stationsGood + "harness: {protocol: x}\n", "no harness:, prompts: or models:"},
		"top-level prompts":    {stationsGood + "prompts: {system: system.md}\n", "no harness:, prompts: or models:"},
		"models declared too":  {stationsGood + "models: [a]\n", "no harness:, prompts: or models:"},
		"unknown harness":      {strings.Replace(stationsGood, "harness: pi", "harness: codexx", 1), "not a harness this flotillad knows"},
		"bad tools":            {strings.Replace(stationsGood, "tools: read", "tools: some", 1), "all, read or none"},
		"read on flotilla":     {strings.Replace(stationsGood, "tools: all\n    tool_names", "tools: read\n    tool_names", 1), "does not serve tools: read"},
		"hooks on pi":          {strings.Replace(stationsGood, "    tools: read\n", "    tools: read\n    hooks: {before_tool: [{name: h}]}\n", 1), "belong to harness flotilla only"},
		"flotilla w/o names":   {strings.Replace(stationsGood, "    tool_names: [probe]\n    gates: []\n", "", 1), "needs tool_names and gates"},
		"station name":         {strings.Replace(stationsGood, "  decide:", "  Decide-It:", 1), "station name"},
		"text with a path out": {strings.Replace(stationsGood, "task: task.md, system", "task: ../x.md, system", 1), "clean .md or .txt path"},
		"bad timeout":          {strings.Replace(stationsGood, "timeout: 15m", "timeout: soon", 1), "positive duration"},
		"no model":             {strings.Replace(stationsGood, "    model: fix-flash\n", "", 1), "alias of the installation"},
		"limits w/o stations":  {"run: {command: x, workdir: w, timeout: 1m}\nlimits: {a: 1}\n", "only a version with stations"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Load(stationsTree(t, c.version))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want %q, got %v", c.want, err)
			}
		})
	}
}

func TestAFlatSpecRefusesStations(t *testing.T) {
	d := t.TempDir()
	write(t, d, "triage/cycle.yaml", good+"stations:\n  a: {harness: pi, model: m, tools: none}\n")
	if _, err := Load(d); err == nil || !strings.Contains(err.Error(), "stations") {
		t.Fatalf("got %v", err)
	}
}

func TestResumeNeedsAHarnessThatCanContinueAKilledSession(t *testing.T) {
	d := stationsTree(t, `run: {command: x, workdir: w, timeout: 1m}
stations:
  fix: {harness: flotilla, model: m, tools: all, tool_names: [], gates: []}
`)
	write(t, d, "triage/cycle/routine.yaml", strings.Replace(folderRoutine, "trigger: {every: 10m}\n", "trigger: {case: {kind: code_bug}}\nresume: {on: [interrupted], max: 1}\n", 1))
	if _, err := Load(d); err == nil || !strings.Contains(err.Error(), "cannot continue a killed session") {
		t.Fatalf("got %v", err)
	}
	write(t, d, "triage/cycle/v1/spec.yaml", "run: {command: x, workdir: w, timeout: 1m}\nstations:\n  fix: {harness: pi, model: m, tools: all}\n")
	if _, err := Load(d); err != nil {
		t.Fatalf("pi resumes after a kill: %v", err)
	}
}

// fleets/harnesses.json on disk is the copy flotillad embeds: a mounted fleets folder cannot shadow it, and a stale embed is a failure.
func TestTheEmbeddedHarnessTableIsTheFileOnDisk(t *testing.T) {
	disk, err := os.ReadFile(filepath.Join("..", "..", "fleets", "harnesses.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(disk) != string(fleets.HarnessesJSON) {
		t.Fatal("the embedded harnesses.json differs from fleets/harnesses.json")
	}
	tab, err := Harnesses()
	if err != nil || !tab["pi"].serves("read") || tab["flotilla"].serves("read") || !tab["flotilla"].SupportsHooks || tab["pi"].SupportsHooks {
		t.Fatalf("table %+v err %v", tab, err)
	}
	var raw map[string]any
	if json.Unmarshal(disk, &raw) != nil {
		t.Fatal("not JSON")
	}
}

func TestNoShippedRoutineDeclaresStations(t *testing.T) {
	for _, d := range []string{filepath.Join("..", "..", "fleets"), filepath.Join("..", "..", "deploy", "e2e", "fleets")} {
		rs, err := Load(d)
		if err != nil {
			t.Fatal(err)
		}
		for _, j := range rs {
			for _, v := range j.Versions {
				if len(v.Stations) > 0 {
					t.Errorf("%s v%d declares stations: step 5 moves the examples, not step 3", j.ID(), v.Number)
				}
			}
		}
	}
}

func TestStationsIsNotALegacyOwnedKey(t *testing.T) {
	for _, k := range legacyOwned {
		if k == "stations" || k == "limits" {
			t.Fatalf("%s in legacyOwned would move every recorded flat hash", k)
		}
	}
}

func TestHooksOnAHarnessWithoutThemAreRefused(t *testing.T) {
	tab, err := Harnesses()
	if err != nil {
		t.Fatal(err)
	}
	saved := tab["flotilla"]
	c := saved
	c.SupportsHooks = false
	tab["flotilla"] = c
	defer func() { tab["flotilla"] = saved }()
	v := strings.Replace(stationsGood, "    gates: []\n", "    gates: []\n    hooks: {before_tool: [{name: h}]}\n", 1)
	if _, err := Load(stationsTree(t, v)); err == nil || !strings.Contains(err.Error(), "has no hooks") {
		t.Fatalf("got %v", err)
	}
	tab["flotilla"] = saved
	if _, err := Load(stationsTree(t, v)); err != nil {
		t.Fatalf("hooks on a harness that has them: %v", err)
	}
}
