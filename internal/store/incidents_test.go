package store

import (
	"context"
	"testing"

	"github.com/chipi/flotilla/internal/cases"
)

func shadowIncident(t *testing.T, s *Store, kind, target, monitor string) string {
	t.Helper()
	ctx := context.Background()
	k := Key{Kind: "target", Value: kind + "/" + target}
	id, how, err := s.DetectIncident(ctx, NewCase{Kind: kind, Target: target, Keys: []Key{k}, Canonical: k}, monitor, "")
	if err != nil || how != Opened {
		t.Fatalf("open: %s %v", how, err)
	}
	_ = s.MoveCase(ctx, id, cases.HandedOver, cases.ByDetector, "", monitor, "")
	_ = s.MoveCase(ctx, id, cases.Planned, cases.ByAct, "", "ops/rerun-backup", "")
	if _, err := s.ApplyPlan(ctx, PlanRecord{CaseID: id, RunID: "r", Action: "a.b", Params: map[string]string{}, Outcome: "shadow",
		Autonomy: "shadow"}, cases.Closed, cases.ShadowPlan, target, false); err != nil {
		t.Fatal(err)
	}
	return id
}

// Only the monitor that opened an incident clears it, and only when it no longer reports its key.
func TestClearIncidentsIsTheMonitorsOwn(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	backup := shadowIncident(t, s, "failed_backup", "host:mini", "ops/backup-monitor")
	disk := shadowIncident(t, s, "disk_full", "host:mini", "ops/disk-monitor")
	if c, _ := s.ClearIncidents(ctx, "ops/other-monitor", map[string]bool{}); len(c) != 0 {
		t.Fatalf("another monitor cleared %v", c)
	}
	c, _ := s.ClearIncidents(ctx, "ops/backup-monitor", map[string]bool{})
	if len(c) != 1 || c[0] != backup {
		t.Fatalf("cleared %v, want only %s", c, backup)
	}
	if c, _ := s.ClearIncidents(ctx, "ops/backup-monitor", map[string]bool{}); len(c) != 0 {
		t.Fatalf("cleared twice: %v", c)
	}
	if c, _ := s.ClearIncidents(ctx, "ops/disk-monitor", map[string]bool{"target=disk_full/host:mini": true}); len(c) != 0 {
		t.Fatalf("a key still reported was cleared: %v", c)
	}
	_ = disk
}
