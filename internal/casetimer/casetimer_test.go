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
	id := open(t, st, "host:host-01")
	now := time.Now()
	_ = Run(ctx, st, now.Add(29*time.Minute), Config{}, log, nil)
	if strings.Contains(logs.String(), "stuck in detected") {
		t.Fatal("alerted before 30 minutes")
	}
	_ = Run(ctx, st, now.Add(31*time.Minute), Config{}, log, nil)
	_ = Run(ctx, st, now.Add(40*time.Minute), Config{}, log, nil)
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
	id := open(t, st, "host:host-01")
	_ = st.MoveCase(ctx, id, cases.HandedOver, cases.ByDetector, "", "m", "")
	now := time.Now()
	_ = Run(ctx, st, now.Add(23*time.Hour), Config{}, log, nil)
	if status(t, st, id) != cases.HandedOver {
		t.Fatal("escalated before 24 hours")
	}
	_ = Run(ctx, st, now.Add(25*time.Hour), Config{}, log, nil)
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
	id := open(t, st, "host:host-01")
	_ = st.MoveCase(ctx, id, cases.HandedOver, cases.ByDetector, "", "m", "")
	_ = Run(ctx, st, time.Now().Add(2*time.Hour), Config{HandedOverEscalate: time.Hour}, log, nil)
	if status(t, st, id) != cases.Escalated {
		t.Fatal("a configured 1h limit was not used")
	}
}

// A STOP or a pause holds a handed-over case: no escalation while held, and after the release the 24 hours
// start again (a STOP longer than the limit must not turn the queue into escalations).
func TestHeldCaseIsNotEscalated(t *testing.T) {
	ctx := context.Background()
	st, _, log := setup(t)
	id := open(t, st, "host:host-01")
	_ = st.MoveCase(ctx, id, cases.HandedOver, cases.ByDetector, "", "m", "")
	now := time.Now()
	held := func(context.Context, string, string) (bool, string, error) { return true, "", nil }
	_ = Run(ctx, st, now.Add(30*time.Hour), Config{}, log, held)
	if status(t, st, id) != cases.HandedOver {
		t.Fatal("a held case was escalated")
	}
	released := now.Add(29 * time.Hour).UTC().Format(store.TimeFormat)
	free := func(context.Context, string, string) (bool, string, error) { return false, released, nil }
	_ = Run(ctx, st, now.Add(30*time.Hour), Config{}, log, free)
	if status(t, st, id) != cases.HandedOver {
		t.Fatal("escalated one hour after the release")
	}
	_ = Run(ctx, st, now.Add(54*time.Hour), Config{}, log, free)
	if status(t, st, id) != cases.Escalated {
		t.Fatal("not escalated 25 hours after the release")
	}
}

func watching(t *testing.T, st *store.Store, target string) store.Case {
	t.Helper()
	ctx := context.Background()
	id := open(t, st, target)
	_ = st.MoveCase(ctx, id, cases.HandedOver, cases.ByDetector, "", "m", "")
	_ = st.MoveCase(ctx, id, cases.Planned, cases.ByAct, "", "ops/rerun", "")
	if _, err := st.ApplyPlan(ctx, store.PlanRecord{CaseID: id, RunID: "r", Action: "a.b", Params: map[string]string{}, Outcome: "record", Autonomy: "record"},
		cases.Watching, "", target, false); err != nil {
		t.Fatal(err)
	}
	c, err := st.Case(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func notices(t *testing.T, st *store.Store, id string) []store.Notice {
	t.Helper()
	ns, err := st.Notices(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return ns
}

// ADR-0023: a notice at once, then at 4 hours and 24 hours, then every 24 hours; one per pass at most, never a repeat
// of the same hour, and the case stays watching.
func TestWatchNoticeSchedule(t *testing.T) {
	ctx := context.Background()
	st, _, log := setup(t)
	c := watching(t, st, "host:host-01")
	start, _ := time.Parse(time.RFC3339Nano, c.StatusAt)
	at := func(h float64) time.Time { return start.Add(time.Duration(h * float64(time.Hour))) }
	// each pass: how long after the watching began, and how many notices there are then
	for _, p := range []struct {
		hours float64
		want  int
	}{{0, 1}, {0.5, 1}, {3.9, 1}, {4.1, 2}, {5, 2}, {23.9, 2}, {24.1, 3}, {40, 3}, {48.1, 4}, {72.1, 5}, {96.1, 6}, {120.1, 7}, {144.1, 7}, {500, 7}} {
		if err := Run(ctx, st, at(p.hours), Config{}, log, nil); err != nil {
			t.Fatal(err)
		}
		if got := len(notices(t, st, c.ID)); got != p.want {
			t.Fatalf("%.1fh after it began: %d notices, want %d", p.hours, got, p.want)
		}
	}
	if status(t, st, c.ID) != cases.Watching {
		t.Fatal("a notice moved the case")
	}
	ns := notices(t, st, c.ID)
	for i, n := range ns {
		if n.N != i+1 || n.Kind != store.NoticeWatching || n.SentAt != "" {
			t.Fatalf("notice %d: %+v", i, n)
		}
	}
	ev, _ := st.CaseEvents(ctx, c.ID)
	due := 0
	for _, e := range ev {
		if e.Kind == "notice.due" {
			due++
		}
	}
	if due != 7 {
		t.Fatalf("%d notice.due events, want 7", due)
	}
}

// At most 7 notices: a case watched for weeks is told 7 times, not once a day for ever.
func TestNoticeCap(t *testing.T) {
	start := time.Date(2026, 11, 21, 8, 0, 0, 0, time.UTC)
	if got := NoticeDue(start, 7, DefaultWatchNotify).Sub(start); got != 120*time.Hour {
		t.Fatalf("the 7th notice is due %s after the start, want 120h", got)
	}
	c := Config{}.withDefaults()
	if n := nextNotice(start, start.Add(400*24*time.Hour), &store.Notice{N: MaxNotices}, c); n != 0 {
		t.Fatalf("notice %d after the cap", n)
	}
}

// A timer that was down skips to the latest notice due: one notice, not the ones it missed.
func TestNoticeAfterAnOutageIsOne(t *testing.T) {
	start := time.Date(2026, 11, 21, 8, 0, 0, 0, time.UTC)
	c := Config{}.withDefaults()
	if n := nextNotice(start, start.Add(60*time.Hour), &store.Notice{N: 1}, c); n != 4 {
		t.Fatalf("after 60h with one sent, the next is %d, want 4 (due at 48h)", n)
	}
}

// Acknowledging a notice holds the next back for the snooze, however much of the schedule has passed.
func TestAckSnoozesTheNextNotice(t *testing.T) {
	start := time.Date(2026, 11, 21, 8, 0, 0, 0, time.UTC)
	c := Config{}.withDefaults()
	acked := &store.Notice{N: 1, AckedAt: start.Add(3 * time.Hour).Format(store.TimeFormat), AckedBy: "user:ada"}
	if n := nextNotice(start, start.Add(5*time.Hour), acked, c); n != 0 {
		t.Fatalf("notice %d inside the snooze", n)
	}
	if n := nextNotice(start, start.Add(27*time.Hour+time.Minute), acked, c); n != 3 {
		t.Fatalf("after the snooze the next is %d, want 3", n)
	}
}

// A case handed back and watched again is a new watching period: its notices start from the first.
func TestWatchedAgainStartsAnew(t *testing.T) {
	ctx := context.Background()
	st, _, log := setup(t)
	c := watching(t, st, "host:host-01")
	start, _ := time.Parse(time.RFC3339Nano, c.StatusAt)
	_ = Run(ctx, st, start, Config{}, log, nil)
	if len(notices(t, st, c.ID)) != 1 {
		t.Fatal("setup: the first notice")
	}
	if err := st.MoveCase(ctx, c.ID, cases.HandedOver, cases.ByOperator, "", "user:ada", "replan"); err != nil {
		t.Fatal(err)
	}
	_ = st.MoveCase(ctx, c.ID, cases.Planned, cases.ByAct, "", "ops/rerun", "")
	if _, err := st.ApplyPlan(ctx, store.PlanRecord{CaseID: c.ID, RunID: "r2", Action: "a.b", Params: map[string]string{}, Outcome: "record", Autonomy: "record"},
		cases.Watching, "", "host:host-01", false); err != nil {
		t.Fatal(err)
	}
	_ = Run(ctx, st, time.Now().Add(time.Minute), Config{}, log, nil)
	ns := notices(t, st, c.ID)
	if len(ns) != 2 || ns[1].N != 1 {
		t.Fatalf("%+v", ns)
	}
}
