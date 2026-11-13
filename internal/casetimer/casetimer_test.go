package casetimer

import (
	"bytes"
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chipi/flotilla/internal/cases"
	"github.com/chipi/flotilla/internal/store"
)

func setup(t *testing.T) (*store.Store, *bytes.Buffer, *slog.Logger) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	var buf bytes.Buffer
	return st, &buf, slog.New(slog.NewTextHandler(&buf, nil))
}

func open(t *testing.T, st *store.Store, target string) string {
	t.Helper()
	k := store.Key{Kind: "target", Value: "failed_backup/" + target}
	id, _, err := st.OpenCase(context.Background(), store.NewCase{Kind: "failed_backup", Target: target, Keys: []store.Key{k}, Canonical: k}, "m", "")
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func status(t *testing.T, st *store.Store, id string) string {
	c, err := st.Case(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return c.Status
}

func TestDetectedTooLongAlertsOnce(t *testing.T) {
	ctx := context.Background()
	st, logs, log := setup(t)
	id := open(t, st, "host:mini")
	now := time.Now()
	_ = Run(ctx, st, now.Add(29*time.Minute), Config{}, log)
	if strings.Contains(logs.String(), "stuck in detected") {
		t.Fatal("alerted before 30 minutes")
	}
	_ = Run(ctx, st, now.Add(31*time.Minute), Config{}, log)
	_ = Run(ctx, st, now.Add(40*time.Minute), Config{}, log)
	if n := strings.Count(logs.String(), "stuck in detected"); n != 1 || !strings.Contains(logs.String(), "level=ERROR") {
		t.Fatalf("alerts: %d\n%s", n, logs)
	}
	if status(t, st, id) != cases.Detected {
		t.Fatal("the alert moved the case")
	}
	ev, _ := st.CaseEvents(ctx, id)
	if ev[len(ev)-1].Kind != "timer.stuck" {
		t.Fatalf("the alert is not on the case: %+v", ev[len(ev)-1])
	}
}

func TestHandedOverTooLongEscalates(t *testing.T) {
	ctx := context.Background()
	st, _, log := setup(t)
	id := open(t, st, "host:mini")
	_ = st.MoveCase(ctx, id, cases.HandedOver, cases.ByDetector, "", "m", "")
	now := time.Now()
	_ = Run(ctx, st, now.Add(23*time.Hour), Config{}, log)
	if status(t, st, id) != cases.HandedOver {
		t.Fatal("escalated before 24 hours")
	}
	_ = Run(ctx, st, now.Add(25*time.Hour), Config{}, log)
	if status(t, st, id) != cases.Escalated {
		t.Fatal("not escalated after 24 hours")
	}
	ev, _ := st.CaseEvents(ctx, id)
	if e := ev[len(ev)-1]; e.By != cases.ByTimer || !strings.Contains(e.Detail, "longer than 24h") {
		t.Fatalf("the escalation: %+v", e)
	}
}

// A deferral (planned -> handed_over) keeps the clock of the first hand-over; an operator's
// reassignment starts a new one.
func TestHandOverClock(t *testing.T) {
	at := func(m int) string { return time.Date(2026, 11, 13, 0, m, 0, 0, time.UTC).Format(time.RFC3339Nano) }
	ev := []store.CaseEvent{
		{Kind: "case.detected", To: cases.Detected, At: at(0)},
		{Kind: "case.handed_over", From: cases.Detected, To: cases.HandedOver, At: at(1)},
		{Kind: "case.planned", From: cases.HandedOver, To: cases.Planned, At: at(2)},
		{Kind: "case.handed_over", From: cases.Planned, To: cases.HandedOver, At: at(3)},
	}
	if got, _ := handedOverSince(ev); got.Minute() != 1 {
		t.Fatalf("after a deferral the clock starts at minute %d, want 1", got.Minute())
	}
	ev = append(ev,
		store.CaseEvent{Kind: "case.escalated", From: cases.HandedOver, To: cases.Escalated, At: at(4)},
		store.CaseEvent{Kind: "case.handed_over", From: cases.Escalated, To: cases.HandedOver, At: at(5)},
		store.CaseEvent{Kind: "transition.refused", From: cases.HandedOver, To: cases.HandedOver, At: at(6)})
	if got, _ := handedOverSince(ev); got.Minute() != 5 {
		t.Fatalf("after a reassignment the clock starts at minute %d, want 5", got.Minute())
	}
}

func TestConfiguredLimits(t *testing.T) {
	ctx := context.Background()
	st, _, log := setup(t)
	id := open(t, st, "host:mini")
	_ = st.MoveCase(ctx, id, cases.HandedOver, cases.ByDetector, "", "m", "")
	_ = Run(ctx, st, time.Now().Add(2*time.Hour), Config{HandedOverEscalate: time.Hour}, log)
	if status(t, st, id) != cases.Escalated {
		t.Fatal("a configured 1h limit was not used")
	}
}
