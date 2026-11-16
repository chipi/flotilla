package spec

import (
	"strings"
	"testing"
)

const explained = good + `explain:
  trigger: Every ten minutes.
  dedup: An issue decided before is a recurrence.
steps:
  - {id: dedup, lane: sort, who: code, title: already decided, short: no model, explain: Written as a recurrence., count: recurrences}
chain:
  - {routine: bugfix/intake, via: a GitHub issue}
`

const intake = `fleet: bugfix
routine: intake
description: reads issues
enabled: true
worker: bugfix
trigger: {every: 1h}
run: {command: "python3 i.py", timeout: 2m}
autonomy: shadow
freshness: 3h
`

func TestExplainStepsChain(t *testing.T) {
	d := t.TempDir()
	write(t, d, "triage/cycle.yaml", explained)
	write(t, d, "bugfix/intake.yaml", intake)
	rs, err := Load(d)
	if err != nil {
		t.Fatal(err)
	}
	var j *Routine
	for _, r := range rs {
		if r.ID() == "triage/cycle" {
			j = r
		}
	}
	if j.Explain["dedup"] == "" || len(j.Steps) != 1 || j.Steps[0].Count != "recurrences" || j.Chain[0].Routine != "bugfix/intake" {
		t.Fatalf("got %+v %+v %+v", j.Explain, j.Steps, j.Chain)
	}
}

// Each mistake an explanation can make is refused, so a typo never hides a text.
func TestExplainRejects(t *testing.T) {
	for name, c := range map[string]struct{ body, want string }{
		"unknown key":            {good + "explain: {trgger: x}\n", `"trgger" explains no block`},
		"harness key, none":      {good + "explain: {harness.tools: x}\n", `"harness.tools" explains no block`},
		"gate not in harness":    {good + "explain: {gate.intent: x}\n", `"gate.intent" explains no block`},
		"empty text":             {good + "explain: {trigger: \" \"}\n", "explain.trigger: a text of 1 to 600"},
		"too long":               {good + "explain: {trigger: " + strings.Repeat("x", 601) + "}\n", "explain.trigger: a text of 1 to 600"},
		"lane":                   {good + "steps: [{id: a, lane: middle, who: code, title: t, explain: e}]\n", `steps[a].lane "middle"`},
		"who":                    {good + "steps: [{id: a, lane: sort, who: robot, title: t, explain: e}]\n", `steps[a].who "robot"`},
		"count":                  {good + "steps: [{id: a, lane: sort, who: code, title: t, explain: e, count: everything}]\n", `steps[a].count "everything"`},
		"gate count, no harness": {good + "steps: [{id: a, lane: gates, who: code, title: t, explain: e, count: \"gate:edge\"}]\n", `not one of the harness's gates`},
		"step twice":             {good + "steps: [{id: a, lane: sort, who: code, title: t, explain: e}, {id: a, lane: sort, who: code, title: t, explain: e}]\n", `id "a" named twice`},
		"step is a field":        {good + "steps: [{id: worker, lane: sort, who: code, title: t, explain: e}]\n", `"worker" is a spec part's own key`},
		"no title":               {good + "steps: [{id: a, lane: sort, who: code, explain: e}]\n", "a title and an explain are required"},
		"unknown step field":     {good + "steps: [{id: a, lane: sort, who: code, title: t, explain: e, colour: red}]\n", "colour"},
		"chain id":               {good + "chain: [{routine: intake}]\n", `chain[0].routine "intake"`},
		"chain self":             {good + "chain: [{routine: triage/cycle}]\n", "does not follow itself"},
		"chain missing":          {good + "chain: [{routine: bugfix/nothing}]\n", "bugfix/nothing does not exist"},
	} {
		d := t.TempDir()
		write(t, d, "triage/cycle.yaml", c.body)
		_, err := Load(d)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: want %q, got %v", name, c.want, err)
		}
	}
}

// A gate's explanation is allowed exactly for the harness's gates.
func TestExplainGateKeys(t *testing.T) {
	j := &Routine{Harness: &Harness{Gates: []string{"intent", "edge"}}, Steps: []Step{{ID: "dedup"}}}
	keys := strings.Join(j.ExplainKeys(), " ")
	for _, want := range []string{"gate.intent", "gate.edge", "harness.tools", "dedup", "trigger"} {
		if !strings.Contains(keys, want) {
			t.Errorf("%s not among %s", want, keys)
		}
	}
	if strings.Contains(keys, "gate.cleanup") || strings.Contains(keys, "skills") {
		t.Errorf("keys for parts this spec does not set: %s", keys)
	}
}
