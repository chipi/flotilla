package ui

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/chipi/flotilla/internal/store"
)

// A cancelled run is a choice, not a failure: Now's list of what is wrong counts the error and leaves the
// cancelled runs out, and the run's own chip is neutral, never the red of a failure.
func TestCancelledRunIsNotAFailure(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	base := time.Now().UTC().Add(-time.Hour)
	for i, st := range []string{store.StatusError, store.StatusCancelled, store.StatusCancelled} {
		id := fmt.Sprintf("triage-cycle-%03d", i)
		_ = f.st.StartRun(ctx, store.Run{ID: id, Routine: "triage/cycle", SpecHash: "h", Trigger: "schedule", Actor: "flotillad",
			Autonomy: "shadow", StartedAt: base.Add(time.Duration(i) * time.Minute).Format(store.TimeFormat), Status: "running"})
		_ = f.st.FinishRun(ctx, id, st, nil, 0.1, 1, "cancelled: operator, test")
	}
	ck := f.signin(f.vic)
	_, now := f.do("GET", "/", ck, nil, nil)
	if !strings.Contains(now, "triage/cycle failed 1 time") || strings.Contains(now, "failed 3 times") || strings.Contains(now, "failed 2 times") {
		t.Fatalf("Now counts a cancelled run as a failure: %s", grep(now, "failed"))
	}
	if got := runChip(store.StatusCancelled); got != "chip chip--neutral" {
		t.Fatalf("a cancelled run's chip: %q", got)
	}
	if got := funcs["chip"].(func(string) string)("cancelled"); got != "chip chip--wait" {
		t.Fatalf("the status chip of a cancelled run: %q", got)
	}
}

func grep(body, word string) string {
	var out []string
	for _, l := range strings.Split(body, "\n") {
		if strings.Contains(l, word) {
			out = append(out, strings.TrimSpace(l))
		}
	}
	return strings.Join(out, " | ")
}
