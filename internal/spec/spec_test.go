package spec

import (
	"crypto/sha256"
	"encoding/hex"
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

// A fleet's fleet.yaml is its description, not a routine (G6); its pack hash follows its specs.
func TestLoadFleets(t *testing.T) {
	d := t.TempDir()
	write(t, d, "triage/cycle.yaml", good)
	write(t, d, "triage/fleet.yaml", "fleet: triage\ndescription: >-\n  Reads alerts\n  and errors.\n")
	routines, err := Load(d)
	if err != nil || len(routines) != 1 {
		t.Fatalf("fleet.yaml read as a routine: %v %d", err, len(routines))
	}
	fs, err := LoadFleets(d, routines)
	if err != nil {
		t.Fatal(err)
	}
	if f := fs["triage"]; f.Description != "Reads alerts and errors." || len(f.Pack) != 64 {
		t.Fatalf("the fleet: %+v", f)
	}
	pack := fs["triage"].Pack
	write(t, d, "triage/cycle.yaml", strings.Replace(good, "one pass", "one pass, changed", 1))
	routines, _ = Load(d)
	if fs2, _ := LoadFleets(d, routines); fs2["triage"].Pack == pack {
		t.Fatal("a changed spec left the pack hash as it was")
	}
	write(t, d, "triage/fleet.yaml", "fleet: ops\ndescription: x\n")
	if _, err := LoadFleets(d, routines); err == nil {
		t.Fatal("a fleet.yaml naming another fleet was accepted")
	}
	write(t, d, "triage/fleet.yaml", "fleet: triage\ndescrption: typo\n")
	if _, err := LoadFleets(d, routines); err == nil {
		t.Fatal("an unknown field was accepted")
	}
}

func TestRejects(t *testing.T) {
	cases := map[string]struct{ rel, body, want string }{
		"unknown field":               {"triage/cycle.yaml", good + "retries: 3\n", "field retries not found"},
		"fleet not its folder":        {"other/cycle.yaml", good, `fleet "triage", but the file is under other/`},
		"routine not its file name":   {"triage/pass.yaml", good, `routine "cycle", but the file is pass.yaml`},
		"bad autonomy":                {"triage/cycle.yaml", strings.Replace(good, "autonomy: shadow", "autonomy: yolo", 1), "autonomy \"yolo\""},
		"bad interval":                {"triage/cycle.yaml", strings.Replace(good, "every: 10m", "every: soon", 1), "trigger.every"},
		"interval too short":          {"triage/cycle.yaml", strings.Replace(good, "every: 10m", "every: 5s", 1), "at least 1m0s"},
		"timeout too long":            {"triage/cycle.yaml", strings.Replace(good, "timeout: 20m", "timeout: 2h", 1), "more than three intervals"},
		"always stale":                {"triage/cycle.yaml", strings.Replace(good, "freshness: 35m", "freshness: 5m", 1), "always look stale"},
		"negative budget":             {"triage/cycle.yaml", strings.Replace(good, "day_usd: 2", "day_usd: -1", 1), "must not be negative"},
		"no command":                  {"triage/cycle.yaml", strings.Replace(good, `command: "python3 x.py", `, "", 1), "run.command is required"},
		"no description":              {"triage/cycle.yaml", strings.Replace(good, "description: one pass\n", "", 1), "description is required"},
		"bad max_autonomy":            {"triage/cycle.yaml", good + "max_autonomy: yolo\n", `max_autonomy "yolo"`},
		"autonomy above its cap":      {"triage/cycle.yaml", strings.Replace(good, "autonomy: shadow", "autonomy: propose", 1) + "max_autonomy: shadow\n", "above max_autonomy record"},
		"skills on demand":            {"triage/cycle.yaml", good + "skills: {on_demand: [x]}\n", "comes with RFC-0002 §9 step 4"},
		"skill rule without when":     {"triage/cycle.yaml", good + "skills: {rule: [{load: [x]}]}\n", "non-empty when and load"},
		"skill rule without load":     {"triage/cycle.yaml", good + "skills: {rule: [{when: {class: a}}]}\n", "non-empty when and load"},
		"bad skill name":              {"triage/cycle.yaml", good + "skills: {rule: [{when: {class: a}, load: [Bad_Name]}]}\n", `skill name "Bad_Name"`},
		"bad case field":              {"triage/cycle.yaml", good + "skills: {rule: [{when: {Class: a}, load: [x]}]}\n", `field "Class"`},
		"unknown skills field":        {"triage/cycle.yaml", good + "skills: {rules: []}\n", "field rules not found"},
		"every and case":              {"triage/cycle.yaml", strings.Replace(good, "trigger: {every: 10m}", "trigger: {every: 10m, case: {kind: failed_backup}}", 1), "one of every, case, execute and check"},
		"case and check":              {"triage/cycle.yaml", strings.Replace(good, "trigger: {every: 10m}", "trigger: {case: {kind: failed_backup}, check: {verify: a_b}}", 1), "one of every, case, execute and check"},
		"a bad verify":                {"triage/cycle.yaml", strings.Replace(good, "trigger: {every: 10m}", "trigger: {check: {verify: Not-One}}", 1), "trigger.check.verify"},
		"execute and case":            {"triage/cycle.yaml", strings.Replace(good, "trigger: {every: 10m}", "trigger: {case: {kind: failed_backup}, execute: {action: github.open_pr}}", 1), "one of every, case, execute and check"},
		"a bad execute action":        {"triage/cycle.yaml", strings.Replace(good, "trigger: {every: 10m}", "trigger: {execute: {action: open_pr}}", 1), "trigger.execute.action"},
		"a check routine detecting":   {"triage/cycle.yaml", strings.Replace(strings.Replace(good, "trigger: {every: 10m}", "trigger: {check: {verify: a_b}}", 1), "freshness: 35m\n", "", 1) + "detects: [failed_backup]\n", "a monitor runs on time"},
		"a bad case kind":             {"triage/cycle.yaml", strings.Replace(good, "trigger: {every: 10m}", "trigger: {case: {kind: Failed-Backup}}", 1), "trigger.case.kind"},
		"no trigger":                  {"triage/cycle.yaml", strings.Replace(good, "trigger: {every: 10m}\n", "", 1), "trigger.every"},
		"a bad detected kind":         {"triage/cycle.yaml", good + "detects: [Disk Full]\n", "detects: \"Disk Full\""},
		"a case routine detecting":    {"triage/cycle.yaml", strings.Replace(strings.Replace(good, "trigger: {every: 10m}", "trigger: {case: {kind: failed_backup}}", 1), "freshness: 35m\n", "", 1) + "detects: [failed_backup]\n", "a monitor runs on time"},
		"a brief section, no monitor": {"triage/cycle.yaml", good + "brief_section: Search\n", "only a monitor"},
		"a brief section, bad title":  {"triage/cycle.yaml", good + "detects: [x]\nbrief_section: search-weekly\n", "not a short title"},
		"a brief section of its own":  {"triage/cycle.yaml", good + "detects: [x]\nbrief_section: Watch\n", "one of the brief's own sections"},
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

// RFC-0013 §2: one acting routine per kind and stage; the case's limit sits on the first stage's routine.
func TestCaseStages(t *testing.T) {
	onCase := func(name, trigger string) string {
		return strings.NewReplacer("fleet: triage", "fleet: ops", "routine: cycle", "routine: "+name,
			"trigger: {every: 10m}", "trigger: "+trigger, "freshness: 35m\n", "").Replace(good)
	}
	d := t.TempDir()
	write(t, d, "ops/fix.yaml", onCase("fix", "{case: {kind: code_bug, limit: {usd: 1, minutes: 90}}}"))
	write(t, d, "ops/clarify.yaml", onCase("clarify", "{case: {kind: code_bug, stage: clarify}}"))
	routines, err := Load(d)
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range routines {
		if j.ID() == "ops/clarify" && (j.OnCase() != "code_bug" || j.OnStage() != "clarify") {
			t.Fatalf("clarify acts on %q at %q", j.OnCase(), j.OnStage())
		}
		if j.ID() == "ops/fix" && (j.Trigger.Case.Limit == nil || j.Trigger.Case.Limit.Minutes != 90) {
			t.Fatalf("fix's limit %+v", j.Trigger.Case.Limit)
		}
	}
	for name, c := range map[string]struct{ trigger, want string }{
		"the same stage twice":     {"{case: {kind: code_bug, stage: clarify}}", "at stage \"clarify\" already have an acting routine"},
		"a limit on a later stage": {"{case: {kind: code_bug, stage: verify, limit: {usd: 1, minutes: 9}}}", "first-stage routine"},
		"a zero limit":             {"{case: {kind: other_bug, limit: {usd: 0, minutes: 9}}}", "both above 0"},
		"a bad stage name":         {"{case: {kind: code_bug, stage: Clarify}}", "a stage name"},
	} {
		t.Run(name, func(t *testing.T) {
			d2 := t.TempDir()
			write(t, d2, "ops/fix.yaml", onCase("fix", "{case: {kind: code_bug, limit: {usd: 1, minutes: 90}}}"))
			write(t, d2, "ops/clarify.yaml", onCase("clarify", "{case: {kind: code_bug, stage: clarify}}"))
			write(t, d2, "ops/other.yaml", onCase("other", c.trigger))
			if _, err := Load(d2); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want %q, got %v", c.want, err)
			}
		})
	}
}

// A check routine (RFC-0004 §7): runs on cases in checking, never on time; one per verify condition.
func TestCheckTrigger(t *testing.T) {
	onCheck := strings.Replace(strings.Replace(good, "trigger: {every: 10m}", "trigger: {check: {verify: backup_newer_than_execution}}", 1),
		"freshness: 35m\n", "", 1)
	d := t.TempDir()
	write(t, d, "ops/verify.yaml", strings.NewReplacer("fleet: triage", "fleet: ops", "routine: cycle", "routine: verify").Replace(onCheck))
	routines, err := Load(d)
	if err != nil {
		t.Fatal(err)
	}
	if j := routines[0]; j.OnCheck() != "backup_newer_than_execution" || j.OnCase() != "" || !j.OnEvent() || j.Every != 0 || j.Fresh != 0 {
		t.Fatalf("a check routine: verify %q case %q every %s fresh %s", j.OnCheck(), j.OnCase(), j.Every, j.Fresh)
	}
	write(t, d, "ops/verify2.yaml", strings.NewReplacer("fleet: triage", "fleet: ops", "routine: cycle", "routine: verify2").Replace(onCheck))
	if _, err := Load(d); err == nil || !strings.Contains(err.Error(), "already has a check routine") {
		t.Fatalf("two check routines for one condition: %v", err)
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
	if tri.Autonomy != AutonomyRecord {
		t.Fatalf("triage/cycle must default to record (the switch raises it with a control), got %s", tri.Autonomy)
	}
	if _, ok := ids["platform/heartbeat"]; !ok {
		t.Fatal("platform/heartbeat missing")
	}
	if j := ids["sites/site-check"]; j == nil || j.Autonomy != AutonomyRecord {
		t.Error("sites/site-check: the example site watch, read only, at record")
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
	h := `prompts:
  system: playbooks/triage.md
harness:
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

func TestPrompts(t *testing.T) {
	d := t.TempDir()
	write(t, d, "triage/playbooks/triage.md", "the playbook\n")
	write(t, d, "triage/playbooks/rules.md", "rules\n")
	write(t, d, "triage/plain.yaml", strings.Replace(good, "routine: cycle", "routine: plain", 1))
	write(t, d, "triage/cycle.yaml", good+"prompts:\n  system: playbooks/triage.md\n  rules: playbooks/rules.md\n")
	routines, err := Load(d)
	if err != nil {
		t.Fatal(err)
	}
	var j, plain *Routine
	for _, r := range routines {
		if r.Routine == "cycle" {
			j = r
		} else {
			plain = r
		}
	}
	sha := func(s string) string { x := sha256.Sum256([]byte(s)); return hex.EncodeToString(x[:]) }
	want := `{"rules":{"path":"playbooks/rules.md","sha256":"` + sha("rules\n") + `"},"system":{"path":"playbooks/triage.md","sha256":"` + sha("the playbook\n") + `"}}`
	if got := j.PromptsJSON(); got != want {
		t.Fatalf("PromptsJSON: %s\nwant %s", got, want)
	}
	if plain.PromptsJSON() != "" {
		t.Fatal("no prompts: no FLOTILLA_PROMPTS")
	}

	// a routine without prompts keeps the hash of its own bytes; one with prompts follows its files
	if x := sha256.Sum256([]byte(strings.Replace(good, "routine: cycle", "routine: plain", 1))); plain.Hash != hex.EncodeToString(x[:])[:12] {
		t.Fatalf("a routine without prompts changed its hash: %s", plain.Hash)
	}
	before := j.Hash
	write(t, d, "triage/playbooks/triage.md", "the playbook, edited\n")
	routines, _ = Load(d)
	for _, r := range routines {
		if r.Routine == "cycle" && r.Hash == before {
			t.Fatal("a changed prompt file must change the routine's hash")
		}
		if r.Routine == "plain" && r.Hash != plain.Hash {
			t.Fatal("a routine without prompts must not follow another routine's files")
		}
	}

	for name, c := range map[string]struct{ body, want string }{
		"absolute":   {"prompts:\n  system: /etc/passwd.md\n", "inside the fleet's folder"},
		"up":         {"prompts:\n  system: ../other/x.md\n", "inside the fleet's folder"},
		"inside up":  {"prompts:\n  system: a/../x.md\n", "inside the fleet's folder"},
		"not text":   {"prompts:\n  system: playbooks/triage.py\n", "inside the fleet's folder"},
		"bad role":   {"prompts:\n  System: playbooks/triage.md\n", "role"},
		"empty path": {"prompts:\n  system: \"\"\n", "inside the fleet's folder"},
	} {
		t.Run(name, func(t *testing.T) {
			dd := t.TempDir()
			write(t, dd, "triage/cycle.yaml", good+c.body)
			if _, err := Load(dd); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want an error with %q, got %v", c.want, err)
			}
		})
	}
}

func TestHarnessHooks(t *testing.T) {
	h := `prompts:
  system: playbooks/triage.md
harness:
  protocol: json
  tools: [a, b]
  gates: [g]
  limits: {max_tool_calls: 3, max_steps: 4, tool_calls_per_turn: 1}
  quirks: {bad_json_uses_turn: true, empty_retry: true, final_decision_call: true, reply_echo_chars: 600, decision_echo_chars: 400, result_chars: 1800}
  hooks:
    before_tool: [{name: deny_big}]
    after_tool:
      - {name: redact_credentials, mode: observe}
`
	d := t.TempDir()
	write(t, d, "triage/cycle.yaml", good+h)
	routines, err := Load(d)
	if err != nil {
		t.Fatal(err)
	}
	want := `"hooks":{"before_tool":[{"name":"deny_big","mode":"enforce"}],"after_tool":[{"name":"redact_credentials","mode":"observe"}]}`
	if got := routines[0].HarnessJSON(); !strings.Contains(got, want) {
		t.Fatalf("HarnessJSON: %s\nwant it to hold %s", got, want)
	}
	if !strings.Contains(strings.Join(routines[0].ExplainKeys(), " "), "harness.hooks") {
		t.Error("harness.hooks is an explain key for a harness with hooks")
	}
	for name, c := range map[string]struct{ from, to, want string }{
		"bad mode":      {"mode: observe", "mode: audit", `mode "audit" is enforce or observe`},
		"bad name":      {"name: deny_big", "name: Deny-Big", "hook name"},
		"duplicate":     {"before_tool: [{name: deny_big}]", "before_tool: [{name: deny_big}, {name: deny_big}]", "declared twice"},
		"unknown field": {"mode: observe", "mode: observe, fn: x", "field fn not found"},
	} {
		t.Run(name, func(t *testing.T) {
			dd := t.TempDir()
			write(t, dd, "triage/cycle.yaml", good+strings.Replace(h, c.from, c.to, 1))
			if _, err := Load(dd); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want an error with %q, got %v", c.want, err)
			}
		})
	}
	// a harness without hooks hands the run the JSON it always did
	write(t, d, "triage/cycle.yaml", good+strings.Replace(h, "  hooks:\n    before_tool: [{name: deny_big}]\n    after_tool:\n      - {name: redact_credentials, mode: observe}\n", "", 1))
	routines, err = Load(d)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(routines[0].HarnessJSON(), "hooks") {
		t.Fatalf("no hooks: no hooks key: %s", routines[0].HarnessJSON())
	}
}

// A harness routine without prompts loads as before: no FLOTILLA_PROMPTS, so the run loads its default.
func TestHarnessWithoutPrompts(t *testing.T) {
	d := t.TempDir()
	write(t, d, "triage/cycle.yaml", good+`harness:
  protocol: json
  tools: [a]
  gates: [g]
  limits: {max_tool_calls: 3, max_steps: 4, tool_calls_per_turn: 1}
  quirks: {bad_json_uses_turn: true, empty_retry: true, final_decision_call: true, reply_echo_chars: 600, decision_echo_chars: 400, result_chars: 1800}
`)
	routines, err := Load(d)
	if err != nil {
		t.Fatal(err)
	}
	if routines[0].PromptsJSON() != "" || routines[0].HarnessJSON() == "" {
		t.Fatal("a harness routine without prompts gets FLOTILLA_HARNESS and no FLOTILLA_PROMPTS")
	}
}

func TestCanonicalLevel(t *testing.T) {
	for in, want := range map[string]string{"shadow": "record", "record": "record", "propose": "propose", "auto": "auto", "yolo": "yolo", "": ""} {
		got, note := CanonicalLevel(in)
		if got != want || (in == "shadow") != (note == ShadowNote) {
			t.Errorf("%q: %q, %q", in, got, note)
		}
	}
}
