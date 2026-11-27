package spec

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const libraryChainFlow = `flow: backup
case_kind: failed_backup
steps:
  - name: ex-monitor
    use: metric-monitor@1
    description: |-
      Watches the example source.
    worker: ops-read
    every: 10m
    freshness: 25m
    with: {action: backup.rerun, metric: example_last_success}
  - name: ex-act
    use: sop-act@1
    worker: ops-act-host
    max_autonomy: record
    case: {on_record: watch}
    with: {metric: example_last_success}
  - name: ex-investigate
    use: sop-investigate@1
    worker: triage
    max_autonomy: propose
    case: {on_record: watch}
    model: example-model
    with: {vm_url: "http://metrics.example:8428", metric: example_last_success, instance: example, registry: /opt/flotilla/fleets/registry.json}
  - name: ex-verify
    use: metric-verify@1
    worker: ops-read
    verify: backup_newer_than_execution
    with: {action: backup.rerun, metric: example_last_success}
`

const libraryChainLearn = `learn:
  - name: ex-draft
    use: sop-draft@1
    worker: ops-read
    with: {repo_prefix: fleets/, flows: [backup]}
  - name: ex-review
    use: case-review@1
    worker: ops-read
  - name: ex-process-change
    use: process-change@1
    worker: ops-read
    max_autonomy: propose
    with: {repo: example/fleets, file: host/registry.json}
`

// TestTheSevenBlocksWireIntoTheOperatorChain expands all seven library blocks, the chain the installation runs, with
// placeholder values: the commands, the station, the prompts and the explain texts come out as the live routines have them.
func TestTheSevenBlocksWireIntoTheOperatorChain(t *testing.T) {
	root := t.TempDir()
	fleetDir := filepath.Join(root, "ops")
	put(t, fleetDir, "flows/backup/flow.yaml", libraryChainFlow)
	put(t, fleetDir, "flows/learn.yaml", libraryChainLearn)
	ff, err := LoadFlows(fleetDir)
	if err != nil {
		t.Fatal(err)
	}
	exp, err := ff.Expand(repoBlocks, actionsStub)
	if err != nil {
		t.Fatal(err)
	}
	if len(exp.Steps) != 7 {
		t.Fatalf("%d steps", len(exp.Steps))
	}
	cmds := map[string]string{
		"ex-monitor":        "env BACKUP_ACTION=backup.rerun BACKUP_METRIC=example_last_success python3 /opt/flotilla/ops/backup_monitor.py",
		"ex-act":            "env BACKUP_METRIC=example_last_success python3 /opt/flotilla/ops/sop_act.py --fleet ops",
		"ex-investigate":    "env VM_URL=http://metrics.example:8428 BACKUP_METRIC=example_last_success BACKUP_INSTANCE=example FLOTILLA_REGISTRY=/opt/flotilla/fleets/registry.json python3 /opt/flotilla/ops/sop_investigate.py --fleet ops",
		"ex-verify":         "env BACKUP_ACTION=backup.rerun BACKUP_METRIC=example_last_success python3 /opt/flotilla/ops/verify_backup.py",
		"ex-draft":          "env SOP_REPO_PREFIX=fleets/ python3 /opt/flotilla/ops/sop_draft.py --fleet ops --kinds failed_backup",
		"ex-review":         "python3 /opt/flotilla/ops/case_review.py",
		"ex-process-change": "env PROCESS_CHANGE_REPO=example/fleets PROCESS_CHANGE_FILE=host/registry.json python3 /opt/flotilla/ops/process_change.py",
	}
	for n, c := range cmds {
		if got := string(exp.Files[n+"/v1/spec.yaml"]); !strings.HasPrefix(got, "run:\n  command: "+c+"\n  workdir: .\n  timeout: ") {
			t.Errorf("%s:\n%s", n, got)
		}
	}
	inv := string(exp.Files["ex-investigate/v1/spec.yaml"])
	for _, want := range []string{"  timeout: 3m\nstations:\n  investigate:\n    harness: flotilla\n    model: example-model\n", "limits: {operator: 1}\n"} {
		if !strings.Contains(inv, want) {
			t.Errorf("the station lacks %q:\n%s", want, inv)
		}
	}
	for _, f := range []string{"system.md", "task.md"} {
		want, _ := os.ReadFile(filepath.Join(repoBlocks, "sop-investigate", "v1", f))
		if string(exp.Files["ex-investigate/v1/"+f]) != string(want) {
			t.Errorf("%s is not the block's, byte for byte", f)
		}
	}
	if draft := string(exp.Files["ex-draft/v1/spec.yaml"]); !strings.Contains(draft, "Runs on the worker ops-read. Read only") || !strings.Contains(draft, "Every hour.") {
		t.Errorf("the explain texts carry the step's worker:\n%s", draft)
	}
	for n, want := range map[string]string{
		"ex-act":            "trigger:\n  case: {kind: failed_backup, on_record: watch}\nautonomy: record\nmax_autonomy: record\n",
		"ex-investigate":    "trigger:\n  case: {kind: failed_backup, stage: investigate, on_record: watch}\nautonomy: propose\nmax_autonomy: propose\nbudget:\n  day_usd: 1\n",
		"ex-draft":          "trigger:\n  every: 1h\ndetects: [sop_proposal]\nautonomy: record\nbudget:\n  day_usd: 0\nfreshness: 3h\n",
		"ex-review":         "trigger:\n  every: 30m\ndetects: [process_change]\n",
		"ex-process-change": "trigger:\n  case: {kind: process_change}\nautonomy: propose\nmax_autonomy: propose\n",
		"ex-verify":         "trigger:\n  check: {verify: backup_newer_than_execution}\nautonomy: record\n",
		"ex-monitor":        "trigger:\n  every: 10m\ndetects: [failed_backup]\nautonomy: record\nbudget:\n  day_usd: 0\nfreshness: 25m\n",
	} {
		if got := string(exp.Files[n+"/routine.yaml"]); !strings.Contains(got, want) {
			t.Errorf("%s routine.yaml lacks %q:\n%s", n, want, got)
		}
	}
	if err := exp.Write(fleetDir); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadVersions(root); err != nil {
		t.Fatalf("the seven generated routines load as ordinary routines, one acting step per kind and stage: %v", err)
	}
}
