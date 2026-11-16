package ui

import (
	"context"
	"strings"
	"testing"

	"github.com/chipi/flotilla/internal/store"
)

func i64(v int64) *int64     { return &v }
func f64(v float64) *float64 { return &v }
func bptr(v bool) *bool      { return &v }

// A model call sits on its model.chat span, in time order, only when the two lists match one to one.
func TestCallsOnSpans(t *testing.T) {
	tv := traceView{Spans: []traceSpan{{Name: "run x", Kind: "run"}, {Name: "model.chat", Kind: "model"}, {Name: "tool event_detail", Kind: "tool"}, {Name: "model.chat", Kind: "model"}}}
	calls := []store.ModelCall{
		{At: "2026-11-16T15:05:07Z", Alias: "triage-pro", TokensIn: i64(14), TokensOut: i64(46), CostUSD: f64(0.000165), Check: "json_object", CheckOK: bptr(false)},
		{At: "2026-11-16T15:05:00Z", Alias: "triage-flash", TokensIn: i64(37), TokensOut: i64(6), CostUSD: f64(0.000004)},
	}
	if !callsOnSpans(&tv, calls) {
		t.Fatal("two calls, two model spans: not paired")
	}
	if tv.Spans[1].Call != "triage-flash · 37 → 6 tokens · $0.000004" || tv.Spans[1].CallBad {
		t.Fatalf("the first span: %q", tv.Spans[1].Call)
	}
	if !strings.Contains(tv.Spans[3].Call, "check json_object fail") || !tv.Spans[3].CallBad {
		t.Fatalf("the second span: %q", tv.Spans[3].Call)
	}
	if tv.Spans[0].Call != "" || tv.Spans[2].Call != "" {
		t.Fatal("a call on a span that is not a model call")
	}
	tv2 := traceView{Spans: []traceSpan{{Name: "model.chat", Kind: "model"}}}
	if callsOnSpans(&tv2, calls) || tv2.Spans[0].Call != "" {
		t.Fatal("one span, two calls: paired anyway")
	}
}

// The run page lists the decisions the run wrote, in time order, with their outcome, gates and offset.
func TestRunDecisions(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	if err := f.st.StartRun(ctx, store.Run{ID: "triage-cycle-1", Routine: "triage/cycle", SpecHash: "h", Trigger: "schedule", Actor: "flotillad",
		Autonomy: "propose", StartedAt: "2026-11-16T10:00:00Z", Status: "running"}); err != nil {
		t.Fatal(err)
	}
	for _, row := range []map[string]string{
		{"ts": "2026-11-16T10:00:30Z", "disposition": "escalate", "alertname": "APITimeoutError: Request timed out.", "fingerprint": "glitchtip:LITELLM-18",
			"gates": "i:n/a/c:n/a/d:downgraded (no corroboration)/e:n/a", "certainty": "high", "n_probes": "3", "stage": "propose", "occurrence_id": "a"},
		{"ts": "2026-11-16T10:00:05Z", "disposition": "file", "alertname": "OpenRouter credit low", "fingerprint": "grafana:abc",
			"gates": "i:passed/c:n/a/d:n/a/e:n/a", "certainty": "high", "n_probes": "0", "stage": "propose", "occurrence_id": "b"},
	} {
		if _, err := f.st.AppendEntry(ctx, "triage/cycle", "decision", row, "triage-cycle-1"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.st.AppendEntry(ctx, "triage/cycle", "recurrence", map[string]string{"ts": "2026-11-16T10:00:40Z", "occurrence_id": "c"}, "triage-cycle-1"); err != nil {
		t.Fatal(err)
	}
	ck, _ := f.csrfOf(f.addOperator())
	_, page := f.do("GET", "/runs/triage-cycle-1", ck, nil, nil)
	for _, want := range []string{"What the run decided, in time order", "&#43;30 s", "APITimeoutError: Request timed out.", "d: downgraded (no corroboration)", "&#43;5 s", "none fired"} {
		if !strings.Contains(page, want) {
			t.Errorf("the run page lacks %q", want)
		}
	}
	if i, j := strings.Index(page, "OpenRouter credit low"), strings.Index(page, "APITimeoutError"); i < 0 || j < 0 || i > j {
		t.Error("decisions not in the order the run wrote them")
	}
}
