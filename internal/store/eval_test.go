package store

import (
	"context"
	"testing"
)

// The newest result for each metric of a routine, prompt and model is the current one; results are appended.
func TestEvalResults(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	rec := func(routine, sha, model, metric string, v float64) {
		t.Helper()
		if _, err := s.RecordEval(ctx, EvalResult{Routine: routine, PromptSHA: sha, Model: model, Metric: metric, Value: v, N: 65, Repeats: 3,
			Source: "a replay", RecordedBy: "operator"}); err != nil {
			t.Fatal(err)
		}
	}
	rec("triage/cycle", "abc", "m1", "triage_cost", 0.5)
	rec("triage/cycle", "abc", "m1", "triage_cost", 0.446)
	rec("triage/cycle", "abc", "m1", "agree", 0.9)
	rec("triage/cycle", "abc", "m2", "triage_cost", 0.1)
	rec("ops/brief", "abc", "m1", "triage_cost", 7)

	got, err := s.LatestEval(ctx, "triage/cycle", "abc", "m1")
	if err != nil || len(got) != 2 {
		t.Fatalf("latest: %+v %v", got, err)
	}
	if got[0].Metric != "agree" || got[1].Metric != "triage_cost" || got[1].Value != 0.446 || got[1].N != 65 || got[1].Repeats != 3 ||
		got[1].RecordedBy != "operator" || got[1].RecordedAt == "" {
		t.Fatalf("latest: %+v", got)
	}
	if none, err := s.LatestEval(ctx, "triage/cycle", "zzz", "m1"); err != nil || len(none) != 0 {
		t.Fatalf("an unmeasured prompt: %+v %v", none, err)
	}
	all, err := s.Evals(ctx, "triage/cycle", 10)
	if err != nil || len(all) != 4 || all[0].Model != "m2" {
		t.Fatalf("the routine's results, newest first: %+v %v", all, err)
	}
	if every, _ := s.Evals(ctx, "", 10); len(every) != 5 {
		t.Fatalf("every routine's: %d", len(every))
	}
	if _, err := s.RecordEval(ctx, EvalResult{Routine: "r", Model: "m", Metric: "x", RecordedBy: "u"}); err == nil {
		t.Error("a result without a prompt sha was kept")
	}
	if _, err := s.RecordEval(ctx, EvalResult{Routine: "r", PromptSHA: "p", Model: "m", Metric: "x"}); err == nil {
		t.Error("a result without who recorded it was kept")
	}
}
