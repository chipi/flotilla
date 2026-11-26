package store

import (
	"context"
	"testing"

	"github.com/chipi/flotilla/internal/cases"
)

// watched is an incident whose recorded plan kept its case open (on_record: watch, ADR-0023).
func watched(t *testing.T, s *Store, kind, target, monitor, data string) string {
	t.Helper()
	ctx := context.Background()
	k := Key{Kind: "target", Value: kind + "/" + target}
	id, how, err := s.DetectIncident(ctx, NewCase{Kind: kind, Target: target, Keys: []Key{k}, Canonical: k, Data: data}, monitor, "")
	if err != nil || how != Opened {
		t.Fatalf("open: %s %v", how, err)
	}
	for _, m := range []struct{ to, by, who string }{{cases.HandedOver, cases.ByDetector, monitor}, {cases.Planned, cases.ByAct, "ops/rerun-backup"}} {
		if err := s.MoveCase(ctx, id, m.to, m.by, "", m.who, ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.ApplyPlan(ctx, PlanRecord{CaseID: id, RunID: "r", Action: "a.b", Params: map[string]string{}, Outcome: "record",
		Autonomy: "record"}, cases.Watching, "", target, false); err != nil {
		t.Fatal(err)
	}
	return id
}

// A detection of a watched incident attaches to its open case, and an unchanged one counts as a repeat; it is never
// absorbed, which is what a closed plan_recorded case does.
func TestWatchingAttachesRepeatsNotAbsorbs(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	id := watched(t, s, "failed_backup", "host:backup-01", "ops/backup-monitor", `{"age_h":20}`)
	k := Key{Kind: "target", Value: "failed_backup/host:backup-01"}
	for i := 0; i < 3; i++ {
		got, how, err := s.DetectIncident(ctx, NewCase{Kind: "failed_backup", Target: "host:backup-01", Keys: []Key{k}, Canonical: k, Data: `{"age_h":20}`}, "ops/backup-monitor", "again")
		if err != nil || how != Attached || got != id {
			t.Fatalf("detection %d: %s %s %v", i, got, how, err)
		}
	}
	c, _ := s.Case(ctx, id)
	if c.Status != cases.Watching || c.Repeats != 3 {
		t.Fatalf("%+v", c)
	}
	if _, _, err := s.DetectIncident(ctx, NewCase{Kind: "failed_backup", Target: "host:backup-01", Keys: []Key{k}, Canonical: k, Data: `{"age_h":22}`}, "ops/backup-monitor", "older"); err != nil {
		t.Fatal(err)
	}
	ev, _ := s.CaseEvents(ctx, id)
	if last := ev[len(ev)-1]; last.Kind != "signal.attached" {
		t.Fatalf("a changed reading is an event of its own: %+v", last)
	}
	c, _ = s.Case(ctx, id)
	if c.Repeats != 3 {
		t.Fatalf("a changed reading counted as a repeat: %d", c.Repeats)
	}
}

// The monitor that opened a watched case closes it when its key is absent from a run; another monitor, or a run that
// still reports the key, does not.
func TestWatchingClosesWhenMonitorReportsClear(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	id := watched(t, s, "failed_backup", "host:backup-01", "ops/backup-monitor", "")
	key := "target=failed_backup/host:backup-01"
	if c, _ := s.ClearIncidents(ctx, "ops/other-monitor", map[string]bool{}); len(c) != 0 {
		t.Fatalf("another monitor cleared %v", c)
	}
	if c, _ := s.ClearIncidents(ctx, "ops/backup-monitor", map[string]bool{key: true}); len(c) != 0 {
		t.Fatalf("a key still reported was cleared: %v", c)
	}
	c, err := s.ClearIncidents(ctx, "ops/backup-monitor", map[string]bool{})
	if err != nil || len(c) != 1 || c[0] != id {
		t.Fatalf("cleared %v %v", c, err)
	}
	got, _ := s.Case(ctx, id)
	if got.Status != cases.Closed || got.Resolution != cases.ResolvedItself {
		t.Fatalf("%+v", got)
	}
	ev, _ := s.CaseEvents(ctx, id)
	if last := ev[len(ev)-1]; last.Kind != "case.closed" || last.By != cases.ByDetector || last.Actor != "ops/backup-monitor" || last.From != cases.Watching {
		t.Fatalf("the close names its actor: %+v", last)
	}
	if c, _ := s.ClearIncidents(ctx, "ops/backup-monitor", map[string]bool{}); len(c) != 0 {
		t.Fatalf("cleared twice: %v", c)
	}
}

// ADR-0023 decision 1: the promotion gate counts a watched case as a recorded plan, while it watches and once its
// monitor has closed it as resolved itself.
func TestUnjudgedAndStreakIncludeWatchClosed(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	a := watched(t, s, "failed_backup", "host:a", "ops/backup-monitor", "")
	b := watched(t, s, "failed_backup", "host:b", "ops/backup-monitor", "")
	if _, n, err := s.UnjudgedRecorded(ctx, 10); err != nil || n != 2 {
		t.Fatalf("watching: %d unjudged, %v", n, err)
	}
	if c, _ := s.ClearIncidents(ctx, "ops/backup-monitor", map[string]bool{}); len(c) != 2 {
		t.Fatalf("cleared %v", c)
	}
	ps, n, err := s.UnjudgedRecorded(ctx, 10)
	if err != nil || n != 2 || len(ps) != 2 {
		t.Fatalf("closed as resolved itself: %d unjudged %v, %v", n, ps, err)
	}
	if err := s.SetVerdict(ctx, a, "ada", VerdictRight); err != nil {
		t.Fatal(err)
	}
	if _, n, _ = s.UnjudgedRecorded(ctx, 10); n != 1 {
		t.Fatalf("%d unjudged after one verdict, want 1", n)
	}
	if st, err := s.PromotionStreak(ctx, "a.b"); err != nil || st.Clean != 1 {
		t.Fatalf("streak %+v %v: a watched-then-closed case judged right is a clean plan", st, err)
	}
	_ = b
}

func TestNoticesAreRecordedWithTheirEvent(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	id := watched(t, s, "failed_backup", "host:backup-01", "ops/backup-monitor", "")
	if err := s.DueNotice(ctx, id, NoticeWatching, 1, "2026-11-21T08:00:00Z"); err != nil {
		t.Fatal(err)
	}
	ns, err := s.Notices(ctx, id)
	if err != nil || len(ns) != 1 || ns[0].N != 1 || ns[0].Kind != NoticeWatching || ns[0].SentAt != "" || ns[0].AckedBy != "" {
		t.Fatalf("%+v %v", ns, err)
	}
	ev, _ := s.CaseEvents(ctx, id)
	if last := ev[len(ev)-1]; last.Kind != "notice.due" || last.Actor != "flotillad" {
		t.Fatalf("%+v", last)
	}
	if err := s.DueNotice(ctx, "case-none", NoticeWatching, 1, "2026-11-21T08:00:00Z"); err != ErrNoSuchCase {
		t.Fatalf("a notice for no case: %v", err)
	}
}
