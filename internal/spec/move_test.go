package spec

import (
	"strings"
	"testing"
)

const moveFlatBody = `run: {command: "python3 x.py", workdir: signal-fleet/mvp, timeout: 20m}
skills:
  rule:
    - when: {class: external}
      load: [hint-x]
prompts:
  system: playbooks/triage.md
  user: playbooks/user.md
models: [triage-flash]
`

const moveFolderBody = `run: {command: "python3 x.py", workdir: signal-fleet/mvp, timeout: 20m}
skills:
  rule:
    - when: {class: external}
      load: [hint-x]
prompts:
  system: system.md
  user: user.md
models: [triage-flash]
`

// moveProof loads a flat routine and the same routine in the folder layout, and proves the move from the flat v1's manifest, the way the
// start does: the flat one is what was recorded, the folder one is what is found. edit may change the folder side before it loads.
func moveProof(t *testing.T, edit func(d string)) MoveProof {
	t.Helper()
	flat := t.TempDir()
	write(t, flat, "triage/cycle.yaml", strings.Replace(folderRoutine, "autonomy: shadow", "autonomy: record", 1)+moveFlatBody)
	write(t, flat, "triage/playbooks/triage.md", "the system text\n")
	write(t, flat, "triage/playbooks/user.md", "the user text\n")
	write(t, flat, "triage/skills/hint-x/SKILL.md", "hint x\n")
	fj, err := Load(flat)
	if err != nil {
		t.Fatal(err)
	}
	folder := t.TempDir()
	write(t, folder, "triage/cycle/routine.yaml", strings.Replace(folderRoutine, "autonomy: shadow", "autonomy: record", 1))
	write(t, folder, "triage/cycle/v1/spec.yaml", moveFolderBody)
	write(t, folder, "triage/cycle/v1/system.md", "the system text\n")
	write(t, folder, "triage/cycle/v1/user.md", "the user text\n")
	write(t, folder, "triage/skills/hint-x/SKILL.md", "hint x\n")
	if edit != nil {
		edit(folder)
	}
	nj, err := Load(folder)
	if err != nil {
		t.Fatal(err)
	}
	return nj[0].ProveMoved(nj[0].Versions[0], fj[0].Versions[0].Manifest)
}

func TestProveMovedAcceptsAPureMove(t *testing.T) {
	p := moveProof(t, nil)
	if !p.Unchanged() {
		t.Fatalf("a pure move: %+v", p)
	}
	got := strings.Join(p.Compared, ", ")
	for _, want := range []string{"run", "skills", "models", "prompt-file:playbooks/triage.md -> v1/system.md", "prompt-file:playbooks/user.md -> v1/user.md", "prompts", "skill-file:hint-x"} {
		if !strings.Contains(got, want) {
			t.Errorf("compared lacks %q: %s", want, got)
		}
	}
}

// Each guard has a case that names the key or file; a comparison that skipped it would accept the change.
func TestProveMovedRefusesAnyDifferenceAndNamesIt(t *testing.T) {
	for _, c := range []struct {
		name string
		edit func(d string)
		want string
	}{
		{"a changed run", func(d string) {
			write(t, d, "triage/cycle/v1/spec.yaml", strings.Replace(moveFolderBody, "x.py", "y.py", 1))
		}, "run (changed)"},
		{"a changed model list", func(d string) {
			write(t, d, "triage/cycle/v1/spec.yaml", strings.Replace(moveFolderBody, "triage-flash", "triage-pro", 1))
		}, "models (changed)"},
		{"a dropped key", func(d string) {
			write(t, d, "triage/cycle/v1/spec.yaml", strings.Replace(moveFolderBody, "models: [triage-flash]\n", "", 1))
		}, "models (missing)"},
		{"an added key", func(d string) {
			write(t, d, "triage/cycle/v1/spec.yaml", moveFolderBody+"explain:\n  run: why\n")
		}, "explain (added)"},
		{"a changed prompt byte", func(d string) { write(t, d, "triage/cycle/v1/system.md", "the system text!\n") },
			"prompt-file:v1/system.md (changed"},
		{"a prompt role pointing at the other file", func(d string) {
			write(t, d, "triage/cycle/v1/spec.yaml", strings.Replace(moveFolderBody, "system: system.md\n  user: user.md", "system: user.md\n  user: system.md", 1))
		}, "prompts (changed: a role points at another file)"},
		{"a dropped prompt", func(d string) {
			write(t, d, "triage/cycle/v1/spec.yaml", strings.Replace(moveFolderBody, "  user: user.md\n", "", 1))
		}, "prompt-file:playbooks/user.md (missing)"},
		{"a changed skill byte", func(d string) { write(t, d, "triage/skills/hint-x/SKILL.md", "hint x!\n") }, "skill-file:hint-x (changed)"},
		{"a station", func(d string) {
			write(t, d, "triage/cycle/v1/spec.yaml", `run: {command: "python3 x.py", workdir: signal-fleet/mvp, timeout: 20m}
stations:
  decide: {harness: flotilla, model: triage-flash, tools: all, tool_names: [], gates: [], prompts: {system: system.md}}
`)
		}, "stations (a flat spec has none)"},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := moveProof(t, c.edit)
			if p.Unchanged() || !strings.Contains(strings.Join(p.Differs, "; "), c.want) {
				t.Fatalf("want a difference naming %q, got %+v", c.want, p)
			}
		})
	}
}

// the routine's own fields are not in the recorded manifest and are not part of the freeze: a move that changes one is still the same version
func TestProveMovedIgnoresRoutineFields(t *testing.T) {
	p := moveProof(t, func(d string) {
		write(t, d, "triage/cycle/routine.yaml", strings.Replace(strings.Replace(folderRoutine, "autonomy: shadow", "autonomy: record", 1), "day_usd: 2", "day_usd: 9", 1))
	})
	if !p.Unchanged() {
		t.Fatalf("%+v", p)
	}
}
