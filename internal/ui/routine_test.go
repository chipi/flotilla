package ui

import (
	"strings"
	"testing"
	"time"

	"github.com/chipi/flotilla/internal/knowledge"
	"github.com/chipi/flotilla/internal/spec"
)

// The routine page draws a routine from its spec: a block per spec part and step, each with its
// explanation from the spec; an open block shows its panel; an unexplained block says so; the board's
// block for the routine leads here.
func TestRoutinePage(t *testing.T) {
	triage := &spec.Routine{Fleet: "triage", Routine: "cycle", Description: "one pass", Enabled: true, Worker: "triage", Autonomy: "shadow",
		Run: spec.Run{Command: "python3 orchestrator.py --cycle", Timeout: "20m"}, Every: 10 * time.Minute, Limit: time.Minute, Fresh: 35 * time.Minute,
		Trigger: spec.Trigger{Every: "10m"}, Freshness: "35m",
		Harness: &spec.Harness{Protocol: "json", Tools: []string{"event_detail"}, Gates: []string{"edge"},
			Limits: spec.HarnessLimits{MaxToolCalls: 3, MaxSteps: 4, ToolCallsPerTurn: 1}},
		Knowledge: knowledge.Use{Sources: []string{"homelab-handbook"}, Queries: []string{"by_service"}},
		Explain:   map[string]string{"trigger": "Every ten minutes. Then more.", "gate.edge": "A burst during a restart is never dismissed.", "knowledge": "The handbook page for a service."},
		Steps:     []spec.Step{{ID: "dedup", Lane: "sort", Who: "code", Title: "already decided", Short: "a recurrence", Explain: "Written as a recurrence.", Count: "recurrences"}},
		Chain:     []spec.ChainLink{{Routine: "bugfix/fix", Via: "a GitHub issue"}}}
	fix := &spec.Routine{Fleet: "bugfix", Routine: "fix", Description: "fixes", Enabled: true, Worker: "bugfix", Autonomy: "shadow",
		Run: spec.Run{Command: "python3 fix.py", Timeout: "3h"}, Limit: time.Hour, Trigger: spec.Trigger{Case: &spec.CaseTrigger{Kind: "code_bug"}},
		Explain: map[string]string{"run": "fix.py fixes the bug."},
		Steps:   []spec.Step{{ID: "coder", Lane: "agent", Who: "agent", Title: "coding agent", Short: "writes the fix", Explain: "pi writes the fix."}}}
	f := setupWith(t, []*spec.Routine{triage, fix})
	ck, _ := f.csrfOf(f.addOperator())

	_, page := f.do("GET", "/fleets/triage/cycle", ck, nil, nil)
	for _, want := range []string{
		"triage/cycle · drawn from its spec", "1 · When", "every 10m", "Every ten minutes.", // the trigger, its first sentence
		"node node--agent", "the model", "node node--tool", "node node--code", "already decided", "a recurrence",
		"The chain after it", "a GitHub issue", "bugfix/fix", "coding agent", "Open a block",
		`href="/fleets/triage/cycle?b=gate.edge#block"`,
		"knowledge homelab-handbook", `href="/fleets/triage/cycle?b=knowledge#block"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the routine page lacks %q", want)
		}
	}
	if strings.Contains(page, "Then more.") {
		t.Error("a block's short line is the explanation's first sentence only")
	}

	_, page = f.do("GET", "/fleets/triage/cycle?b=gate.edge", ck, nil, nil)
	for _, want := range []string{"A burst during a restart is never dismissed.", "harness.gates", "In the spec", "- edge"} {
		if !strings.Contains(page, want) {
			t.Errorf("the open block's panel lacks %q", want)
		}
	}
	_, page = f.do("GET", "/fleets/triage/cycle?b=worker", ck, nil, nil)
	if !strings.Contains(page, "Not explained in the spec yet.") || !strings.Contains(page, "explain: worker") {
		t.Error("an unexplained block does not say so")
	}
	if resp, _ := f.do("GET", "/fleets/triage/nothing", ck, nil, nil); resp.StatusCode != 404 {
		t.Errorf("an unknown routine: %d", resp.StatusCode)
	}
	if _, page := f.do("GET", "/fleets/triage", ck, nil, nil); !strings.Contains(page, `href="/fleets/triage/cycle"`) || !strings.Contains(page, "node node--agent") {
		t.Error("the board's block does not lead to the routine page, or lacks who acts")
	}
}
