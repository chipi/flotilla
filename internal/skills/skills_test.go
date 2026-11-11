package skills

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/chipi/flotilla/internal/spec"
)

const good = "---\nname: %s\ndescription: Use when testing.\n---\n\nDo the thing.\n"

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func routine(fleet string, names ...string) *spec.Routine {
	r := &spec.Routine{Fleet: fleet, Routine: "r"}
	for _, n := range names {
		r.Skills.Rule = append(r.Skills.Rule, spec.SkillRule{When: map[string]string{"class": n}, Load: []string{n}})
	}
	return r
}

func TestCheck(t *testing.T) {
	root := t.TempDir()
	specs := filepath.Join(root, "fleets")
	skill := func(layer, name, body string) {
		dir := filepath.Join(root, "skills", name)
		if layer == "fleet" {
			dir = filepath.Join(specs, "f", "skills", name)
		}
		write(t, filepath.Join(dir, "SKILL.md"), strings.ReplaceAll(body, "%s", name))
	}
	skill("fleet", "in-fleet", good)
	skill("flotilla", "in-default", good)
	skill("fleet", "in-both", good)
	skill("flotilla", "in-both", good)
	skill("fleet", "wrong-name", "---\nname: other\ndescription: d\n---\nx\n")
	skill("fleet", "no-desc", "---\nname: %s\n---\nx\n")
	skill("fleet", "no-body", "---\nname: %s\ndescription: d\n---\n\n  \n")
	skill("fleet", "unclosed", "---\nname: %s\ndescription: d\nx\n")
	skill("fleet", "too-big", good+strings.Repeat("x", MaxBytes))
	write(t, filepath.Join(root, "elsewhere", "SKILL.md"), strings.ReplaceAll(good, "%s", "linked"))
	if err := os.Symlink(filepath.Join(root, "elsewhere"), filepath.Join(specs, "f", "skills", "linked")); err != nil {
		t.Fatal(err)
	}

	if err := Check(specs, []*spec.Routine{routine("f", "in-fleet", "in-default")}); err != nil {
		t.Fatalf("fleet and default layers: %v", err)
	}
	for name, want := range map[string]string{
		"in-both": "more than one layer", "missing": "in no layer", "wrong-name": "not the folder's name",
		"no-desc": "description", "no-body": "no instructions", "unclosed": "not closed", "too-big": "never trimmed",
		"linked": "symlink",
	} {
		err := Check(specs, []*spec.Routine{routine("f", name)})
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: want an error with %q, got %v", name, want, err)
		}
	}
	if err := Check(specs, []*spec.Routine{routine("other-fleet", "in-fleet")}); err == nil {
		t.Error("another fleet's skill must not be found")
	}
}

// The repo's own specs and skills: every skill the specs name passes the check, and the triage fleet's
// default-rules.json (what triage applies without flotillad) is the spec's rules.
func TestRepoSkills(t *testing.T) {
	specs := filepath.Join("..", "..", "fleets")
	routines, err := spec.Load(specs)
	if err != nil {
		t.Fatal(err)
	}
	if err := Check(specs, routines); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(specs, "triage", "skills", "default-rules.json"))
	if err != nil {
		t.Fatal(err)
	}
	var def []spec.SkillRule
	if err := json.Unmarshal(raw, &def); err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, r := range routines {
		if r.Fleet != "triage" || r.Routine == "" || len(r.Skills.Rule) == 0 {
			continue
		}
		n++
		if !reflect.DeepEqual(r.Skills.Rule, def) {
			t.Errorf("%s: skills.rule differs from default-rules.json", r.ID())
		}
	}
	if n < 2 {
		t.Errorf("want triage/cycle and triage/cycle-shadow with rules, found %d", n)
	}
}
