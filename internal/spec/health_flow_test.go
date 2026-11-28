package spec

import (
	"os"
	"strings"
	"testing"
)

// The batch re-run flow (the health contract, docs/wip/health-contract.md) reads its action's precondition and check from the registry,
// as the service flow does: a flow that names another check, another kind's action or a missing action is refused at -check.
func TestTheBatchFlowChecksItsActionsConditionAndCheck(t *testing.T) {
	for name, c := range map[string]struct{ from, to, want string }{
		"another check":                       {"verify: window_failed_episodes_processed", "verify: service_up_again", "registry check"},
		"another kind":                        {"action: pipeline.rerun_scheduled, url_source", "action: disk.cleanup_known, url_source", "not an action for batch_window_failed"},
		"a missing action":                    {"action: pipeline.rerun_scheduled, url_source", "action: nope.nope, url_source", "the registry has no action"},
		"a url in place of the source's name": {"url_source: health_example", "url_source: https://health.example/ready", "a source name"},
	} {
		fleetDir := t.TempDir()
		raw := mustRead(t, "../../fleets/jobs/flows/batch/flow.yaml")
		if !strings.Contains(raw, c.from) {
			t.Fatalf("%s: the edit does not apply", name)
		}
		put(t, fleetDir, "flows/batch/flow.yaml", strings.Replace(raw, c.from, c.to, 1))
		ff, err := LoadFlows(fleetDir)
		if err != nil || ff == nil {
			t.Fatalf("%s: %v %v", name, ff, err)
		}
		if _, err := ff.Expand(repoBlocks, actionsStub); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: want a refusal naming %q, got %v", name, c.want, err)
		}
	}
}

// The probe's command carries the route's NAME, never its URL: the URL is the worker's host config.
func TestTheBatchProbeCommandNamesTheSourceNeverTheURL(t *testing.T) {
	raw := mustRead(t, "../../fleets/jobs/batch-probe/v1/spec.yaml")
	if !strings.Contains(raw, "PROBE_SOURCE=health_example") || strings.Contains(raw, "http") {
		t.Errorf("the probe's spec: %s", raw)
	}
}

// service-act and service-investigate read who holds the host, and stay version 1: the example is not deployed, and a second version would
// need a shadow worker for each of their workers in every stack that runs the examples (the e2e's shadow check refuses it).
func TestTheServiceFlowIsHolderAwareAtVersionOne(t *testing.T) {
	for _, r := range []string{"service-act", "service-investigate"} {
		v1 := mustRead(t, "../../fleets/jobs/"+r+"/v1/spec.yaml")
		if !strings.Contains(v1, "HOLDER_MODE_METRIC=gpu_mode") {
			t.Errorf("%s v1 does not read who holds the host: %s", r, v1)
		}
		if _, err := os.Stat("../../fleets/jobs/" + r + "/v2"); err == nil {
			t.Errorf("%s has a version 2", r)
		}
	}
}
