package store

import (
	"context"
	"testing"
	"time"

	"github.com/chipi/flotilla/internal/cases"
)

func hookCall(delivery, target string) HookCall {
	k := Key{Kind: "hook:meeting", Value: target}
	return HookCall{Hook: "meeting", Delivery: delivery, Keep: 10 * time.Minute, Detail: "delivery " + delivery,
		Case: NewCase{Title: "A meeting's notes arrived", Kind: "meeting", Target: target, Project: "meetings", Keys: []Key{k}, Canonical: k, Data: `{"file":"` + target + `"}`}}
}

func TestAHookCallOpensOneHandedOverCase(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	r, err := s.AcceptHookCall(ctx, hookCall("d-1", "notes.md"))
	if err != nil || !r.Opened || r.Replay || r.CaseID == "" {
		t.Fatalf("first call: %+v %v", r, err)
	}
	c, err := s.Case(ctx, r.CaseID)
	if err != nil || c.Status != cases.HandedOver || c.CanonicalKey != "hook:meeting=notes.md" || c.Title != "A meeting's notes arrived" {
		t.Fatalf("case: %+v %v", c, err)
	}
}

func TestTheSameDeliveryOpensNothing(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	first, _ := s.AcceptHookCall(ctx, hookCall("d-1", "notes.md"))
	again, err := s.AcceptHookCall(ctx, hookCall("d-1", "other.md"))
	if err != nil || !again.Replay || again.Opened || again.CaseID != first.CaseID {
		t.Fatalf("replay: %+v %v", again, err)
	}
	if n := countCases(t, s); n != 1 {
		t.Fatalf("%d cases after a replay", n)
	}
}

func TestANewDeliveryOnTheSameKeyAttaches(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	first, _ := s.AcceptHookCall(ctx, hookCall("d-1", "notes.md"))
	second, err := s.AcceptHookCall(ctx, hookCall("d-2", "notes.md"))
	if err != nil || second.Opened || second.Replay || second.CaseID != first.CaseID {
		t.Fatalf("second delivery: %+v %v", second, err)
	}
}

func TestADeliveryIdIsForgottenAfterItsKeep(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	now := time.Now()
	s.now = func() time.Time { return now }
	c := hookCall("d-1", "notes.md")
	c.Close = true
	if _, err := s.AcceptHookCall(ctx, c); err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return now.Add(11 * time.Minute) }
	r, err := s.AcceptHookCall(ctx, c)
	if err != nil || r.Replay || !r.Opened {
		t.Fatalf("after its keep: %+v %v", r, err)
	}
}

func TestTheSmokeHooksCaseClosesOnArrival(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	c := hookCall("d-1", "smoke")
	c.Close = true
	r, err := s.AcceptHookCall(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Case(ctx, r.CaseID)
	if err != nil || got.Status != cases.Closed || got.Resolution != cases.ResolvedItself {
		t.Fatalf("smoke case: %+v %v", got, err)
	}
}

func countCases(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM cases`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
