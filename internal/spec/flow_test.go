package spec

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// repoBlocks is the library the repo ships.
var repoBlocks = filepath.Join("..", "..", "blocks")

func put(t *testing.T, root, rel, body string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// actionsStub is a registry with backup.rerun, as host/registry.json declares it.
func actionsStub(name string) (ActionInfo, bool) {
	switch name {
	case "backup.rerun":
		return ActionInfo{Pre: []string{"backup_age_above(26h)"}, Verify: "backup_newer_than_execution", ForKinds: []string{"failed_backup"}}, true
	case "no.precondition":
		return ActionInfo{Verify: "x", ForKinds: []string{"failed_backup"}}, true
	case "disk.cleanup":
		return ActionInfo{Pre: []string{"x"}, ForKinds: []string{"disk_low"}}, true
	case "disk.cleanup_known":
		return ActionInfo{Pre: []string{"free_ratio_below(0.15)"}, Verify: "disk_free_above_threshold", ForKinds: []string{"disk_low"}}, true
	case "dgx.gpu_mode_prod":
		return ActionInfo{Pre: []string{"service_down(model-prod)"}, Verify: "service_up_again", ForKinds: []string{"dgx_not_serving"}}, true
	}
	return ActionInfo{}, false
}

const goodBlock = `block: probe
version: 1
role: monitor
description: Reads a thing.
consumes: {every: true}
emits: [detection]
hands_to: []
actions: {precondition_of: action}
params:
  action: {kind: registry_action, env: THE_ACTION}
  metric: {kind: metric_name, env: THE_METRIC}
  skip: {kind: label_match, env: THE_SKIP, optional: true}
env: [VM_URL]
model: none
max_autonomy: record
timeout: 1m
command: python3 /opt/flotilla/ops/probe.py
code: [ops/probe.py]
`

func TestLoadBlockAcceptsAGoodBlock(t *testing.T) {
	root := t.TempDir()
	put(t, root, "probe/v1/block.yaml", goodBlock)
	b, err := LoadBlock(filepath.Join(root, "probe", "v1"))
	if err != nil {
		t.Fatal(err)
	}
	if b.Role != RoleMonitor || len(b.Params) != 3 || b.Params[0].Name != "action" || b.Params[2].Name != "skip" {
		t.Fatalf("params must keep their declared order: %+v", b.Params)
	}
}

func TestLoadBlockRefusals(t *testing.T) {
	cases := []struct{ name, edit, with, want string }{
		{"unknown field", "role: monitor", "role: monitor\nsurprise: 1", "field surprise not found"},
		{"bad role", "role: monitor", "role: planner", `role "planner"`},
		{"role and consumes disagree", "consumes: {every: true}", "consumes: {case: true}", "monitor block consumes {every: true}"},
		{"stage without a stage", "role: monitor\ndescription: Reads a thing.\nconsumes: {every: true}", "role: stage\ndescription: Reads a thing.\nconsumes: {case: true}", "stage block consumes"},
		{"unknown emit", "emits: [detection]", "emits: [detection, magic]", `emits "magic"`},
		{"hands_to without hand_over", "hands_to: []", "hands_to: [investigate]", "does not emit hand_over"},
		{"unknown kind", "metric: {kind: metric_name,", "metric: {kind: telepathy,", `kind "telepathy"`},
		{"env and arg both", "metric: {kind: metric_name, env: THE_METRIC}", "metric: {kind: metric_name, env: THE_METRIC, arg: --metric}", "exactly one of env and arg"},
		{"neither env nor arg", "metric: {kind: metric_name, env: THE_METRIC}", "metric: {kind: metric_name}", "exactly one of env and arg"},
		{"actions names a non-action param", "actions: {precondition_of: action}", "actions: {precondition_of: metric}", "not a param of kind registry_action"},
		{"actions names an unknown need", "actions: {precondition_of: action}", "actions: {frobnicate: action}", `actions "frobnicate"`},
		{"bad model", "model: none", "model: gpt", `model "gpt"`},
		{"station without a tail", "model: none", "model: station", "ships its stations"},
		{"cap missing", "max_autonomy: record", "max_autonomy: sometimes", `max_autonomy "sometimes"`},
		{"start above the cap", "max_autonomy: record", "max_autonomy: record\nautonomy: auto", "at or below max_autonomy"},
		{"no timeout", "timeout: 1m\n", "", "timeout is required"},
		{"command outside the image's paths", "python3 /opt/flotilla/ops/probe.py", "python3 /tmp/probe.py", "the path the workers carry the code at"},
		{"no code", "code: [ops/probe.py]\n", "", "the repo files the block runs"},
		{"missing description", "description: Reads a thing.\n", "", "description is required"},
		{"folder and name disagree", "block: probe", "block: other", `block "other", but the folder is under probe/`},
		{"folder and version disagree", "version: 1", "version: 2", "version 2, but the folder is v1"},
		{"missing text", "code: [ops/probe.py]", "code: [ops/probe.py]\ntexts: [nope.md]", "nope.md"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := t.TempDir()
			if !strings.Contains(goodBlock, c.edit) {
				t.Fatalf("test bug: %q is not in the block", c.edit)
			}
			put(t, root, "probe/v1/block.yaml", strings.Replace(goodBlock, c.edit, c.with, 1))
			_, err := LoadBlock(filepath.Join(root, "probe", "v1"))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want an error with %q, got %v", c.want, err)
			}
		})
	}
}

func TestSpecTailIsChecked(t *testing.T) {
	for name, c := range map[string]struct{ model, tail, want string }{
		"run belongs to the generator": {"none", "run: {command: x}\n", "run is the generator's"},
		"a station names $MODEL":       {"station", "stations: {s: {model: fixed}}\n", "names $MODEL"},
		"none has no $MODEL":           {"none", "explain: {run: $MODEL}\n", "has no model"},
		"must parse":                   {"none", "explain: [\n", "spec_tail"},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			put(t, root, "probe/v1/block.yaml", strings.Replace(strings.Replace(goodBlock, "model: none", "model: "+c.model, 1), "code:", "spec_tail: tail.yaml\ncode:", 1))
			put(t, root, "probe/v1/tail.yaml", c.tail)
			if _, err := LoadBlock(filepath.Join(root, "probe", "v1")); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want %q, got %v", c.want, err)
			}
		})
	}
}

func TestParseUse(t *testing.T) {
	r, err := ParseUse("metric-monitor@3")
	if err != nil || r.Name != "metric-monitor" || r.Version != 3 || r.String() != "metric-monitor@3" {
		t.Fatalf("%+v %v", r, err)
	}
	for _, bad := range []string{"metric-monitor", "metric-monitor@0", "metric-monitor@", "@1", "Metric@1", "a@1.5", "a@latest", "a@-1"} {
		if _, err := ParseUse(bad); err == nil {
			t.Errorf("%q must be refused: a flow pins its block to a number", bad)
		}
	}
}

// fixture is a fleet with one flow over three throwaway library blocks: a monitor, an act that hands to investigate, an
// investigate stage and a verify.
func fixture(t *testing.T) (fleetDir, lib string) {
	t.Helper()
	root := t.TempDir()
	lib = filepath.Join(root, "blocks")
	fleetDir = filepath.Join(root, "fleets", "demo")
	put(t, lib, "probe/v1/block.yaml", goodBlock)
	put(t, lib, "check/v1/block.yaml", `block: check
version: 1
role: verify
description: Checks a thing.
consumes: {check: true}
emits: [verified]
hands_to: []
actions: {check_of: action}
params:
  action: {kind: registry_action, env: THE_ACTION}
env: []
model: none
max_autonomy: record
timeout: 1m
command: python3 /opt/flotilla/ops/check.py
code: [ops/check.py]
`)
	put(t, lib, "doer/v1/block.yaml", `block: doer
version: 1
role: act
description: Acts.
consumes: {case: true}
emits: [plan, hand_over]
hands_to: [investigate]
actions: {}
params: {}
env: []
model: none
max_autonomy: auto
timeout: 1m
command: python3 /opt/flotilla/ops/doer.py --fleet {{fleet}}
code: [ops/doer.py]
`)
	put(t, lib, "thinker/v1/block.yaml", `block: thinker
version: 1
role: stage
description: Thinks.
consumes: {case: true, stage: investigate}
emits: [plan]
hands_to: []
actions: {}
params: {}
env: []
model: station
max_autonomy: propose
autonomy: propose
budget_usd: 1
timeout: 3m
command: python3 /opt/flotilla/ops/thinker.py --fleet {{fleet}}
code: [ops/thinker.py]
texts: [system.md]
spec_tail: spec.tail.yaml
`)
	put(t, lib, "thinker/v1/system.md", "You think.\n")
	put(t, lib, "thinker/v1/spec.tail.yaml", "stations:\n  think:\n    model: $MODEL\n    prompts: {system: system.md}\n")
	put(t, fleetDir, "fleet.yaml", "fleet: demo\ndescription: A demo.\n")
	put(t, fleetDir, "flows/chain/flow.yaml", goodFlow)
	return fleetDir, lib
}

const goodFlow = `flow: chain
case_kind: failed_backup
steps:
  - name: chain-monitor
    use: probe@1
    description: |-
      Watches the thing.
      Second line.
    worker: ops-read
    every: 10m
    freshness: 25m
    with: {action: backup.rerun, metric: some_metric}
  - name: chain-act
    use: doer@1
    worker: ops-act
    max_autonomy: record
    case: {on_record: watch}
  - name: chain-think
    use: thinker@1
    worker: triage
    model: flash
    case: {on_record: watch}
  - name: chain-verify
    use: check@1
    worker: ops-read
    verify: backup_newer_than_execution
    with: {action: backup.rerun}
`

func expand(t *testing.T, fleetDir, lib string) (*Expansion, error) {
	t.Helper()
	ff, err := LoadFlows(fleetDir)
	if err != nil || ff == nil {
		t.Fatalf("LoadFlows: %v %v", ff, err)
	}
	return ff.Expand(lib, actionsStub)
}

func TestExpandWritesOrdinaryRoutineFolders(t *testing.T) {
	fleetDir, lib := fixture(t)
	exp, err := expand(t, fleetDir, lib)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"chain-monitor/routine.yaml": "fleet: demo\nroutine: chain-monitor\ndescription: >-\n  Watches the thing.\n  Second line.\nenabled: true\nworker: ops-read\ntrigger:\n  every: 10m\ndetects: [failed_backup]\nautonomy: record\nbudget:\n  day_usd: 0\nfreshness: 25m\n",
		"chain-monitor/v1/spec.yaml": "run:\n  command: env THE_ACTION=backup.rerun THE_METRIC=some_metric python3 /opt/flotilla/ops/probe.py\n  workdir: .\n  timeout: 1m\n",
		"chain-act/routine.yaml":     "fleet: demo\nroutine: chain-act\ndescription: >-\n  Acts.\nenabled: true\nworker: ops-act\ntrigger:\n  case: {kind: failed_backup, on_record: watch}\nautonomy: record\nmax_autonomy: record\nbudget:\n  day_usd: 0\n",
		"chain-act/v1/spec.yaml":     "run:\n  command: python3 /opt/flotilla/ops/doer.py --fleet demo\n  workdir: .\n  timeout: 1m\n",
		"chain-think/routine.yaml":   "fleet: demo\nroutine: chain-think\ndescription: >-\n  Thinks.\nenabled: true\nworker: triage\ntrigger:\n  case: {kind: failed_backup, stage: investigate, on_record: watch}\nautonomy: propose\nmax_autonomy: propose\nbudget:\n  day_usd: 1\n",
		"chain-think/v1/spec.yaml":   "run:\n  command: python3 /opt/flotilla/ops/thinker.py --fleet demo\n  workdir: .\n  timeout: 3m\nstations:\n  think:\n    model: flash\n    prompts: {system: system.md}\n",
		"chain-think/v1/system.md":   "You think.\n",
		"chain-verify/routine.yaml":  "fleet: demo\nroutine: chain-verify\ndescription: >-\n  Checks a thing.\nenabled: true\nworker: ops-read\ntrigger:\n  check: {verify: backup_newer_than_execution}\nautonomy: record\nbudget:\n  day_usd: 0\n",
		"flows/.generated":           "# The routine folders the flows generate (RFC-0016). flotilla flow expand writes this file; do not edit it.\nchain-act\nchain-monitor\nchain-think\nchain-verify\n",
		"chain-verify/v1/spec.yaml":  "run:\n  command: env THE_ACTION=backup.rerun python3 /opt/flotilla/ops/check.py\n  workdir: .\n  timeout: 1m\n",
	}
	if len(exp.Files) != len(want) {
		t.Errorf("%d files, want %d", len(exp.Files), len(want))
	}
	for p, w := range want {
		if got := string(exp.Files[p]); got != w {
			t.Errorf("%s:\n--- got\n%s--- want\n%s", p, got, w)
		}
	}
}

func TestExpandedFoldersLoadAsOrdinaryRoutines(t *testing.T) {
	fleetDir, lib := fixture(t)
	exp, err := expand(t, fleetDir, lib)
	if err != nil {
		t.Fatal(err)
	}
	if err := exp.Write(fleetDir); err != nil {
		t.Fatal(err)
	}
	if d := exp.Drift(fleetDir); len(d) != 0 {
		t.Fatalf("a written expansion has no drift: %v", d)
	}
	rs, err := LoadVersions(filepath.Dir(fleetDir))
	if err != nil {
		t.Fatalf("the generated folders must load as routines: %v", err)
	}
	if len(rs) != 4 {
		t.Fatalf("%d routines", len(rs))
	}
}

func TestExpandRefusals(t *testing.T) {
	cases := []struct{ name, edit, with, want string }{
		{"a stage the act hands to has no step", "  - name: chain-think\n    use: thinker@1\n    worker: triage\n    model: flash\n    case: {on_record: watch}\n", "", `hands cases to stage "investigate"`},
		{"two acting steps for one kind and stage", "  - name: chain-verify", "  - name: chain-act-too\n    use: doer@1\n    worker: ops-act\n  - name: chain-verify", "already have an acting step"},
		{"a param of the wrong kind", "metric: some_metric", "metric: 'bad metric!'", "a metric name"},
		{"a count that is text", "every: 10m", "every: ten", `every "ten"`},
		{"a required param missing", "with: {action: backup.rerun, metric: some_metric}", "with: {action: backup.rerun}", "with metric: required"},
		{"a param the block does not have", "with: {action: backup.rerun, metric: some_metric}", "with: {action: backup.rerun, metric: some_metric, extra: 1}", "has no such param"},
		{"an action the registry lacks", "action: backup.rerun, metric", "action: backup.nothing, metric", "the registry has no action backup.nothing"},
		{"an action for another kind", "action: backup.rerun, metric", "action: disk.cleanup, metric", "not an action for failed_backup cases"},
		{"a precondition to read missing", "action: backup.rerun, metric", "action: no.precondition, metric", "declares no precondition"},
		{"a verify that is not the action's check", "verify: backup_newer_than_execution", "verify: something_else", `registry check is "backup_newer_than_execution"`},
		{"a cap raised", "    verify: backup_newer_than_execution\n", "    verify: backup_newer_than_execution\n    max_autonomy: propose\n", "raises check's cap, record"},
		{"a block cap raised", "    model: flash\n", "    model: flash\n    max_autonomy: auto\n", "raises thinker's cap, propose"},
		{"an autonomy above the step's cap", "max_autonomy: record\n", "max_autonomy: record\n    autonomy: auto\n", "above the step's cap"},
		{"a step with no model for a station", "    model: flash\n", "", "names the model alias"},
		{"a model on a block that has none", "    every: 10m\n", "    every: 10m\n    model: flash\n", "has no model"},
		{"a pin to a block that is not there", "use: doer@1", "use: doer@2", "no such block"},
		{"an unpinned use", "use: doer@1", "use: doer", "a block pinned to a version"},
		{"a verify step without its check", "    verify: backup_newer_than_execution\n", "", "verify"},
		{"a case trigger on a monitor", "    every: 10m\n", "    every: 10m\n    case: {on_record: watch}\n", "only an act or stage step has a case trigger"},
		{"an on_record that is neither", "case: {on_record: watch}\n  - name: chain-think", "case: {on_record: ignore}\n  - name: chain-think", "close or watch"},
		{"a monitor with no schedule", "    every: 10m\n", "", "a monitor step is scheduled"},
		{"a schedule on an act", "    worker: ops-act\n", "    worker: ops-act\n    every: 5m\n", "only a scheduled step has them"},
		{"a description with a blank line", "      Second line.", "\n      Third.", "line"},
		{"two steps with one name", "name: chain-act\n", "name: chain-monitor\n", "already a step of"},
		{"a bad worker", "worker: ops-act\n", "worker: ''\n", "worker"},
		{"a flow's kind that is not a kind", "case_kind: failed_backup", "case_kind: Failed!", "a case kind"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fleetDir, lib := fixture(t)
			if !strings.Contains(goodFlow, c.edit) {
				t.Fatalf("test bug: %q is not in the flow", c.edit)
			}
			put(t, fleetDir, "flows/chain/flow.yaml", strings.Replace(goodFlow, c.edit, c.with, 1))
			_, err := expand(t, fleetDir, lib)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want an error with %q, got %v", c.want, err)
			}
		})
	}
}

func TestExpandWithoutARegistrySkipsTheActionChecks(t *testing.T) {
	fleetDir, lib := fixture(t)
	put(t, fleetDir, "flows/chain/flow.yaml", strings.Replace(goodFlow, "action: backup.rerun, metric", "action: backup.nothing, metric", 1))
	ff, err := LoadFlows(fleetDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ff.Expand(lib, nil); err != nil {
		t.Fatalf("the registry's side of the wiring needs a registry: %v", err)
	}
}

func TestFlowFilesAreStrict(t *testing.T) {
	fleetDir, _ := fixture(t)
	put(t, fleetDir, "flows/chain/flow.yaml", goodFlow+"surprise: 1\n")
	if _, err := LoadFlows(fleetDir); err == nil || !strings.Contains(err.Error(), "surprise") {
		t.Fatalf("an unknown field is a typo, not a setting: %v", err)
	}
	fleetDir, _ = fixture(t)
	put(t, fleetDir, "flows/other/flow.yaml", goodFlow)
	if _, err := LoadFlows(fleetDir); err == nil || !strings.Contains(err.Error(), "but the file is under other/") {
		t.Fatalf("the flow's folder names it: %v", err)
	}
}

func TestAFlowsFolderOfNothingIsAnError(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "demo", "flows"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFlows(filepath.Join(root, "demo")); err == nil {
		t.Fatal("an empty flows folder is not a fleet without flows")
	}
	if ff, err := LoadFlows(filepath.Join(root, "demo-without")); ff != nil || err != nil {
		t.Fatalf("a fleet without a flows folder has none: %v %v", ff, err)
	}
}

func TestDriftNamesTheFileAndTheLine(t *testing.T) {
	fleetDir, lib := fixture(t)
	exp, err := expand(t, fleetDir, lib)
	if err != nil {
		t.Fatal(err)
	}
	if d := exp.Drift(fleetDir); len(d) != 10 {
		t.Fatalf("nothing is committed yet: every generated file is missing, got %v", d)
	}
	if err := exp.Write(fleetDir); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(fleetDir, "chain-act", "routine.yaml")
	raw, _ := os.ReadFile(p)
	if err := os.WriteFile(p, []byte(strings.Replace(string(raw), "worker: ops-act", "worker: ops-edited", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	put(t, fleetDir, "chain-act/v1/extra.md", "hand-written\n")
	if err := os.Remove(filepath.Join(fleetDir, "chain-verify", "v1", "spec.yaml")); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(exp.Drift(fleetDir), "\n")
	for _, want := range []string{
		`chain-act/routine.yaml: differs at line 6: committed "worker: ops-edited", generated "worker: ops-act"`,
		"chain-act/v1/extra.md: not generated",
		"chain-verify/v1/spec.yaml: missing",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("drift lacks %q:\n%s", want, got)
		}
	}
}

func TestCheckFlowsRefusesDriftAndNamesIt(t *testing.T) {
	fleetDir, lib := fixture(t)
	specs := filepath.Dir(fleetDir)
	if _, err := CheckFlows(specs, lib, actionsStub); err == nil || !strings.Contains(err.Error(), "differ from the flows' expansion") {
		t.Fatalf("an uncommitted expansion is drift: %v", err)
	}
	exp, _ := expand(t, fleetDir, lib)
	if err := exp.Write(fleetDir); err != nil {
		t.Fatal(err)
	}
	lines, err := CheckFlows(specs, lib, actionsStub)
	if err != nil || len(lines) != 1 || !strings.Contains(lines[0], "4 routines") {
		t.Fatalf("%v %v", lines, err)
	}
	// A block edited under its pin changes what the pin expands to: the committed folders no longer equal it.
	put(t, lib, "doer/v1/block.yaml", strings.Replace(mustRead(t, filepath.Join(lib, "doer/v1/block.yaml")), "timeout: 1m", "timeout: 2m", 1))
	if _, err := CheckFlows(specs, lib, actionsStub); err == nil || !strings.Contains(err.Error(), "chain-act/v1/spec.yaml: differs") {
		t.Fatalf("a block edited under a pinned version is drift: %v", err)
	}
}

func mustRead(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestPinResolutionPrefersTheFlowsOwnBlock(t *testing.T) {
	fleetDir, lib := fixture(t)
	put(t, fleetDir, "flows/chain/blocks/doer/v1/block.yaml", strings.Replace(mustRead(t, filepath.Join(lib, "doer/v1/block.yaml")), "doer.py", "local_doer.py", 1))
	exp, err := expand(t, fleetDir, lib)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(exp.Files["chain-act/v1/spec.yaml"]); !strings.Contains(got, "local_doer.py") {
		t.Fatalf("a flow's own block comes before the library's:\n%s", got)
	}
}

func TestPinnedVersionsAreSeparateFolders(t *testing.T) {
	fleetDir, lib := fixture(t)
	v2 := strings.NewReplacer("version: 1", "version: 2", "timeout: 1m", "timeout: 4m").Replace(mustRead(t, filepath.Join(lib, "doer/v1/block.yaml")))
	put(t, lib, "doer/v2/block.yaml", v2)
	exp, err := expand(t, fleetDir, lib)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(exp.Files["chain-act/v1/spec.yaml"]), "timeout: 1m") {
		t.Fatal("publishing a block version changes no flow: the pin is still @1")
	}
	put(t, fleetDir, "flows/chain/flow.yaml", strings.Replace(goodFlow, "use: doer@1\n    worker: ops-act", "use: doer@2\n    version: 2\n    worker: ops-act", 1))
	exp, err = expand(t, fleetDir, lib)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(exp.Files["chain-act/v2/spec.yaml"]), "timeout: 4m") || exp.Files["chain-act/v1/spec.yaml"] != nil {
		t.Fatal("a bumped pin with a bumped routine version writes the new folder and leaves v1 alone")
	}
}

func TestLearningBlocksAndFlowListParams(t *testing.T) {
	fleetDir, lib := fixture(t)
	put(t, lib, "drafter/v1/block.yaml", `block: drafter
version: 1
role: learn
description: Drafts.
consumes: {every: true}
emits: [detection]
hands_to: []
actions: {}
params:
  flows: {kind: flow_list, arg: --kinds}
env: []
model: none
max_autonomy: record
timeout: 2m
every: 1h
freshness: 3h
command: python3 /opt/flotilla/ops/drafter.py --fleet {{fleet}}
case_kind: sop_proposal
code: [ops/drafter.py]
`)
	put(t, fleetDir, "flows/learn.yaml", "learn:\n  - name: drafter\n    use: drafter@1\n    worker: ops-read\n    with: {flows: [chain]}\n")
	exp, err := expand(t, fleetDir, lib)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(exp.Files["drafter/v1/spec.yaml"]); !strings.Contains(got, "drafter.py --fleet demo --kinds failed_backup\n") {
		t.Fatalf("the kinds come from the flows named:\n%s", got)
	}
	if got := string(exp.Files["drafter/routine.yaml"]); !strings.Contains(got, "every: 1h") || !strings.Contains(got, "detects: [sop_proposal]") || !strings.Contains(got, "freshness: 3h") {
		t.Fatalf("a learning block takes its block's schedule:\n%s", got)
	}
	put(t, fleetDir, "flows/learn.yaml", "learn:\n  - name: drafter\n    use: drafter@1\n    worker: ops-read\n    with: {flows: [ghost]}\n")
	if _, err := expand(t, fleetDir, lib); err == nil || !strings.Contains(err.Error(), `no flow "ghost"`) {
		t.Fatalf("a learning block names flows the fleet has: %v", err)
	}
	put(t, fleetDir, "flows/learn.yaml", "learn:\n  - name: drafter\n    use: drafter@1\n    worker: ops-read\n    every: 5m\n    with: {flows: [chain]}\n")
	put(t, lib, "drafter/v1/spec.tail.yaml", "explain:\n  trigger: Every hour.\n")
	put(t, lib, "drafter/v1/block.yaml", mustRead(t, filepath.Join(lib, "drafter/v1/block.yaml"))+"spec_tail: spec.tail.yaml\n")
	if _, err := expand(t, fleetDir, lib); err == nil || !strings.Contains(err.Error(), "explain text states its defaults") {
		t.Fatalf("a flow may not change the schedule an explain text states: %v", err)
	}
}

// The generator quotes nothing but a label match, which is one single-quoted JSON value of characters that are inert there
// (disk_flow_test.go); every other kind is still limited to characters a shell passes through unchanged.
func TestShellUnsafeValuesAreRefused(t *testing.T) {
	fleetDir, lib := fixture(t)
	put(t, fleetDir, "flows/chain/flow.yaml", strings.Replace(goodFlow, "metric: some_metric}", "metric: some_metric, skip: {fstype: tmpfs}, other: 'a|b'}", 1))
	put(t, lib, "probe/v1/block.yaml", strings.Replace(goodBlock, "env: [VM_URL]", "  other: {kind: string, env: THE_OTHER, optional: true}\nenv: [VM_URL]", 1))
	if _, err := expand(t, fleetDir, lib); err == nil || !strings.Contains(err.Error(), "the value goes on a command line") {
		t.Fatalf("a string is not quoted: %v", err)
	}
	put(t, fleetDir, "flows/chain/flow.yaml", strings.Replace(goodFlow, "metric: some_metric}", "metric: some_metric, skip: {fstype: tmpfs}}", 1))
	exp, err := expand(t, fleetDir, lib)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(exp.Files["chain-monitor/v1/spec.yaml"]), `THE_SKIP='{"fstype":"tmpfs"}' `) {
		t.Fatal(string(exp.Files["chain-monitor/v1/spec.yaml"]))
	}
}

func TestPackCoversFlows(t *testing.T) {
	fleetDir, lib := fixture(t)
	exp, _ := expand(t, fleetDir, lib)
	if err := exp.Write(fleetDir); err != nil {
		t.Fatal(err)
	}
	specs := filepath.Dir(fleetDir)
	pack := func() string {
		t.Helper()
		rs, err := LoadVersions(specs)
		if err != nil {
			t.Fatal(err)
		}
		fs, err := LoadFleets(specs, rs)
		if err != nil {
			t.Fatal(err)
		}
		return fs["demo"].Pack
	}
	with := pack()
	put(t, fleetDir, "flows/chain/flow.yaml", goodFlow+"# a comment\n")
	if pack() == with {
		t.Fatal("a changed flow is a changed fleet pack")
	}
	put(t, fleetDir, "flows/chain/flow.yaml", goodFlow)
	if pack() != with {
		t.Fatal("the pack is a function of the flows' bytes")
	}
	if err := os.RemoveAll(filepath.Join(fleetDir, "flows")); err != nil {
		t.Fatal(err)
	}
	if pack() == with {
		t.Fatal("the flows are part of the pack")
	}
}

// A routine that plans an action always carries a cap: the step's, else its block's. Without it a flow's routine could be
// raised past what its block allows. Monitors, verify and learning blocks never plan (dispatch.DecidePlan is reached only
// from a case trigger), so their routines keep no max_autonomy line.
func TestActingRoutinesAlwaysCarryACap(t *testing.T) {
	fleetDir, lib := fixture(t)
	exp, err := expand(t, fleetDir, lib)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(exp.Files["chain-think/routine.yaml"]); !strings.Contains(got, "autonomy: propose\nmax_autonomy: propose\n") {
		t.Errorf("a stage step with no cap of its own takes its block's:\n%s", got)
	}
	put(t, fleetDir, "flows/chain/flow.yaml", strings.Replace(goodFlow, "    max_autonomy: record\n", "", 1))
	exp, err = expand(t, fleetDir, lib)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(exp.Files["chain-act/routine.yaml"]); !strings.Contains(got, "autonomy: record\nmax_autonomy: auto\n") {
		t.Errorf("an act step takes its block's cap:\n%s", got)
	}
	for _, n := range []string{"chain-monitor", "chain-verify"} {
		if strings.Contains(string(exp.Files[n+"/routine.yaml"]), "max_autonomy") {
			t.Errorf("%s never plans: no cap line", n)
		}
	}
}
