package store

import (
	"context"
	"testing"
	"time"
)

// Stored times are compared as strings in SQL, so they must sort as times. RFC3339Nano drops trailing
// zeros ("…:00Z" and "…:00.5Z"), and '.' sorts before 'Z': within one second, a later time compared
// as earlier. A hold ending half a second from now read as already over.
func TestStoredTimesSortWithinASecond(t *testing.T) {
	ctx := context.Background()
	s, _ := open(t)
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	if _, err := s.AddHold(ctx, "host:mini", now.Add(500*time.Millisecond), "user:ada", "api", "deploy"); err != nil {
		t.Fatal(err)
	}
	hs, err := s.Holds(ctx, now, false)
	if err != nil || len(hs) != 1 {
		t.Fatalf("a hold ending in 0.5 s, read at a whole second: %d active, %v", len(hs), err)
	}
	if hs, _ := s.Holds(ctx, now.Add(500*time.Millisecond), false); len(hs) != 0 {
		t.Fatalf("a hold at its end: %d active", len(hs))
	}
	if got := now.Format(ts); got != "2026-11-13T12:00:00.000000000Z" {
		t.Fatalf("the stored form %q", got)
	}
}
