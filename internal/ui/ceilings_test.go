package ui

import (
	"strings"
	"testing"

	"github.com/chipi/flotilla/internal/store"
)

// The Autonomy and Hosts pages say which cap applies to which kind of action: a host action stops at its host's cap, a routine action at
// flotillad.yaml's routine_actions.max_autonomy (propose until an installation opts in).
func TestCeilingsOnAutonomyAndHosts(t *testing.T) {
	f := setup(t)
	op := f.addOperator()
	ck, _ := f.csrfOf(op)
	for _, c := range []struct{ option, want string }{{"", "propose"}, {"auto", "auto"}} {
		f.ui.API.RoutineActionMax = c.option
		_, page := f.do("GET", "/autonomy", ck, nil, nil)
		for _, s := range []string{"Autonomy caps", "Host actions", `<span>Routine actions</span><span class="level level--` + c.want + `">` + c.want + `</span>`} {
			if !strings.Contains(page, s) {
				t.Errorf("option %q: the Autonomy page lacks %q", c.option, s)
			}
		}
		_, hosts := f.do("GET", "/hosts", ck, nil, nil)
		if !strings.Contains(hosts, "routine actions (CRM, campaigns, mail) have their own cap, "+c.want+" here") || !strings.Contains(hosts, "Host autonomy cap") {
			t.Errorf("option %q: the Hosts page does not say the routine-action cap is %s", c.option, c.want)
		}
	}
}

// The Case page reads policy's reason back: recorded plans carry "host ceiling", a routine action's says "routine-action ceiling", a
// repo change's has no ceiling.
func TestPlanLevelsReadsEveryWording(t *testing.T) {
	for _, c := range []struct {
		reason                         string
		routine, action, host, routAct string
	}{
		{"autonomy: routine propose, action auto, host ceiling propose", "propose", "auto", "propose", ""},
		{"autonomy: routine auto, action auto, routine-action ceiling auto", "auto", "auto", "", "auto"},
		{"autonomy: routine auto, action auto", "auto", "auto", "", ""},
	} {
		r, a, h, ra, ok := planLevels(store.PlanRecord{Reasons: []string{"registry: x", c.reason}})
		if !ok || r != c.routine || a != c.action || h != c.host || ra != c.routAct {
			t.Errorf("%q: %q %q %q %q %v", c.reason, r, a, h, ra, ok)
		}
	}
}
