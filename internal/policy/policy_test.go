package policy

import (
	"strings"
	"testing"

	"github.com/chipi/flotilla/internal/registry"
)

func rerun() registry.Action {
	return registry.Action{Name: "backup.rerun", ForKinds: []string{"failed_backup"},
		Params: map[string]registry.Param{"host": {Kind: "target_host"}}, Risk: "low", Reversible: "regenerable",
		Pre: []string{"not_running"}, Limits: registry.Limits{PerTargetPerDay: 2}}
}

func plan() Plan {
	return Plan{CaseID: "case-1", RunID: "run-1", Action: "backup.rerun", Params: map[string]string{"host": "host-01"}}
}

// all clear, everything at auto: the plan runs.
func clear() State {
	return State{CaseKind: "failed_backup", CaseTarget: "host:host-01", Action: rerun(), ActionFound: true, RunOnRecord: true,
		RoutineAutonomy: "auto", ActionAutonomy: "auto", HostExecutorCeiling: "auto", RoutineCeiling: "auto"}
}

// One row per rule of RFC-0004 §5.1 and §5.3: the change from clear(), and what policy must decide.
func TestDecide(t *testing.T) {
	for _, c := range []struct {
		name     string
		plan     func(*Plan)
		state    func(*State)
		outcome  string
		autonomy string
		would    string
		reason   string
	}{
		{"all clear at auto", nil, nil, Auto, "auto", "", ""},
		// 1. The registry: refused, whatever the autonomy.
		{"no such action", func(p *Plan) { p.Action = "rm.everything" }, func(s *State) { s.ActionFound = false }, Refuse, "", "", "not in the registry"},
		{"another kind's action", nil, func(s *State) { s.CaseKind = "memory_growth" }, Refuse, "", "", "not for memory_growth"},
		{"retargeted", func(p *Plan) { p.Params["host"] = "model-host" }, nil, Refuse, "", "", "no retargeting"},
		{"a parameter missing", func(p *Plan) { delete(p.Params, "host") }, nil, Refuse, "", "", "missing"},
		{"a parameter added", func(p *Plan) { p.Params["force"] = "1" }, nil, Refuse, "", "", "not declared"},
		{"an enum value outside", nil, func(s *State) {
			s.Action.Params["mode"] = registry.Param{Kind: "enum", Enum: []string{"quick"}}
		}, Refuse, "", "", "parameter mode is missing"},
		{"no re-check on record", nil, func(s *State) { s.RunOnRecord = false }, Refuse, "", "", "not on the case's record"},
		{"refused even when frozen", func(p *Plan) { p.Params["host"] = "model-host" }, func(s *State) { s.Frozen = true }, Refuse, "", "", "no retargeting"},
		// 2. Autonomy, lowest wins.
		{"the routine in propose", nil, func(s *State) { s.RoutineAutonomy = "propose" }, Propose, "propose", "", ""},
		{"a new action (record)", nil, func(s *State) { s.ActionAutonomy = "record" }, Record, "record", Auto, ""},
		{"the host's ceiling", nil, func(s *State) { s.HostExecutorCeiling = "propose" }, Propose, "propose", "", ""},
		{"a tainted run", nil, func(s *State) { s.Tainted = true }, Propose, "propose", "", "tainted"},
		{"cannot be undone", nil, func(s *State) { s.Action.Reversible = "no" }, Propose, "propose", "", "cannot be undone"},
		{"high risk, though reversible", nil, func(s *State) { s.Action.Risk = "high" }, Propose, "propose", "", "high risk"},
		{"the circuit breaker", nil, func(s *State) { s.FailedHour = 3 }, Propose, "propose", "", "circuit breaker"},
		{"below the breaker", nil, func(s *State) { s.FailedHour = 2 }, Auto, "auto", "", ""},
		{"a host incident", nil, func(s *State) { s.HostNewCases = 6 }, Propose, "propose", "", "host incident"},
		{"a verified fix that came back", nil, func(s *State) { s.RecursVerified = "case-0" }, Propose, "propose", "", "came back within 7 days"},
		{"the freeze is record, not propose", nil, func(s *State) { s.Frozen = true }, Record, "record", Auto, "frozen"},
		{"an unknown autonomy word is record", nil, func(s *State) { s.ActionAutonomy = "yolo" }, Record, "record", Auto, ""},
		// 3 and 4. Defer: the case goes back to handed_over.
		{"the target locked", nil, func(s *State) { s.LockHeldBy = "case-0" }, Defer, "auto", "", "locked by case-0"},
		{"the target's daily limit", nil, func(s *State) { s.TargetToday = 2 }, Defer, "auto", "", "2 of 2"},
		{"under the daily limit", nil, func(s *State) { s.TargetToday = 1 }, Auto, "auto", "", ""},
		{"the host's hourly limit", nil, func(s *State) { s.HostHour = 3 }, Defer, "auto", "", "on this host"},
		{"the platform's hourly cap", nil, func(s *State) { s.PlatformHour = 10 }, Defer, "auto", "", "platform-wide"},
		{"the fleet STOPped", nil, func(s *State) { s.FleetStopped = true }, Defer, "auto", "", "STOPped"},
		{"an operator's hold", nil, func(s *State) { s.Holds = []string{"host:host-01 until 12:00 by user:ada"} }, Defer, "auto", "", "hold: host:host-01"},
		{"a defer at propose", nil, func(s *State) { s.RoutineAutonomy = "propose"; s.LockHeldBy = "case-0" }, Defer, "propose", "", "locked"},
		// Record records what it would have done.
		{"record that would defer", nil, func(s *State) { s.ActionAutonomy = "record"; s.LockHeldBy = "case-0" }, Record, "record", Defer, "locked"},
		{"record that would propose", nil, func(s *State) { s.ActionAutonomy = "record"; s.Tainted = true }, Record, "record", Propose, ""},
	} {
		p, s := plan(), clear()
		s.Action.Params = map[string]registry.Param{"host": {Kind: "target_host"}}
		if c.plan != nil {
			c.plan(&p)
		}
		if c.state != nil {
			c.state(&s)
		}
		d := Decide(p, s, Limits{})
		if d.Outcome != c.outcome || (c.autonomy != "" && d.Autonomy != c.autonomy) || d.Would != c.would {
			t.Errorf("%s: outcome %s autonomy %s would %q, want %s %s %q (%v)", c.name, d.Outcome, d.Autonomy, d.Would, c.outcome, c.autonomy, c.would, d.Reasons)
		}
		if c.reason != "" && !strings.Contains(strings.Join(d.Reasons, "; "), c.reason) {
			t.Errorf("%s: no reason with %q in %v", c.name, c.reason, d.Reasons)
		}
	}
}

func TestPlanHashBindsTheExactPlan(t *testing.T) {
	a := plan()
	b := plan()
	b.RunID = "run-2" // the run that planned it is not part of what is approved
	if a.Hash() != b.Hash() {
		t.Fatal("the hash changed with the run")
	}
	for name, edit := range map[string]func(*Plan){
		"case":      func(p *Plan) { p.CaseID = "case-2" },
		"action":    func(p *Plan) { p.Action = "backup.rerun_other" },
		"parameter": func(p *Plan) { p.Params["host"] = "model-host" },
		"added":     func(p *Plan) { p.Params["x"] = "" },
	} {
		c := plan()
		edit(&c)
		if c.Hash() == a.Hash() {
			t.Errorf("a changed %s keeps the hash", name)
		}
	}
	// Parameter order never matters; a separator inside a value cannot fake another plan.
	x := Plan{CaseID: "c", Action: "a.b", Params: map[string]string{"p": "1", "q": "2"}}
	y := Plan{CaseID: "c", Action: "a.b", Params: map[string]string{"p": "1\x00q=2"}}
	if x.Hash() == y.Hash() {
		t.Fatal("two plans with one hash")
	}
}

func TestHostOf(t *testing.T) {
	if HostOf("host:host-01") != "host-01" || HostOf("container:host-01/x") != "" {
		t.Fatal("HostOf")
	}
}

// Which ceiling caps which executor (ADR-0017): a host action stops at the host executor's, a routine action at the
// routine-action ceiling, a repo change at neither (propose by kind), and the other kind's ceiling is never read.
func TestCeilingsCapTheirOwnExecutor(t *testing.T) {
	routineAct := func() registry.Action {
		return registry.Action{Name: "crm.update_field", Executor: registry.ExecRoutine, ForKinds: []string{"crm_fix"},
			Params: map[string]registry.Param{"to": {Kind: "text", Max: 20}}, Risk: "low", Reversible: "yes", Limits: registry.Limits{PerTargetPerDay: 2}}
	}
	repoAct := func() registry.Action {
		a := routineAct()
		a.Name, a.Executor = "github.open_pr", registry.ExecRepo
		return a
	}
	for _, c := range []struct {
		name                string
		act                 func() registry.Action
		irreversible        bool
		hostCeil, routCeil  string
		earned, routineAuto string
		outcome             string
	}{
		{"host action, host ceiling propose, routine ceiling auto", rerun, false, "propose", "auto", "auto", "auto", Propose},
		{"host action, host ceiling auto", rerun, false, "auto", "propose", "auto", "auto", Auto},
		{"routine action, routine ceiling auto, earned auto", routineAct, false, "propose", "auto", "auto", "auto", Auto},
		{"routine action, routine ceiling propose (the default)", routineAct, false, "auto", "propose", "auto", "auto", Propose},
		{"routine action, not earned", routineAct, false, "auto", "auto", "propose", "auto", Propose},
		{"routine action, new action is record", routineAct, false, "auto", "auto", "record", "auto", Record},
		{"routine action, its routine at propose", routineAct, false, "auto", "auto", "auto", "propose", Propose},
		{"routine action, irreversible", routineAct, true, "auto", "auto", "auto", "auto", Propose},
		{"routine action, an unset ceiling is record", routineAct, false, "auto", "", "auto", "auto", Record},
		{"repo change, every ceiling auto", repoAct, false, "auto", "auto", "auto", "auto", Propose},
	} {
		p, s := plan(), clear()
		a := c.act()
		if c.irreversible {
			a.Reversible = "no"
		}
		s.Action = a
		p.Action = a.Name
		if a.ByRoutine() {
			s.CaseKind, s.CaseTarget = "crm_fix", "crm:contact/c-1/email"
			p.Params = map[string]string{"to": "x"}
		}
		s.HostExecutorCeiling, s.RoutineCeiling, s.ActionAutonomy, s.RoutineAutonomy = c.hostCeil, c.routCeil, c.earned, c.routineAuto
		d := Decide(p, s, Limits{})
		if d.Outcome != c.outcome {
			t.Errorf("%s: outcome %s (autonomy %s), want %s: %v", c.name, d.Outcome, d.Autonomy, c.outcome, d.Reasons)
		}
		if a.Repo() && d.Outcome == Auto {
			t.Errorf("%s: a repo change reached auto", c.name)
		}
	}
}

// The reason's wording is what the Case page reads back, and recorded plans carry the host one.
func TestCeilingReasons(t *testing.T) {
	p, s := plan(), clear()
	if r := strings.Join(Decide(p, s, Limits{}).Reasons, "; "); !strings.Contains(r, "routine auto, action auto, host ceiling auto") {
		t.Errorf("host action: %s", r)
	}
	s.Action.Executor = registry.ExecRoutine
	s.Action.Params = map[string]registry.Param{"host": {Kind: "text", Max: 20}}
	if r := strings.Join(Decide(p, s, Limits{}).Reasons, "; "); !strings.Contains(r, "routine-action ceiling auto") || strings.Contains(r, "host ceiling") {
		t.Errorf("routine action: %s", r)
	}
	s.Action.Executor = registry.ExecRepo
	if r := strings.Join(Decide(p, s, Limits{}).Reasons, "; "); strings.Contains(r, "ceiling") {
		t.Errorf("repo change: %s", r)
	}
}
