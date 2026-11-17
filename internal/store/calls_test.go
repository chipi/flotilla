package store

import (
	"context"
	"path/filepath"
	"testing"
)

// A run's model calls are kept with their tokens and cost; an unreported cost stays nil, never zero.
func TestRunCalls(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	in, out, cost := int64(1104), int64(96), 0.006
	if err := s.RecordCalls(ctx, []ModelCall{
		{RunID: "r1", Routine: "triage/cycle", At: "2026-11-14T10:00:00.000000000Z", Alias: "triage-pro", Model: "deepseek-v4-pro",
			TokensIn: &in, TokensOut: &out, CostUSD: &cost, CostSource: "reported:body"},
		{RunID: "r1", Routine: "triage/cycle", At: "2026-11-14T10:00:05.000000000Z", Alias: "triage-pro", Error: "Transient"},
		{RunID: "r2", Routine: "platform/heartbeat", At: "2026-11-13T10:00:00.000000000Z", Alias: "triage-flash"},
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.RunCalls(ctx, []string{"r1", "r9"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got["r1"]) != 2 || len(got["r9"]) != 0 {
		t.Fatalf("calls by run: %+v", got)
	}
	c := got["r1"][0]
	if *c.TokensIn != 1104 || *c.TokensOut != 96 || *c.CostUSD != 0.006 || c.Model != "deepseek-v4-pro" {
		t.Fatalf("the first call: %+v", c)
	}
	if f := got["r1"][1]; f.CostUSD != nil || f.TokensIn != nil || f.Error != "Transient" {
		t.Fatalf("a failed call's unreported numbers must stay nil: %+v", f)
	}
	since, err := s.CallsSince(ctx, "2026-11-14T00:00:00Z")
	if err != nil || len(since) != 2 {
		t.Fatalf("calls since: %d %v", len(since), err)
	}
	if err := s.RecordCalls(ctx, nil); err != nil {
		t.Fatalf("no calls: %v", err)
	}
}

// finish_reason, step and decision_key come back as recorded; a call that gave none reads as "", nil, "".
func TestCallsKeepFinishReasonStepAndDecisionKey(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	step := int64(3)
	if err := s.RecordCalls(ctx, []ModelCall{
		{RunID: "r1", Routine: "triage/cycle", At: "2026-11-17T10:00:00.000000000Z", Alias: "a", FinishReason: "length", Step: &step, DecisionKey: "fp-1"},
		{RunID: "r1", Routine: "triage/cycle", At: "2026-11-17T10:00:01.000000000Z", Alias: "a"},
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.RunCalls(ctx, []string{"r1"})
	if err != nil || len(got["r1"]) != 2 {
		t.Fatalf("calls: %+v %v", got, err)
	}
	if c := got["r1"][0]; c.FinishReason != "length" || c.Step == nil || *c.Step != 3 || c.DecisionKey != "fp-1" {
		t.Fatalf("the first call: %+v", c)
	}
	if c := got["r1"][1]; c.FinishReason != "" || c.Step != nil || c.DecisionKey != "" {
		t.Fatalf("a call with none of them: %+v", c)
	}
	since, err := s.CallsSince(ctx, "2026-11-17T00:00:00Z")
	if err != nil || len(since) != 2 || since[0].FinishReason != "length" || *since[0].Step != 3 {
		t.Fatalf("calls since: %+v %v", since, err)
	}
}
