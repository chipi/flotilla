package spec

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestLabelMatchIsOneQuotedJSONValue(t *testing.T) {
	fleetDir, lib := fixture(t)
	for _, c := range []struct{ with, want string }{
		{"{fstype: 'tmpfs|overlay'}", `THE_SKIP='{"fstype":"tmpfs|overlay"}'`},
		{"{mountpoint: '/run/.*|/boot/efi', fstype: '^(tmpfs|squashfs)$'}", `THE_SKIP='{"fstype":"^(tmpfs|squashfs)$","mountpoint":"/run/.*|/boot/efi"}'`},
		{"{fstype: tmpfs}", `THE_SKIP='{"fstype":"tmpfs"}'`},
	} {
		put(t, fleetDir, "flows/chain/flow.yaml", strings.Replace(goodFlow, "metric: some_metric}", "metric: some_metric, skip: "+c.with+"}", 1))
		exp, err := expand(t, fleetDir, lib)
		if err != nil {
			t.Fatalf("%s: %v", c.with, err)
		}
		spec := string(exp.Files["chain-monitor/v1/spec.yaml"])
		if !strings.Contains(spec, c.want+" ") {
			t.Fatalf("%s:\n%s", c.with, spec)
		}
		// the shell hands the value to the script exactly as the JSON says: nothing in it is a pipe, a glob, an expansion or a quote
		line := strings.TrimPrefix(strings.SplitN(spec, "\n", 3)[1], "  command: ")
		env := strings.SplitN(strings.TrimPrefix(line, "env "), " python3 ", 2)[0]
		out, err := exec.Command("sh", "-c", "env "+env+" sh -c 'printf %s \"$THE_SKIP\"'").Output()
		if err != nil || string(out) != strings.Trim(c.want[len("THE_SKIP="):], "'") {
			t.Fatalf("%s: the shell gave %q, %v", c.with, out, err)
		}
	}
}

func TestLabelMatchRefusals(t *testing.T) {
	long := strings.Repeat("a", 201)
	for name, with := range map[string]string{
		"a single quote":             `{fstype: "a'b"}`,
		"a command after a quote":    `{fstype: "'; rm -rf /; echo '"}`,
		"a double quote":             `{fstype: 'a"b'}`,
		"a backslash":                `{fstype: 'a\b'}`,
		"a newline":                  `{fstype: "a\nb"}`,
		"a space":                    `{fstype: 'a b'}`,
		"a semicolon":                `{fstype: 'a;b'}`,
		"an ampersand":               `{fstype: 'a&b'}`,
		"a backtick":                 "{fstype: 'a`b'}",
		"a redirect":                 `{fstype: 'a>b'}`,
		"a brace":                    `{fstype: 'a{1}'}`,
		"an empty match":             `{fstype: ''}`,
		"a number":                   `{fstype: 1}`,
		"a regex that is not one":    `{fstype: '(tmpfs'}`,
		"a label that is not a name": `{'fs type': tmpfs}`,
		"the name label":             `{__name__: x}`,
		"a long value":               `{fstype: ` + long + `}`,
		"no labels":                  `{}`,
		"a list":                     `[tmpfs]`,
	} {
		fleetDir, lib := fixture(t)
		put(t, fleetDir, "flows/chain/flow.yaml", strings.Replace(goodFlow, "metric: some_metric}", "metric: some_metric, skip: "+with+"}", 1))
		if _, err := expand(t, fleetDir, lib); err == nil || !strings.Contains(err.Error(), "with skip") {
			t.Errorf("%s: want a refusal of with skip, got %v", name, err)
		}
	}
}

func TestACommandCanNameTheFlowsCaseKind(t *testing.T) {
	fleetDir, lib := fixture(t)
	put(t, lib, "doer/v1/block.yaml", strings.Replace(mustRead(t, filepath.Join(lib, "doer/v1/block.yaml")), "--fleet {{fleet}}", "--fleet {{fleet}} --kind {{case_kind}}", 1))
	exp, err := expand(t, fleetDir, lib)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(exp.Files["chain-act/v1/spec.yaml"]); !strings.Contains(got, "doer.py --fleet demo --kind failed_backup\n") {
		t.Fatal(got)
	}
}

func TestTheDiskBlocksAreNewVersionsAndLeaveV1AsItWas(t *testing.T) {
	lib, err := LoadLibrary(repoBlocks)
	if err != nil {
		t.Fatal(err)
	}
	v1 := map[string]string{"metric-monitor": "action metric", "sop-act": "metric", "sop-investigate": "vm_url metric instance registry", "metric-verify": "action metric"}
	for n, params := range v1 {
		b := lib[BlockRef{n, 1}]
		var got []string
		for _, p := range b.Params {
			got = append(got, p.Name)
		}
		if strings.Join(got, " ") != params {
			t.Errorf("%s@1 params are %v: v1 stays as it was", n, got)
		}
		b2 := lib[BlockRef{n, 2}]
		if b2 == nil {
			t.Fatalf("%s@2 is not in the library", n)
		}
		if b2.Role != b.Role {
			t.Errorf("%s@2 is a %s, v1 a %s", n, b2.Role, b.Role)
		}
	}
	m := lib[BlockRef{"metric-monitor", 2}]
	for _, want := range []string{"group_by", "exclude", "clear_after", "size_metric"} {
		if _, ok := m.ParamByName(want); !ok {
			t.Errorf("metric-monitor@2 has no %s", want)
		}
	}
	if m.Actions["precondition_of"] != "action" || !strings.Contains(m.Command, "--kind {{case_kind}}") {
		t.Errorf("metric-monitor@2: %+v %q", m.Actions, m.Command)
	}
}

const diskFlow = `flow: disk
case_kind: disk_low
steps:
  - name: disk-monitor
    use: metric-monitor@2
    worker: ops-read
    every: 10m
    freshness: 25m
    with: {action: disk.cleanup_known, metric: node_filesystem_avail_bytes, size_metric: node_filesystem_size_bytes,
           group_by: [instance, mountpoint], exclude: {fstype: "tmpfs|overlay"}, clear_after: 2, step: 10m}
  - name: disk-act
    use: sop-act@2
    worker: ops-act-host
    max_autonomy: record
    case: {on_record: watch}
  - name: disk-investigate
    use: sop-investigate@2
    worker: triage
    max_autonomy: propose
    case: {on_record: watch}
    model: flash
    with: {vm_url: "http://vm.example:8428"}
  - name: disk-verify
    use: metric-verify@2
    worker: ops-read
    verify: disk_free_above_threshold
    with: {action: disk.cleanup_known, metric: node_filesystem_avail_bytes, size_metric: node_filesystem_size_bytes}
`

func diskFixture(t *testing.T) string {
	t.Helper()
	fleetDir := filepath.Join(t.TempDir(), "demo")
	put(t, fleetDir, "fleet.yaml", "fleet: demo\ndescription: A demo.\n")
	put(t, fleetDir, "flows/disk/flow.yaml", diskFlow)
	return fleetDir
}

func expandDisk(t *testing.T, fleetDir string) (*Expansion, error) {
	t.Helper()
	ff, err := LoadFlows(fleetDir)
	if err != nil || ff == nil {
		t.Fatalf("LoadFlows: %v %v", ff, err)
	}
	return ff.Expand(repoBlocks, actionsStub) // actionsStub declares disk.cleanup_known as host/registry.json does
}

func TestTheDiskFlowExpandsToOrdinaryRoutines(t *testing.T) {
	exp, err := expandDisk(t, diskFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(exp.Files["disk-monitor/v1/spec.yaml"]), "run:\n  command: env MONITOR_ACTION=disk.cleanup_known MONITOR_METRIC=node_filesystem_avail_bytes "+
		`MONITOR_SIZE_METRIC=node_filesystem_size_bytes MONITOR_GROUP_BY=instance,mountpoint MONITOR_EXCLUDE='{"fstype":"tmpfs|overlay"}' `+
		"MONITOR_CLEAR_AFTER=2 MONITOR_STEP=10m python3 /opt/flotilla/ops/metric_monitor.py --kind disk_low\n  workdir: .\n  timeout: 1m\n"; got != want {
		t.Fatalf("monitor spec:\n%s\nwant:\n%s", got, want)
	}
	if got := string(exp.Files["disk-monitor/routine.yaml"]); !strings.Contains(got, "detects: [disk_low]") || !strings.Contains(got, "every: 10m") {
		t.Fatal(got)
	}
	if got := string(exp.Files["disk-act/routine.yaml"]); !strings.Contains(got, "case: {kind: disk_low, on_record: watch}") ||
		!strings.Contains(got, "max_autonomy: record") {
		t.Fatal(got)
	}
	if got := string(exp.Files["disk-investigate/routine.yaml"]); !strings.Contains(got, "case: {kind: disk_low, stage: investigate, on_record: watch}") ||
		!strings.Contains(got, "max_autonomy: propose") {
		t.Fatal(got)
	}
	if got := string(exp.Files["disk-verify/routine.yaml"]); !strings.Contains(got, "check: {verify: disk_free_above_threshold}") {
		t.Fatal(got)
	}
	if got := string(exp.Files["disk-verify/v1/spec.yaml"]); !strings.Contains(got, "verify_free_ratio.py") {
		t.Fatal(got)
	}
	if !strings.Contains(string(exp.Files["disk-investigate/v1/system.md"]), "mountpoint") {
		t.Fatal("the investigate prompt of @2 tells the model it can read a mountpoint")
	}
}

func TestTheDiskFlowRefusals(t *testing.T) {
	for _, c := range []struct{ name, edit, with, want string }{
		{"no size metric", "size_metric: node_filesystem_size_bytes,\n", "", "with size_metric: required"},
		{"group_by as text", "group_by: [instance, mountpoint]", "group_by: instance", "with group_by"},
		{"group_by with a bad label", "group_by: [instance, mountpoint]", "group_by: [instance, 'mount point']", "with group_by"},
		{"clear_after as text", "clear_after: 2", "clear_after: two", "with clear_after"},
		{"a quote in exclude", `exclude: {fstype: "tmpfs|overlay"}`, `exclude: {fstype: "tmp'fs"}`, "with exclude"},
		{"an action the registry lacks", "action: disk.cleanup_known, metric: node_filesystem_avail_bytes, size_metric: node_filesystem_size_bytes,\n", "action: disk.nothing, metric: node_filesystem_avail_bytes, size_metric: node_filesystem_size_bytes,\n", "the registry has no action"},
		{"a verify that is not the action's check", "verify: disk_free_above_threshold", "verify: something_else", `registry check is "disk_free_above_threshold"`},
		{"a cap raised past the block's", "max_autonomy: propose\n    case: {on_record: watch}\n    model", "max_autonomy: auto\n    case: {on_record: watch}\n    model", "raises sop-investigate's cap, propose"},
	} {
		t.Run(c.name, func(t *testing.T) {
			fleetDir := diskFixture(t)
			if !strings.Contains(diskFlow, c.edit) {
				t.Fatalf("test bug: %q", c.edit)
			}
			put(t, fleetDir, "flows/disk/flow.yaml", strings.Replace(diskFlow, c.edit, c.with, 1))
			_, err := expandDisk(t, fleetDir)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want %q, got %v", c.want, err)
			}
		})
	}
}

// The backup chain still pins the v1 blocks, and what it writes is what the committed folders hold: the new versions change no
// routine that pins the old ones.
func TestTheExampleFleetsBackupFlowIsStillV1(t *testing.T) {
	fleetDir := filepath.Join("..", "..", "fleets", "jobs")
	ff, err := LoadFlows(fleetDir)
	if err != nil || ff == nil {
		t.Fatalf("%v %v", ff, err)
	}
	var backup *Flow
	for _, f := range ff.Flows {
		if f.Flow == "backup" {
			backup = f
		}
	}
	if backup == nil || backup.Steps[0].Use != "metric-monitor@1" || backup.Steps[2].Use != "metric-verify@1" {
		t.Fatalf("%+v", backup)
	}
}
