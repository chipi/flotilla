package api

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/chipi/flotilla/internal/policy"
	"github.com/chipi/flotilla/internal/registry"
	"github.com/chipi/flotilla/internal/spec"
)

// ADR-0008 / RFC-0004 §5.2: one step up at a time, with a reason, by an operator or an admin; down any
// number of steps without one; an action that cannot be undone stays at most propose; policy reads it.
func TestActionAutonomy(t *testing.T) {
	ctx := context.Background()
	s := newServer(t, func(*spec.Routine) {})
	op := Caller{Name: "operator", Role: RoleOperator}
	agent := Caller{Name: "claude-homelab", Role: RoleAgent}
	code := func(err error) int {
		if p := AsProblem(err); p != nil {
			return p.Code
		}
		if err != nil {
			return 500
		}
		return 200
	}
	if c := code(s.SetActionAutonomy(ctx, agent, "api", rerun, "propose", "r")); c != 403 {
		t.Fatalf("an agent promoting: %d", c)
	}
	if c := code(s.SetActionAutonomy(ctx, op, "api", rerun, "auto", "r")); c != 400 {
		t.Fatalf("shadow straight to auto: %d", c)
	}
	if c := code(s.SetActionAutonomy(ctx, op, "api", rerun, "propose", " ")); c != 400 {
		t.Fatalf("a promotion without a reason: %d", c)
	}
	if c := code(s.SetActionAutonomy(ctx, op, "api", rerun, "propose", "5 shadow plans agreed")); c != 200 {
		t.Fatalf("shadow to propose: %d", c)
	}
	if c := code(s.SetActionAutonomy(ctx, op, "api", rerun, "auto", "10 approved unchanged")); c != 200 {
		t.Fatalf("propose to auto: %d", c)
	}
	if c := code(s.SetActionAutonomy(ctx, op, "api", rerun, "shadow", "")); c != 200 {
		t.Fatalf("a demotion without a reason: %d", c)
	}
	if c := code(s.SetActionAutonomy(ctx, op, "api", "no.such", "shadow", "")); c != 404 {
		t.Fatalf("an unknown action: %d", c)
	}
	// Policy reads it: at propose, a plan proposes (the routine at propose too).
	_ = s.SetActionAutonomy(ctx, op, "api", rerun, "propose", "again")
	_ = s.Store.SetControl(ctx, "triage/cycle", "autonomy", "propose", "user:ada", "api")
	id := plannedCase(t, s, "", "run-1")
	if d, _, _ := s.DecidePlan(ctx, planFor(id, "run-1", "mini")); d.Outcome != policy.Propose {
		t.Fatalf("policy at the action's propose: %+v", d)
	}
	as, _ := s.Actions(ctx)
	if len(as) != 1 || as[0].Autonomy != "propose" || as[0].Max != "auto" || as[0].Record.Plans != 1 || as[0].Record.Outcomes["propose"] != 1 {
		t.Fatalf("actions %+v", as)
	}
}

func TestIrreversibleStaysAtPropose(t *testing.T) {
	ctx := context.Background()
	s := newServer(t, func(*spec.Routine) {})
	p := filepath.Join(t.TempDir(), "r.json")
	_ = os.WriteFile(p, []byte(`{"component": "c", "actions": [{"name": "disk.wipe_cache", "for_kinds": ["disk_full"],
		"params": {"host": {"kind": "target_host"}}, "risk": "medium", "reversible": "no", "pre": ["disk_above(90)"],
		"check": {"verify": "disk_below", "within": "10m"}, "limits": {"per_target_per_day": 1}, "timeout": "5m"}]}`), 0o600)
	reg, err := registry.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	s.Registry = reg
	op := Caller{Name: "operator", Role: RoleOperator}
	if err := s.SetActionAutonomy(ctx, op, "api", "disk.wipe_cache", "propose", "r"); err != nil {
		t.Fatal(err)
	}
	if p := AsProblem(s.SetActionAutonomy(ctx, op, "api", "disk.wipe_cache", "auto", "r")); p == nil || p.Code != 400 {
		t.Fatalf("an action that cannot be undone promoted to auto: %v", p)
	}
}
