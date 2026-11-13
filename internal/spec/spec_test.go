package spec

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const good = `fleet: triage
routine: cycle
description: one pass
enabled: true
worker: triage
trigger: {every: 10m}
run: {command: "python3 x.py", workdir: signal-fleet/mvp, timeout: 20m}
autonomy: shadow
budget: {day_usd: 2}
freshness: 35m
`

func write(t *testing.T, dir, rel, body string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadGood(t *testing.T) {
	d := t.TempDir()
	write(t, d, "triage/cycle.yaml", good)
	routines, err := Load(d)
	if err != nil {
		t.Fatal(err)
	}
	j := routines[0]
	if j.ID() != "triage/cycle" || j.Every.String() != "10m0s" || j.Limit.String() != "20m0s" || len(j.Hash) != 12 {
		t.Fatalf("got %+v", j)
	}
}

func TestRejects(t *testing.T) {
	cases := map[string]struct{ rel, body, want string }{
		"unknown field":             {"triage/cycle.yaml", good + "retries: 3\n", "field retries not found"},
		"fleet not its folder":      {"other/cycle.yaml", good, `fleet "triage", but the file is under other/`},
		"routine not its file name": {"triage/pass.yaml", good, `routine "cycle", but the file is pass.yaml`},
		"bad autonomy":              {"triage/cycle.yaml", strings.Replace(good, "autonomy: shadow", "autonomy: yolo", 1), "autonomy \"yolo\""},
		"bad interval":              {"triage/cycle.yaml", strings.Replace(good, "every: 10m", "every: soon", 1), "trigger.every"},
		"interval too short":        {"triage/cycle.yaml", strings.Replace(good, "every: 10m", "every: 5s", 1), "at least 1m0s"},
		"timeout too long":          {"triage/cycle.yaml", strings.Replace(good, "timeout: 20m", "timeout: 2h", 1), "more than three intervals"},
		"always stale":              {"triage/cycle.yaml", strings.Replace(good, "freshness: 35m", "freshness: 5m", 1), "always look stale"},
		"negative budget":           {"triage/cycle.yaml", strings.Replace(good, "day_usd: 2", "day_usd: -1", 1), "must not be negative"},
		"no command":                {"triage/cycle.yaml", strings.Replace(good, `command: "python3 x.py", `, "", 1), "run.command is required"},
		"no description":            {"triage/cycle.yaml", strings.Replace(good, "description: one pass\n", "", 1), "description is required"},
		"bad max_autonomy":          {"triage/cycle.yaml", good + "max_autonomy: yolo\n", `max_autonomy "yolo"`},
		"autonomy above its cap":    {"triage/cycle.yaml", strings.Replace(good, "autonomy: shadow", "autonomy: propose", 1) + "max_autonomy: shadow\n", "above max_autonomy shadow"},
		"skills on demand":          {"triage/cycle.yaml", good + "skills: {on_demand: [x]}\n", "comes with RFC-0002 §9 step 4"},
		"skill rule without when":   {"triage/cycle.yaml", good + "skills: {rule: [{load: [x]}]}\n", "non-empty when and load"},
		"skill rule without load":   {"triage/cycle.yaml", good + "skills: {rule: [{when: {class: a}}]}\n", "non-empty when and load"},
		"bad skill name":            {"triage/cycle.yaml", good + "skills: {rule: [{when: {class: a}, load: [Bad_Name]}]}\n", `skill name "Bad_Name"`},
		"bad case field":            {"triage/cycle.yaml", good + "skills: {rule: [{when: {Class: a}, load: [x]}]}\n", `field "Class"`},
		"unknown skills field":      {"triage/cycle.yaml", good + "skills: {rules: []}\n", "field rules not found"},
		"every and case":            {"triage/cycle.yaml", strings.Replace(good, "trigger: {every: 10m}", "trigger: {every: 10m, case: {kind: failed_backup}}", 1), "every or case, not both"},
		"a bad case kind":           {"triage/cycle.yaml", strings.Replace(good, "trigger: {every: 10m}", "trigger: {case: {kind: Failed-Backup}}", 1), "trigger.case.kind"},
		"no trigger":                {"triage/cycle.yaml", strings.Replace(good, "trigger: {every: 10m}\n", "", 1), "trigger.every"},
		"a bad detected kind":       {"triage/cycle.yaml", good + "detects: [Disk Full]\n", "detects: \"Disk Full\""},
		"a case routine detecting":  {"triage/cycle.yaml", strings.Replace(strings.Replace(good, "trigger: {every: 10m}", "trigger: {case: {kind: failed_backup}}", 1), "freshness: 35m\n", "", 1) + "detects: [failed_backup]\n", "a monitor runs on time"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			d := t.TempDir()
			write(t, d, c.rel, c.body)
			_, err := Load(d)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want error containing %q, got %v", c.want, err)
			}
		})
	}
}

func TestSkillsJSON(t *testing.T) {
	d := t.TempDir()
	write(t, d, "triage/cycle.yaml", good+"skills:\n  rule:\n    - {when: {class: a}, load: [s-a, s-b]}\n    - {when: {class: b}, load: [s-a]}\n")
	routines, err := Load(d)
	if err != nil {
		t.Fatal(err)
	}
	j := routines[0]
	if got := j.SkillsJSON(); got != `{"rule":[{"when":{"class":"a"},"load":["s-a","s-b"]},{"when":{"class":"b"},"load":["s-a"]}]}` {
		t.Fatalf("SkillsJSON: %s", got)
	}
	if got := strings.Join(j.SkillNames(), ","); got != "s-a,s-b" {
		t.Fatalf("SkillNames: %s", got)
	}
	write(t, d, "triage/cycle.yaml", good)
	routines, _ = Load(d)
	if routines[0].SkillsJSON() != "" {
		t.Fatal("no rules: no FLOTILLA_SKILLS")
	}
}

func TestOneBadSpecFailsTheLoad(t *testing.T) {
	d := t.TempDir()
	write(t, d, "triage/cycle.yaml", good)
	write(t, d, "triage/broken.yaml", "fleet: triage\njob: broken\n")
	if _, err := Load(d); err == nil {
		t.Fatal("a bad spec must fail the whole load, not drop that routine")
	}
}

// A routine on cases (RFC-0004 §3.7): no interval, no freshness, one acting routine per kind.
func TestCaseTrigger(t *testing.T) {
	onCase := strings.Replace(strings.Replace(good, "trigger: {every: 10m}", "trigger: {case: {kind: failed_backup}}", 1),
		"freshness: 35m\n", "", 1)
	d := t.TempDir()
	write(t, d, "ops/rerun.yaml", strings.NewReplacer("fleet: triage", "fleet: ops", "routine: cycle", "routine: rerun").Replace(onCase))
	routines, err := Load(d)
	if err != nil {
		t.Fatal(err)
	}
	if j := routines[0]; j.OnCase() != "failed_backup" || j.Every != 0 || j.Fresh != 0 {
		t.Fatalf("a routine on cases: kind %q every %s fresh %s", j.OnCase(), j.Every, j.Fresh)
	}
	write(t, d, "ops/rerun2.yaml", strings.NewReplacer("fleet: triage", "fleet: ops", "routine: cycle", "routine: rerun2").Replace(onCase))
	if _, err := Load(d); err == nil || !strings.Contains(err.Error(), "already have an acting routine") {
		t.Fatalf("two acting routines for one kind: %v", err)
	}
}

func TestEmptyDir(t *testing.T) {
	if _, err := Load(t.TempDir()); err == nil || !strings.Contains(err.Error(), "no routine specs") {
		t.Fatalf("got %v", err)
	}
}

// The specs shipped in this repo must load: a broken one is a broken deploy.
func TestRepoSpecs(t *testing.T) {
	routines, err := Load(filepath.Join("..", "..", "fleets"))
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]*Routine{}
	for _, j := range routines {
		ids[j.ID()] = j
	}
	tri, ok := ids["triage/cycle"]
	if !ok {
		t.Fatal("triage/cycle missing")
	}
	if tri.Autonomy != AutonomyShadow {
		t.Fatalf("triage/cycle must default to shadow (the switch raises it with a control), got %s", tri.Autonomy)
	}
	if _, ok := ids["platform/heartbeat"]; !ok {
		t.Fatal("platform/heartbeat missing")
	}
	// harness-default.json is what triage applies without flotillad (eval runs): the specs' harness
	raw, err := os.ReadFile(filepath.Join("..", "..", "fleets", "triage", "harness-default.json"))
	if err != nil {
		t.Fatal(err)
	}
	var def, got map[string]any
	if err := json.Unmarshal(raw, &def); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"triage/cycle", "triage/cycle-shadow"} {
		if err := json.Unmarshal([]byte(ids[id].HarnessJSON()), &got); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		if !reflect.DeepEqual(got, def) {
			t.Errorf("%s: harness differs from harness-default.json:\n spec    %v\n default %v", id, got, def)
		}
	}
}

func TestHarness(t *testing.T) {
	h := `harness:
  protocol: json
  tools: [a, b]
  gates: [g]
  limits: {max_tool_calls: 3, max_steps: 4, tool_calls_per_turn: 1}
  quirks: {bad_json_uses_turn: true, empty_retry: true, final_decision_call: true, reply_echo_chars: 600, decision_echo_chars: 400, result_chars: 1800}
`
	d := t.TempDir()
	write(t, d, "triage/cycle.yaml", good+h)
	routines, err := Load(d)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(routines[0].HarnessJSON(), `"limits":{"max_tool_calls":3,"max_steps":4,"tool_calls_per_turn":1}`) {
		t.Fatalf("HarnessJSON: %s", routines[0].HarnessJSON())
	}
	for name, c := range map[string]struct{ from, to, want string }{
		"protocol":           {"protocol: json", "protocol: native", `harness.protocol "native"`},
		"steps not calls+1":  {"max_steps: 4", "max_steps: 5", "max_steps = max_tool_calls + 1"},
		"two tools per turn": {"tool_calls_per_turn: 1", "tool_calls_per_turn: 2", "tool_calls_per_turn 1"},
		"quirk off":          {"empty_retry: true", "empty_retry: false", "until RFC-0002 §9 step 6"},
		"no gates":           {"gates: [g]", "gates: []", "tools and gates are required"},
		"zero echo":          {"reply_echo_chars: 600", "reply_echo_chars: 0", "must be positive"},
		"unknown field":      {"protocol: json", "protocol: json\n  model: x", "field model not found"},
	} {
		t.Run(name, func(t *testing.T) {
			dd := t.TempDir()
			write(t, dd, "triage/cycle.yaml", good+strings.Replace(h, c.from, c.to, 1))
			if _, err := Load(dd); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want an error with %q, got %v", c.want, err)
			}
		})
	}
	write(t, d, "triage/cycle.yaml", good)
	routines, _ = Load(d)
	if routines[0].HarnessJSON() != "" {
		t.Fatal("no harness: no FLOTILLA_HARNESS")
	}
}
