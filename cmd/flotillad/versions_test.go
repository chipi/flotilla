package main

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chipi/flotilla/internal/spec"
	"github.com/chipi/flotilla/internal/store"
	"github.com/chipi/flotilla/internal/versions"
)

const cycleRoutine = "fleet: triage\nroutine: cycle\ndescription: d\nenabled: true\nworker: triage\ntrigger: {every: 10m}\nautonomy: shadow\nbudget: {day_usd: 2}\nfreshness: 35m\n"

func fleetsDir(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	for rel, body := range map[string]string{
		"triage/cycle/routine.yaml": cycleRoutine,
		"triage/cycle/v1/spec.yaml": "run: {command: \"python3 one.py\", timeout: 20m}\n",
		"triage/cycle/v2/spec.yaml": "run: {command: \"python3 two.py\", timeout: 20m}\n",
	} {
		p := filepath.Join(d, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return d
}

// -check says what it did not check, reads the store without writing it, and fails on the same freeze the
// start fails on.
func TestCheckVersions(t *testing.T) {
	ctx := context.Background()
	specs := fleetsDir(t)
	routines, err := spec.LoadVersions(specs)
	if err != nil {
		t.Fatal(err)
	}
	state := t.TempDir()

	plan, line, err := checkVersions(state, routines)
	if err != nil || !strings.Contains(line, "no store yet, a first deploy") || plan.States("triage/cycle") != "v1:live(new),v2:shadow(new)" {
		t.Fatalf("no store: %q %v %s", line, err, plan.States("triage/cycle"))
	}
	if _, err := os.Stat(filepath.Join(state, "flotilla.db")); !os.IsNotExist(err) {
		t.Fatalf("-check must not create the store: %v", err)
	}
	if routines[0].Autonomy != "record" || len(routines[0].Notes) != 1 {
		t.Fatalf("the old word is read as record with a note: %q %v", routines[0].Autonomy, routines[0].Notes)
	}

	// a store older than the version table: the freeze is skipped, and said
	db, err := sql.Open("sqlite", filepath.Join(state, "flotilla.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE schema_version (n INTEGER NOT NULL); INSERT INTO schema_version (n) VALUES (25)`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if _, line, err = checkVersions(state, routines); err != nil || !strings.Contains(line, "the store is at schema 25, older than 26") {
		t.Fatalf("an old store: %q %v", line, err)
	}
	if err := os.Remove(filepath.Join(state, "flotilla.db")); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(filepath.Join(state, "flotilla.db-wal"))
	_ = os.Remove(filepath.Join(state, "flotilla.db-shm"))

	// a start records the versions; -check then reads them
	st, err := store.Open(filepath.Join(state, "flotilla.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := versions.Reconcile(ctx, st, routines); err != nil {
		t.Fatal(err)
	}
	st.Close()
	plan, line, err = checkVersions(state, routines)
	if err != nil || !strings.Contains(line, "freeze checked against 2 recorded versions") || plan.States("triage/cycle") != "v1:live,v2:shadow" {
		t.Fatalf("a recorded store: %q %v %s", line, err, plan.States("triage/cycle"))
	}
	before, _ := os.ReadFile(filepath.Join(state, "flotilla.db"))

	// an edit of the live folder fails -check, naming the file; a shadow's edit does not
	if err := os.WriteFile(filepath.Join(specs, "triage/cycle/v2/spec.yaml"), []byte("run: {command: \"python3 edited.py\", timeout: 20m}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	routines, _ = spec.LoadVersions(specs)
	if _, _, err = checkVersions(state, routines); err != nil {
		t.Fatalf("a shadow's edit is allowed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(specs, "triage/cycle/v1/spec.yaml"), []byte("run: {command: \"python3 edited.py\", timeout: 20m}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	routines, _ = spec.LoadVersions(specs)
	if _, _, err = checkVersions(state, routines); err == nil || !strings.Contains(err.Error(), "versions: triage/cycle v1 is live and its folder changed") || !strings.Contains(err.Error(), "spec.yaml (changed)") {
		t.Fatalf("a live folder edit: %v", err)
	}
	if after, _ := os.ReadFile(filepath.Join(state, "flotilla.db")); string(after) != string(before) {
		t.Fatal("-check wrote to the store")
	}
}

func writeFile(t *testing.T, p, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Skills and prompts are checked for the versions that can run (live, previous, shadow) and only noted for a
// retired one: a retired v1 whose prompt file is gone must not stop the start or the deploy of a live v2.
func TestCheckTextsRefusesWhatCanRunAndNotesWhatIsRetired(t *testing.T) {
	ctx := context.Background()
	specs := fleetsDir(t)
	withPrompt := "run: {command: \"python3 one.py\", timeout: 20m}\nprompts:\n  system: system.md\n"
	writeFile(t, filepath.Join(specs, "triage/cycle/v1/spec.yaml"), withPrompt) // its system.md does not exist
	routines, err := spec.LoadVersions(specs)
	if err != nil {
		t.Fatal(err)
	}
	state := t.TempDir()
	st, err := store.Open(filepath.Join(state, "flotilla.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := versions.Reconcile(ctx, st, routines); err != nil {
		t.Fatal(err)
	}
	st.Close()
	db, err := sql.Open("sqlite", filepath.Join(state, "flotilla.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE routine_versions SET state = CASE version WHEN 1 THEN 'retired' ELSE 'live' END WHERE routine = 'triage/cycle'`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	plan, _, err := checkVersions(state, routines)
	if err != nil {
		t.Fatal(err)
	}
	notes, err := checkTexts(specs, routines, plan)
	if err != nil || len(notes) != 1 || !strings.Contains(notes[0], "triage/cycle v1 is retired and its prompts do not resolve") {
		t.Fatalf("a retired version's missing prompt is a note: notes %v err %v", notes, err)
	}

	// the live version's own missing prompt refuses
	writeFile(t, filepath.Join(specs, "triage/cycle/v2/spec.yaml"), "run: {command: \"python3 two.py\", timeout: 20m}\nprompts:\n  system: gone.md\n")
	routines, _ = spec.LoadVersions(specs)
	setHash(t, state, 2, routines[0].VersionNamed(2).Hash) // the freeze is not what this checks
	plan, _, err = checkVersions(state, routines)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := checkTexts(specs, routines, plan); err == nil || !strings.Contains(err.Error(), "prompts:") {
		t.Fatalf("a live version's missing prompt: %v", err)
	}

	// so does a new shadow's missing skill
	writeFile(t, filepath.Join(specs, "triage/cycle/v2/spec.yaml"), "run: {command: \"python3 two.py\", timeout: 20m}\n")
	writeFile(t, filepath.Join(specs, "triage/cycle/v3/spec.yaml"), "run: {command: \"python3 three.py\", timeout: 20m}\nskills:\n  rule:\n    - when: {kind: code_bug}\n      load: [no-such-skill]\n")
	routines, _ = spec.LoadVersions(specs)
	setHash(t, state, 2, routines[0].VersionNamed(2).Hash)
	plan, _, err = checkVersions(state, routines)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := checkTexts(specs, routines, plan); err == nil || !strings.Contains(err.Error(), "skills:") {
		t.Fatalf("a shadow's missing skill: %v", err)
	}
}

func setHash(t *testing.T, state string, version int, hash string) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(state, "flotilla.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE routine_versions SET hash = ? WHERE routine = 'triage/cycle' AND version = ?`, hash, version); err != nil {
		t.Fatal(err)
	}
}
