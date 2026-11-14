package store

import (
	"context"
	"errors"
	"testing"
)

// G11: a day's brief is written once; a mark is more or less, replaces the person's earlier one, and is
// counted against the item's kind.
func TestBriefs(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	b := Brief{Day: "2026-11-15", WrittenAt: "2026-11-15T06:00:00Z", Headline: "1 thing needs you.",
		Sections: []BriefSection{{Title: "Needs you", Items: []BriefItem{{Text: "a", Kind: "needs:failed_backup"}}},
			{Title: "Watch", Items: []BriefItem{{Text: "b", Kind: "wrong:stale"}}}}}
	if err := s.SaveBrief(ctx, b); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveBrief(ctx, b); !errors.Is(err, ErrBriefExists) {
		t.Fatalf("a second brief for the day: %v", err)
	}
	if got, _ := s.BriefOn(ctx, "2026-11-15"); got == nil || got.Headline != b.Headline || len(got.Sections) != 2 {
		t.Fatalf("the brief as written: %+v", got)
	}
	if got, _ := s.BriefOn(ctx, "2026-11-14"); got != nil {
		t.Fatal("a day without a brief has one")
	}
	if err := s.MarkBriefItem(ctx, "2026-11-15", 1, "ada", "more"); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkBriefItem(ctx, "2026-11-15", 1, "ada", "less"); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkBriefItem(ctx, "2026-11-15", 0, "vic", "more"); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []struct {
		day  string
		item int
		mark string
	}{{"2026-11-15", 2, "more"}, {"2026-11-15", -1, "more"}, {"2026-11-15", 0, "meh"}, {"2026-11-13", 0, "more"}} {
		if err := s.MarkBriefItem(ctx, bad.day, bad.item, "ada", bad.mark); err == nil {
			t.Errorf("mark %+v was kept", bad)
		}
	}
	if m, _ := s.BriefMarks(ctx, "2026-11-15", "ada"); len(m) != 1 || m[1] != "less" {
		t.Errorf("ada's marks: %v", m)
	}
	if k, _ := s.MarksByKind(ctx, ""); k["wrong:stale"] != [2]int{0, 1} || k["needs:failed_backup"] != [2]int{1, 0} {
		t.Errorf("marks by kind: %v", k)
	}
}
