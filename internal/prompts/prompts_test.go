package prompts

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chipi/flotilla/internal/spec"
)

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func routine(fleet, role, path string) *spec.Routine {
	return &spec.Routine{Fleet: fleet, Routine: "r", Prompts: map[string]string{role: path}}
}

func TestCheck(t *testing.T) {
	root := t.TempDir()
	specs := filepath.Join(root, "fleets")
	write(t, filepath.Join(specs, "f", "playbooks", "ok.md"), "text\n")
	write(t, filepath.Join(specs, "f", "playbooks", "empty.md"), " \n\n")
	write(t, filepath.Join(specs, "f", "playbooks", "latin1.md"), "caf\xe9\n")
	write(t, filepath.Join(specs, "f", "playbooks", "big.md"), strings.Repeat("x", MaxBytes+1))
	write(t, filepath.Join(specs, "f", "playbooks", "max.md"), strings.Repeat("x", MaxBytes))
	write(t, filepath.Join(root, "elsewhere", "secret.md"), "outside\n")
	if err := os.Symlink(filepath.Join(root, "elsewhere", "secret.md"), filepath.Join(specs, "f", "playbooks", "link.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "elsewhere"), filepath.Join(specs, "f", "linked")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(specs, "f", "playbooks", "dir.md"), 0o755); err != nil {
		t.Fatal(err)
	}

	for _, ok := range []string{"playbooks/ok.md", "playbooks/max.md"} {
		if err := Check(specs, []*spec.Routine{routine("f", "system", ok)}); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for path, want := range map[string]string{
		"playbooks/none.md":   "no such file",
		"playbooks/empty.md":  "is empty",
		"playbooks/latin1.md": "not UTF-8",
		"playbooks/big.md":    "over 65536 bytes",
		"playbooks/link.md":   "symlink",
		"linked/secret.md":    "symlink",
		"playbooks/dir.md":    "not a regular file",
		"../g/x.md":           "outside the fleet's folder",
	} {
		err := Check(specs, []*spec.Routine{routine("f", "system", path)})
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: want %q, got %v", path, want, err)
		}
	}
	// every problem is reported, with the routine that has it
	err := Check(specs, []*spec.Routine{routine("f", "system", "playbooks/none.md"), routine("f", "rules", "playbooks/empty.md")})
	if err == nil || strings.Count(err.Error(), "f/r") != 2 {
		t.Errorf("both problems reported: %v", err)
	}
}

// The example fleets declare prompts that exist, and the bugfix rules are the bytes the standalone default carries.
func TestRepoFleetsPrompts(t *testing.T) {
	specs := filepath.Join("..", "..", "fleets")
	routines, err := spec.Load(specs)
	if err != nil {
		t.Fatal(err)
	}
	if err := Check(specs, routines); err != nil {
		t.Fatal(err)
	}
	declared := 0
	for _, r := range routines {
		declared += len(r.Prompts)
	}
	if declared < 3 {
		t.Fatalf("the example triage and bugfix routines declare prompts, found %d", declared)
	}
	a, _ := os.ReadFile(filepath.Join("..", "..", "bugfix", "fleet_rules.md"))
	b, _ := os.ReadFile(filepath.Join(specs, "bugfix", "fix", "v1", "rules.md"))
	if len(a) == 0 || string(a) != string(b) {
		t.Fatal("fleets/bugfix/fix/v1/rules.md differs from bugfix/fleet_rules.md")
	}
}

// a version's station texts go through the same checks as a routine's prompts: size, UTF-8, empty, symlink, inside the fleet's folder
func TestCheckCoversStationTexts(t *testing.T) {
	root := t.TempDir()
	specs := filepath.Join(root, "fleets")
	write(t, filepath.Join(specs, "f", "r", "v1", "ok.md"), "text\n")
	write(t, filepath.Join(specs, "f", "r", "v1", "empty.md"), " \n")
	write(t, filepath.Join(root, "elsewhere", "secret.md"), "outside\n")
	if err := os.Symlink(filepath.Join(root, "elsewhere", "secret.md"), filepath.Join(specs, "f", "r", "v1", "link.md")); err != nil {
		t.Fatal(err)
	}
	mk := func(file string) *spec.Routine {
		return &spec.Routine{Fleet: "f", Routine: "r", StationText: map[string]map[string]spec.StationText{"decide": {"task": {Path: "r/v1/" + file}}}}
	}
	if err := Check(specs, []*spec.Routine{mk("ok.md")}); err != nil {
		t.Fatal(err)
	}
	for file, want := range map[string]string{"empty.md": "is empty", "link.md": "symlink", "none.md": "no such file"} {
		if err := Check(specs, []*spec.Routine{mk(file)}); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: want %q, got %v", file, want, err)
		}
	}
}
