package mcpserver

import (
	"context"
	"testing"
	"time"

	"github.com/chipi/flotilla/internal/store"
)

// run_show carries each call's finish reason, step and decision key, so an agent can tell a reply cut at the cap.
func TestRunShowHasTheCallsStepAndFinishReason(t *testing.T) {
	a, ts := world(t)
	ctx := context.Background()
	if err := a.Store.StartRun(ctx, store.Run{ID: "r-1", Routine: "ops/backup-monitor", SpecHash: "h", Trigger: "manual", Actor: "operator",
		Autonomy: "record", StartedAt: time.Now().UTC().Format(store.TimeFormat), Status: "running"}); err != nil {
		t.Fatal(err)
	}
	step := int64(2)
	if err := a.Store.RecordCalls(ctx, []store.ModelCall{{RunID: "r-1", Routine: "ops/backup-monitor", Alias: "a", FinishReason: "length",
		Step: &step, DecisionKey: "fp-1", Role: "triage", Episode: "fp-1", EpisodeSeq: &step, Tools: `["bash"]`}}); err != nil {
		t.Fatal(err)
	}
	out, isErr := call(t, session(t, ts.URL, agentTok), "run_show", map[string]any{"id": "r-1"})
	if isErr {
		t.Fatalf("run_show: %v", out)
	}
	calls, _ := out["model_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("model_calls: %v", out)
	}
	c, _ := calls[0].(map[string]any)
	if c["finish_reason"] != "length" || c["step"] != float64(2) || c["decision_key"] != "fp-1" || c["role"] != "triage" || c["episode"] != "fp-1" || c["episode_seq"] != float64(2) || c["tools"] != `["bash"]` {
		t.Fatalf("the call: %v", c)
	}
}
