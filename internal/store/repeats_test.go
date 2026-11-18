package store

import (
	"context"
	"testing"

	"github.com/chipi/flotilla/internal/cases"
)

func signals(t *testing.T, s *Store, id string) int {
	t.Helper()
	ev, err := s.CaseEvents(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range ev {
		if e.Kind == "signal.attached" || e.Kind == "signal.absorbed" {
			n++
		}
	}
	return n
}

// A detection that repeats the case's last signal exactly is counted on the case, not written as an event,
// both while the case is open (attached) and after it closed as a shadow plan (absorbed). New data, or a
// detection without data, is still an event. (shop #656's case gained an event every hour, 2026-11-16.)
func TestRepeatedDetectionsAreCounted(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	k := Key{Kind: "target", Value: "code_bug/repo:example/shop/issues/656"}
	n := func(data string) NewCase {
		return NewCase{Kind: "code_bug", Target: "repo:example/shop/issues/656", Keys: []Key{k}, Canonical: k, Data: data}
	}
	issue := `{"issue":{"number":656,"updated_at":"2026-11-15T13:00:00Z"}}`
	id, how, err := s.DetectIncident(ctx, n(issue), "bugfix/intake", "run i1")
	if err != nil || how != Opened {
		t.Fatalf("open: %s %v", how, err)
	}
	for i := 0; i < 3; i++ { // the same issue, unchanged, three more hours
		if got, how, err := s.DetectIncident(ctx, n(issue), "bugfix/intake", "run"); err != nil || got != id || how != Attached {
			t.Fatalf("repeat %d: %s %s %v", i, got, how, err)
		}
	}
	c, _ := s.Case(ctx, id)
	if signals(t, s, id) != 0 || c.Repeats != 3 || c.RepeatAt == "" {
		t.Fatalf("open case: %d signal events, repeats %d at %q", signals(t, s, id), c.Repeats, c.RepeatAt)
	}
	edited := `{"issue":{"number":656,"updated_at":"2026-11-15T15:00:00Z"}}`
	if _, how, _ := s.DetectIncident(ctx, n(edited), "bugfix/intake", "run"); how != Attached || signals(t, s, id) != 1 {
		t.Fatalf("an edited issue is new information: %s, %d events", how, signals(t, s, id))
	}
	if _, _, _ = s.DetectIncident(ctx, n(""), "bugfix/intake", "run"); signals(t, s, id) != 2 {
		t.Fatal("a detection without data is always an event")
	}

	// closed as a shadow plan: absorbed, and repeats are counted the same way
	_ = s.MoveCase(ctx, id, cases.HandedOver, cases.ByDetector, "", "bugfix/intake", "")
	_ = s.MoveCase(ctx, id, cases.Planned, cases.ByAct, "", "bugfix/fix", "")
	if _, err := s.ApplyPlan(ctx, PlanRecord{CaseID: id, RunID: "r", Action: "a.b", Params: map[string]string{}, Outcome: "shadow",
		Autonomy: "shadow"}, cases.Closed, cases.ShadowPlan, "repo:example/shop", false); err != nil {
		t.Fatal(err)
	}
	if _, how, _ := s.DetectIncident(ctx, n(edited), "bugfix/intake", "run"); how != Absorbed || signals(t, s, id) != 3 {
		t.Fatalf("first after close: %s, %d events", how, signals(t, s, id))
	}
	for i := 0; i < 5; i++ {
		_, _, _ = s.DetectIncident(ctx, n(edited), "bugfix/intake", "run")
	}
	c, _ = s.Case(ctx, id)
	if signals(t, s, id) != 3 || c.Repeats != 8 {
		t.Fatalf("closed case: %d signal events, repeats %d (want 3, 8)", signals(t, s, id), c.Repeats)
	}
}

// OpenCase's attach path (a storm is one case) counts exact repeats too.
func TestOpenCaseRepeats(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	k := Key{Kind: "target", Value: "failed_backup/host:backup-01"}
	nc := NewCase{Kind: "failed_backup", Target: "host:backup-01", Keys: []Key{k}, Canonical: k, Data: `{"age_h":27}`}
	id, _, _ := s.OpenCase(ctx, nc, "ops/backup-monitor", "")
	_, _, _ = s.OpenCase(ctx, nc, "ops/backup-monitor", "")
	nc.Data = `{"age_h":28}`
	_, _, _ = s.OpenCase(ctx, nc, "ops/backup-monitor", "")
	c, _ := s.Case(ctx, id)
	if signals(t, s, id) != 1 || c.Repeats != 1 {
		t.Fatalf("%d signal events, repeats %d (want 1, 1)", signals(t, s, id), c.Repeats)
	}
}
