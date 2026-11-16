package ui

import (
	"strings"
	"testing"

	"github.com/chipi/flotilla/internal/store"
)

// Every change of the case stays, the first and last events stay, and each run of two or more events
// that changed nothing folds into one row with its counts and how many came after the case closed.
func TestFoldTimeline(t *testing.T) {
	ev := []store.CaseEvent{
		{At: "2026-11-15T13:56:36Z", Kind: "case.detected", To: "detected"},
		{At: "2026-11-15T13:56:37Z", Kind: "case.handed_over", From: "detected", To: "handed_over"},
		{At: "2026-11-15T14:11:00Z", Kind: "run.no_decision"},
		{At: "2026-11-15T14:14:00Z", Kind: "signal.attached"},
		{At: "2026-11-15T15:14:00Z", Kind: "signal.attached"},
		{At: "2026-11-15T19:04:00Z", Kind: "case.escalated", From: "handed_over", To: "escalated"},
		{At: "2026-11-15T19:30:00Z", Kind: "signal.attached"},
		{At: "2026-11-15T20:41:36Z", Kind: "case.closed", From: "planned", To: "closed"},
		{At: "2026-11-15T21:03:00Z", Kind: "signal.absorbed"},
		{At: "2026-11-15T22:03:00Z", Kind: "signal.absorbed"},
		{At: "2026-11-16T14:46:00Z", Kind: "signal.absorbed"},
	}
	rows := make([]timelineEvent, len(ev))
	for i, e := range ev {
		rows[i] = timelineEvent{What: e.Kind, When: e.At[11:16]}
	}
	out := foldTimeline(ev, rows, "2026-11-15T20:41:36Z")
	var got []string
	for _, r := range out {
		got = append(got, r.What)
	}
	want := "case.detected|case.handed_over|3 repeats folded|case.escalated|signal.attached|case.closed|2 repeats folded|signal.absorbed"
	if strings.Join(got, "|") != want {
		t.Fatalf("rows:\n got %s\nwant %s", strings.Join(got, "|"), want)
	}
	if len(out[2].Fold) != 3 || !strings.Contains(out[2].FoldText, "×") || strings.Contains(out[2].FoldText, "after the case closed") {
		t.Fatalf("the first fold: %+v", out[2])
	}
	if !strings.HasSuffix(out[6].FoldText, "(2 after the case closed)") {
		t.Fatalf("the second fold: %q", out[6].FoldText)
	}
	if out := foldTimeline(ev[:2], rows[:2], ""); len(out) != 2 || out[0].Fold != nil {
		t.Fatal("a short timeline changed")
	}
}
