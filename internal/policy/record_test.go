package policy

import (
	"strings"
	"testing"

	"github.com/chipi/flotilla/internal/registry"
)

// A record-only action (RFC-0016 §12) is recorded whatever the ceilings say: nothing carries it out, so even with every cap at
// auto the decision is Record, and what it would have done is a proposal for a person, never Auto.
func TestRecordOnlyActionIsAlwaysRecorded(t *testing.T) {
	p, s := plan(), clear()
	s.Action = registry.Action{Name: "disk.cleanup_known", Executor: registry.ExecRecord, ForKinds: []string{"failed_backup"},
		Params: map[string]registry.Param{"host": {Kind: "target_host"}}, Risk: "low", Reversible: "yes",
		Limits: registry.Limits{PerTargetPerDay: 2}}
	p.Action = "disk.cleanup_known"
	d := Decide(p, s, Limits{})
	if d.Outcome != Record || d.Autonomy != "record" || d.Would != Propose {
		t.Fatalf("outcome %s, autonomy %s, would %s: %v", d.Outcome, d.Autonomy, d.Would, d.Reasons)
	}
	if r := strings.Join(d.Reasons, "; "); !strings.Contains(r, "record-only") {
		t.Errorf("the reason does not say why: %s", r)
	}
	// the same plan on a host action, for contrast, is what clear() decides
	if d := Decide(plan(), clear(), Limits{}); d.Outcome != Auto {
		t.Fatalf("the host action: %s", d.Outcome)
	}
}
