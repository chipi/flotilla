package store

import (
	"context"
	"path/filepath"
	"strings"
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

// role, episode, its order and the tools of a turn come back as recorded; a call that gave none reads as "", "", nil, "".
func TestCallsKeepRoleEpisodeAndTools(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	seq := int64(2)
	if err := s.RecordCalls(ctx, []ModelCall{
		{RunID: "r1", Routine: "bugfix/fix", At: "2026-11-17T10:00:00.000000000Z", Alias: "a", Role: "coder", Episode: "ep-1", EpisodeSeq: &seq, Tools: `["bash","read"]`},
		{RunID: "r1", Routine: "bugfix/fix", At: "2026-11-17T10:00:01.000000000Z", Alias: "a"},
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.RunCalls(ctx, []string{"r1"})
	if err != nil || len(got["r1"]) != 2 {
		t.Fatalf("calls: %+v %v", got, err)
	}
	if c := got["r1"][0]; c.Role != "coder" || c.Episode != "ep-1" || c.EpisodeSeq == nil || *c.EpisodeSeq != 2 || c.Tools != `["bash","read"]` {
		t.Fatalf("the first call: %+v", c)
	}
	if c := got["r1"][1]; c.Role != "" || c.Episode != "" || c.EpisodeSeq != nil || c.Tools != "" {
		t.Fatalf("a call with none of them: %+v", c)
	}
	since, err := s.CallsSince(ctx, "2026-11-17T00:00:00Z")
	if err != nil || len(since) != 2 || since[0].Role != "coder" || since[0].Tools != `["bash","read"]` || *since[0].EpisodeSeq != 2 {
		t.Fatalf("calls since: %+v %v", since, err)
	}
}

func TestParseToolsReadsBothForms(t *testing.T) {
	yes, no := true, false
	for _, c := range []struct {
		in   string
		want []ToolCall
	}{
		{"", nil},
		{"[]", nil},
		{"not json", nil},
		{`{"name":"bash"}`, nil},
		{`["bash","read"]`, []ToolCall{{Name: "bash"}, {Name: "read"}}},
		{`[{"name":"bash","arg":"grep -n foo src/x.ts","ok":true},{"name":"read","ok":false},{"name":"ls"}]`,
			[]ToolCall{{Name: "bash", Arg: "grep -n foo src/x.ts", OK: &yes}, {Name: "read", OK: &no}, {Name: "ls"}}},
		{`["bash",{"name":"read","arg":"a"},{"arg":"nameless"},"",7]`, []ToolCall{{Name: "bash"}, {Name: "read", Arg: "a"}}},
	} {
		got := ParseTools(c.in)
		if len(got) != len(c.want) {
			t.Fatalf("%q: %+v, want %+v", c.in, got, c.want)
		}
		for i := range got {
			w := c.want[i]
			if got[i].Name != w.Name || got[i].Arg != w.Arg || (got[i].OK == nil) != (w.OK == nil) || (w.OK != nil && *got[i].OK != *w.OK) {
				t.Fatalf("%q[%d]: %+v, want %+v", c.in, i, got[i], w)
			}
		}
	}
	long := strings.Repeat("é", MaxToolArg+40)
	if got := ParseTools(`[{"name":"bash","arg":"` + long + `"}]`); len(got) != 1 || len([]rune(got[0].Arg)) != MaxToolArg {
		t.Fatalf("an over-long argument is cut at %d characters: %+v", MaxToolArg, got)
	}
}

func TestNormalizeToolsWritesTheObjectForm(t *testing.T) {
	if got := NormalizeTools([]byte(`["bash"]`)); got != `[{"name":"bash"}]` {
		t.Fatalf("names become objects: %q", got)
	}
	if got := NormalizeTools([]byte(`[{"name":"bash","arg":"ls","ok":true}]`)); got != `[{"name":"bash","arg":"ls","ok":true}]` {
		t.Fatalf("objects stay: %q", got)
	}
	for _, in := range []string{"", "null", "[]", "garbage"} {
		if got := NormalizeTools([]byte(in)); got != "" {
			t.Fatalf("%q reads as none, not %q", in, got)
		}
	}
}

// Both forms of tools, a step row's kind, and the default kind come back as recorded; CallsSince leaves steps out.
func TestCallsKeepToolObjectsAndKind(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	seq := int64(1)
	if err := s.RecordCalls(ctx, []ModelCall{
		{RunID: "r1", Routine: "bugfix/fix", At: "2026-11-17T10:00:00.000000000Z", Alias: "a", Role: "coder", Tools: `[{"name":"bash","arg":"ls","ok":true}]`},
		{RunID: "r1", Routine: "bugfix/fix", At: "2026-11-17T10:00:01.000000000Z", Alias: "a", Role: "coder", Tools: `["read"]`},
		{RunID: "r1", Routine: "bugfix/fix", At: "2026-11-17T10:00:02.000000000Z", Role: "gates", Kind: KindStep, EpisodeSeq: &seq, Detail: "round 1: gates clean"},
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.RunCalls(ctx, []string{"r1"})
	cs := got["r1"]
	if err != nil || len(cs) != 3 {
		t.Fatalf("calls: %+v %v", cs, err)
	}
	if cs[0].Kind != KindCall || cs[0].IsStep() || cs[1].Kind != KindCall || !cs[2].IsStep() || cs[2].Detail != "round 1: gates clean" || cs[2].Role != "gates" {
		t.Fatalf("kinds: %+v", cs)
	}
	if tc := cs[0].ToolCalls(); len(tc) != 1 || tc[0].Arg != "ls" || tc[0].OK == nil || !*tc[0].OK {
		t.Fatalf("object tools: %+v", tc)
	}
	if tc := cs[1].ToolCalls(); len(tc) != 1 || tc[0].Name != "read" || tc[0].OK != nil {
		t.Fatalf("old-form tools: %+v", tc)
	}
	since, err := s.CallsSince(ctx, "2026-11-17T00:00:00Z")
	if err != nil || len(since) != 2 {
		t.Fatalf("CallsSince leaves the step out: %+v %v", since, err)
	}
}
