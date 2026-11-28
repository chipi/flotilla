package spec

import (
	"strings"
	"testing"
)

// The service watch (RFC-0016 §12.2) adds blocks and versions and leaves the disk versions as they were.
func TestTheServiceBlocksAreNewAndLeaveTheDiskVersionsAsTheyWere(t *testing.T) {
	lib, err := LoadLibrary(repoBlocks)
	if err != nil {
		t.Fatal(err)
	}
	names := func(b *Block) string {
		var got []string
		for _, p := range b.Params {
			got = append(got, p.Name)
		}
		return strings.Join(got, " ")
	}
	for ref, params := range map[BlockRef]string{
		{"sop-act", 2}:         "metric avail_metric size_metric",
		{"sop-investigate", 2}: "vm_url metric instance registry",
		{"sop-act", 3}:         "metric avail_metric size_metric service_metric service_label",
		{"sop-investigate", 3}: "vm_url metric instance registry service_metric service_label",
		{"service-monitor", 1}: "action metric service_label services group_by context context_label readings clear_after step",
		{"service-verify", 1}:  "action metric service_label",
		// the health contract's versions (flotilla's side): the holder of the host, the health route, the alternatives of an SOP
		{"sop-act", 4}: "metric avail_metric size_metric service_metric service_label fixes holder_mode_metric holder_mode_label " +
			"holder_process_metric holder_container_metric holder_process_label holder_resident holder_prod_mode holder_trainer_mode health_source",
		{"sop-investigate", 4}: "vm_url metric instance registry service_metric service_label holder_mode_metric holder_mode_label " +
			"holder_process_metric holder_container_metric holder_process_label holder_resident holder_prod_mode holder_trainer_mode health_source",
		{"health-probe", 1}:  "action url_source capability components instance report readings clear_after",
		{"health-verify", 1}: "action run_source",
	} {
		b := lib[ref]
		if b == nil {
			t.Fatalf("%s is not in the library", ref)
		}
		if got := names(b); got != params {
			t.Errorf("%s params are %q, want %q", ref, got, params)
		}
	}
	if m := lib[BlockRef{"service-monitor", 1}]; m.Role != RoleMonitor || m.Actions["precondition_of"] != "action" || !strings.Contains(m.Command, "--kind {{case_kind}}") {
		t.Errorf("service-monitor@1: %s %+v %q", m.Role, m.Actions, m.Command)
	}
	if v := lib[BlockRef{"service-verify", 1}]; v.Role != RoleVerify || v.Actions["check_of"] != "action" {
		t.Errorf("service-verify@1: %s %+v", v.Role, v.Actions)
	}
	st, err := lib[BlockRef{"sop-investigate", 3}].SpecTailText("m", "w")
	if err != nil || !strings.Contains(string(st), "tool_names: [metric, series]") {
		t.Errorf("sop-investigate@3's station offers the series read: %q %v", st, err)
	}
	if st, err := lib[BlockRef{"sop-investigate", 2}].SpecTailText("m", "w"); err != nil || strings.Contains(string(st), "series") {
		t.Errorf("sop-investigate@2's station is unchanged: %q %v", st, err)
	}
}

// A service flow whose verify names another check than the action's is refused at -check, as disk's is.
func TestTheServiceFlowChecksItsActionsConditionAndCheck(t *testing.T) {
	for name, c := range map[string]struct{ from, to, want string }{
		"another check":    {"verify: service_up_again", "verify: disk_free_above_threshold", "registry check"},
		"another kind":     {"action: dgx.gpu_mode_prod, metric: service_up, service_label: service, services", "action: disk.cleanup_known, metric: service_up, service_label: service, services", "not an action for dgx_not_serving"},
		"a missing action": {"action: dgx.gpu_mode_prod, metric: service_up, service_label: service, services", "action: nope.nope, metric: service_up, service_label: service, services", "the registry has no action"},
	} {
		fleetDir := t.TempDir()
		raw := mustRead(t, "../../fleets/jobs/flows/service/flow.yaml")
		if !strings.Contains(raw, c.from) {
			t.Fatalf("%s: the edit does not apply", name)
		}
		put(t, fleetDir, "flows/service/flow.yaml", strings.Replace(raw, c.from, c.to, 1))
		ff, err := LoadFlows(fleetDir)
		if err != nil || ff == nil {
			t.Fatalf("%s: %v %v", name, ff, err)
		}
		if _, err := ff.Expand(repoBlocks, actionsStub); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: want a refusal naming %q, got %v", name, c.want, err)
		}
	}
}
