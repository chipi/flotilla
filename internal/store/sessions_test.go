package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestSessions(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	clock := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return clock }
	if err := s.AddUser(ctx, "ada", UserAdmin, "h-ada", "operator", "api", true); err != nil {
		t.Fatal(err)
	}
	if err := s.AddUser(ctx, "vic", UserViewer, "h-vic", "user:ada", "api", false); err != nil {
		t.Fatal(err)
	}
	if err := s.StartSession(ctx, "sid-1", "vic", "csrf-1"); err != nil {
		t.Fatal(err)
	}
	if x, _ := s.SessionByID(ctx, "sid-1"); x == nil || x.User != "vic" || x.Role != UserViewer || x.CSRF != "csrf-1" {
		t.Fatalf("a fresh session: %+v", x)
	}
	if x, _ := s.SessionByID(ctx, "no-such"); x != nil {
		t.Fatalf("an unknown id: %+v", x)
	}
	// A role change shows at the next lookup.
	if err := s.SetRole(ctx, "vic", UserOperator, "user:ada", "api"); err != nil {
		t.Fatal(err)
	}
	if x, _ := s.SessionByID(ctx, "sid-1"); x == nil || x.Role != UserOperator {
		t.Fatalf("after promotion: %+v", x)
	}
	// Idle for more than 7 days ends it.
	clock = clock.Add(SessionMaxIdle + time.Minute)
	if x, _ := s.SessionByID(ctx, "sid-1"); x != nil {
		t.Fatalf("an idle session lived: %+v", x)
	}
	// Used every day, it still ends 30 days after sign-in.
	_ = s.StartSession(ctx, "sid-2", "vic", "c")
	for d := 0; d < 30; d++ {
		clock = clock.Add(24 * time.Hour)
		if x, _ := s.SessionByID(ctx, "sid-2"); x == nil {
			t.Fatalf("a session in daily use ended on day %d", d+1)
		}
	}
	clock = clock.Add(2 * time.Hour)
	if x, _ := s.SessionByID(ctx, "sid-2"); x != nil {
		t.Fatalf("a session outlived 30 days: %+v", x)
	}
	// Rotating and revoking end every session; signing out everywhere is recorded as the user's own.
	_ = s.StartSession(ctx, "sid-3", "vic", "c")
	_ = s.StartSession(ctx, "sid-4", "vic", "c")
	if err := s.RotateToken(ctx, "vic", "h-vic-2", "user:ada", "api"); err != nil {
		t.Fatal(err)
	}
	if x, _ := s.SessionByID(ctx, "sid-3"); x != nil {
		t.Fatal("rotation left a session")
	}
	_ = s.StartSession(ctx, "sid-5", "vic", "c")
	if n, err := s.EndSessions(ctx, "vic", "user:vic", "ui"); err != nil || n != 1 {
		t.Fatalf("sign out everywhere: %d %v", n, err)
	}
	if ev, _ := s.Events(ctx, 1); ev[0].Kind != "user.signed-out-everywhere" {
		t.Fatalf("event %+v", ev[0])
	}
	_ = s.StartSession(ctx, "sid-6", "vic", "c")
	if err := s.RevokeUser(ctx, "vic", "user:ada", "api"); err != nil {
		t.Fatal(err)
	}
	if x, _ := s.SessionByID(ctx, "sid-6"); x != nil {
		t.Fatal("a revoked user's session lived")
	}
	if err := s.StartSession(ctx, "sid-7", "vic", "c"); err == nil {
		t.Fatal("a revoked user started a session")
	}
	// Pruning removes what is past either limit.
	_ = s.StartSession(ctx, "sid-8", "ada", "c")
	clock = clock.Add(SessionMaxIdle + time.Hour)
	if n, _ := s.PruneSessions(ctx); n != 1 {
		t.Fatalf("pruned %d, want 1", n)
	}
}
