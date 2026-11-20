package spec

import (
	"strings"
	"testing"
)

func loadOne(t *testing.T, d string) *Routine {
	t.Helper()
	rs, err := Load(d)
	if err != nil || len(rs) != 1 {
		t.Fatalf("%v %v", rs, err)
	}
	return rs[0]
}

// the routine's own fields are the same through both layouts, and a change to one names that field alone
func TestRoutineFieldsAreTheRoutinesOwnAndNameWhatMoved(t *testing.T) {
	head := strings.Replace(folderRoutine, "autonomy: shadow", "autonomy: record", 1)
	flat, folder := t.TempDir(), t.TempDir()
	write(t, flat, "triage/cycle.yaml", head+moveFlatBody)
	write(t, flat, "triage/playbooks/system.md", "the system text\n")
	write(t, flat, "triage/playbooks/user.md", "the user text\n")
	write(t, flat, "triage/skills/hint-x/SKILL.md", "hint x\n")
	write(t, folder, "triage/cycle/routine.yaml", head)
	write(t, folder, "triage/cycle/v1/spec.yaml", moveFolderBody)
	write(t, folder, "triage/cycle/v1/system.md", "the system text\n")
	write(t, folder, "triage/cycle/v1/user.md", "the user text\n")
	write(t, folder, "triage/skills/hint-x/SKILL.md", "hint x\n")
	a, b := loadOne(t, flat).RoutineFields(), loadOne(t, folder).RoutineFields()
	if d := DiffFields(a, b); len(d) != 0 {
		t.Fatalf("the same routine in both layouts differs: %v", d)
	}
	for _, f := range []string{"description", "enabled", "worker", "trigger", "detects", "autonomy", "max_autonomy", "budget", "freshness", "resume", "brief_section", "chain", "acceptance", "shadow_worker", "state", "repo"} {
		if _, ok := a[f]; !ok {
			t.Errorf("the snapshot has no %q", f)
		}
	}
	for _, f := range []string{"fleet", "routine", "run", "prompts", "skills", "models"} {
		if _, ok := a[f]; ok {
			t.Errorf("the snapshot holds %q, which is not a routine-level field", f)
		}
	}
	if a["budget"] != `{"day_usd":2}` || !strings.Contains(a["trigger"], `"every":"10m"`) {
		t.Fatalf("budget %s trigger %s", a["budget"], a["trigger"])
	}
	write(t, folder, "triage/cycle/routine.yaml", strings.Replace(head, "day_usd: 2", "day_usd: 3", 1))
	c := loadOne(t, folder).RoutineFields()
	if d := DiffFields(a, c); len(d) != 1 || d[0] != `budget {"day_usd":2} -> {"day_usd":3}` {
		t.Fatalf("%v", d)
	}
	if FieldsHash(a) != FieldsHash(b) || FieldsHash(a) == FieldsHash(c) {
		t.Fatal("the hash follows the fields")
	}
	if d := DiffFields(map[string]string{"worker": `"x"`}, map[string]string{"worker": `"x"`, "repo": "null"}); len(d) != 1 || d[0] != "repo (none) -> null" {
		t.Fatalf("a field only the new side has: %v", d)
	}
	if d := DiffFields(map[string]string{"worker": `"triage"`}, map[string]string{"worker": `"ops"`}); len(d) != 1 || d[0] != "worker triage -> ops" {
		t.Fatalf("a string is shown unquoted: %v", d)
	}
}
