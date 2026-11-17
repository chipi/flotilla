package ui

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/chipi/flotilla/internal/store"
)

// The run page's calls table shows each call's step and why the model stopped; a reply cut at the token
// cap is said so in the stop chip.
func TestRunPageShowsStepAndFinishReason(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	if err := f.st.StartRun(ctx, store.Run{ID: "run-len", Routine: "triage/cycle", SpecHash: "h", Trigger: "schedule", Actor: "flotillad",
		Autonomy: "shadow", StartedAt: time.Now().UTC().Format(store.TimeFormat), Status: "running"}); err != nil {
		t.Fatal(err)
	}
	s1, s2 := int64(1), int64(2)
	at := time.Now().UTC().Format(store.TimeFormat)
	if err := f.st.RecordCalls(ctx, []store.ModelCall{
		{RunID: "run-len", Routine: "triage/cycle", At: at, Alias: "triage-pro", Model: "m", Step: &s1, FinishReason: "stop"},
		{RunID: "run-len", Routine: "triage/cycle", At: at, Alias: "triage-pro", Model: "m", Step: &s2, FinishReason: "length"}}); err != nil {
		t.Fatal(err)
	}
	ck := f.signin(f.vic)
	_, page := f.do("GET", "/runs/run-len", ck, nil, nil)
	for _, want := range []string{`<th scope="col">Step</th>`, `<td data-label="Step">1</td>`, `<td data-label="Step">2</td>`,
		`<span class="exact soft">stop</span>`, `<span class="chip chip--stop">cut at cap</span>`} {
		if !strings.Contains(page, want) {
			t.Errorf("the run page has no %q", want)
		}
	}
	if n := strings.Count(page, "cut at cap"); n != 1 {
		t.Errorf("%d calls are said to be cut at the cap, want 1", n)
	}
}

// When the calls sit on their spans (no calls table), the span's line carries the turn and a cut reply.
func TestCallLineShowsTurnAndCut(t *testing.T) {
	s2, in, out := int64(2), int64(1800), int64(8192)
	got := callLine(store.ModelCall{Alias: "triage-pro", Step: &s2, TokensIn: &in, TokensOut: &out, FinishReason: "length"})
	if got != "triage-pro · turn 2 · 1800 → 8192 tokens · cut at the token cap" {
		t.Errorf("callLine = %q", got)
	}
	if got := callLine(store.ModelCall{Alias: "a", FinishReason: "stop"}); got != "a" {
		t.Errorf("a plain stop adds nothing: %q", got)
	}
	if got := callLine(store.ModelCall{Alias: "a", FinishReason: "content_filter"}); got != "a · ended: content_filter" {
		t.Errorf("another reason is named: %q", got)
	}
}
