package api

import (
	"context"
	"testing"
)

// An agent never records the numbers its routine is judged by: only an operator or admin does.
func TestEvalRecordIsAnOperatorsNeverAnAgents(t *testing.T) {
	ts, st := server(t)
	body := `{"routine":"triage/cycle","prompt_sha":"abc123","model":"m1","metric":"triage_cost","value":0.446,"n":65,"repeats":3,"source":"a replay"}`
	for name, tok := range map[string]string{"agent": agentTok, "worker": workerTok} {
		if c, _ := call(t, ts, "POST", "/v1/evals", tok, body); c != 403 {
			t.Fatalf("%s: %d, want 403", name, c)
		}
	}
	if c, _ := call(t, ts, "POST", "/v1/evals", "", body); c != 401 {
		t.Fatalf("no token: %d, want 401", c)
	}
	if got, _ := st.Evals(context.Background(), "", 10); len(got) != 0 {
		t.Fatalf("a refused call recorded %+v", got)
	}
	if c, out := call(t, ts, "POST", "/v1/evals", opTok, body); c != 201 || out["by"] != "operator" {
		t.Fatalf("operator: %d %v", c, out)
	}
	got, err := st.LatestEval(context.Background(), "triage/cycle", "abc123", "m1")
	if err != nil || len(got) != 1 || got[0].Value != 0.446 || got[0].RecordedBy != "operator" || got[0].N != 65 {
		t.Fatalf("recorded: %+v %v", got, err)
	}
	if c, _ := call(t, ts, "POST", "/v1/evals", opTok, `{"routine":"r","model":"m","metric":"x","value":1}`); c != 400 {
		t.Fatalf("no prompt sha: %d, want 400", c)
	}
	if c, out := call(t, ts, "GET", "/v1/evals?routine=triage/cycle", agentTok, ""); c != 200 || len(out["evals"].([]any)) != 1 {
		t.Fatalf("an agent may read them: %d %v", c, out)
	}
}
