package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/chipi/flotilla/internal/cases"
)

// G14: a case of the same kind on the same target, opened within 7 days of an earlier one closing,
// links to it and both timelines say so; another target, another kind, or a close 8 days ago does not.
func TestRecurrenceRule(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	open := func(kind, target, key string) string {
		t.Helper()
		k := Key{Kind: "target", Value: kind + "/" + target + key}
		id, _, err := s.OpenCase(ctx, NewCase{Kind: kind, Target: target, Keys: []Key{k}, Canonical: k}, "m", "")
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	closeIt := func(id string) {
		t.Helper()
		_ = s.MoveCase(ctx, id, cases.HandedOver, cases.ByDetector, "", "m", "")
		if err := s.MoveCase(ctx, id, cases.Closed, cases.ByAct, cases.ResolvedItself, "a", "cleared"); err != nil {
			t.Fatal(err)
		}
	}
	first := open("failed_backup", "host:backup-01", "#1")
	if c, _ := s.Case(ctx, first); c.RecurrenceOf != "" {
		t.Fatalf("a first case recurs nothing: %q", c.RecurrenceOf)
	}
	closeIt(first)
	other := open("failed_backup", "host:model-host", "#2")
	kind := open("disk_pressure", "host:backup-01", "#3")
	again := open("failed_backup", "host:backup-01", "#4")
	for id, want := range map[string]string{other: "", kind: "", again: first} {
		if c, _ := s.Case(ctx, id); c.RecurrenceOf != want {
			t.Errorf("%s (%s %s) recurs %q, want %q", id, c.Kind, c.Target, c.RecurrenceOf, want)
		}
	}
	evs, _ := s.CaseEvents(ctx, again)
	ok := false
	for _, e := range evs {
		ok = ok || e.Kind == "case.recurrence" && strings.Contains(e.Detail, first)
	}
	if !ok {
		t.Error("the new case's timeline does not name the case it recurs")
	}
	evs, _ = s.CaseEvents(ctx, first)
	ok = false
	for _, e := range evs {
		ok = ok || e.Kind == "case.recurred" && strings.Contains(e.Detail, again)
	}
	if !ok {
		t.Error("the earlier case's timeline does not say it came back")
	}
	if n, _ := s.Recurrences(ctx, ""); n["failed_backup"] != 1 || len(n) != 1 {
		t.Errorf("recurrences per kind: %v", n)
	}

	// eight days after the close it is a new case, not a recurrence
	closeIt(again)
	s.now = func() time.Time { return time.Now().UTC().Add(8 * 24 * time.Hour) }
	late := open("failed_backup", "host:backup-01", "#5")
	if c, _ := s.Case(ctx, late); c.RecurrenceOf != "" {
		t.Errorf("a case 8 days after the close recurs %q", c.RecurrenceOf)
	}
}
