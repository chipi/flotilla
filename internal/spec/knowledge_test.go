package spec

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

const sopSkill = "---\nname: backup-stale\ndescription: Use when a backup is stale.\napplies_to: {\"kinds\":[\"failed_backup\"]}\nfix: [{\"action\":\"backup.rerun\"}]\nrung: 1\n---\nRerun it.\n"

const knowFleet = "fleet: triage\ndescription: x\nknowledge:\n  - name: sops\n    title: Procedures\n    path: sops/\n    format: skill\n    fields: [applies_to, rung]\n    proposals: true\n"

func knowTree(t *testing.T, fleetYAML string) string {
	t.Helper()
	d := t.TempDir()
	write(t, d, "triage/cycle.yaml", good)
	write(t, d, "triage/fleet.yaml", fleetYAML)
	write(t, d, "triage/sops/backup-stale/SKILL.md", sopSkill)
	return d
}

func loadFleets(t *testing.T, d string) (map[string]Fleet, error) {
	t.Helper()
	routines, err := Load(d)
	if err != nil {
		t.Fatal(err)
	}
	return LoadFleets(d, routines)
}

// A fleet's declared knowledge is read, checked and shown as text; its hash joins the fleet's pack, and a fleet without
// the block keeps the pack it always had.
func TestKnowledgeLoadsAndVersionsTheFleet(t *testing.T) {
	d := knowTree(t, knowFleet)
	fs, err := loadFleets(t, d)
	if err != nil {
		t.Fatal(err)
	}
	k := fs["triage"].Knowledge
	if len(k) != 1 || k[0].Path != "sops" || len(k[0].Items) != 1 {
		t.Fatalf("knowledge: %+v", k)
	}
	it := k[0].Items[0]
	if it.ID != "backup-stale" || it.Description != "Use when a backup is stale." || it.Fields["rung"] != "1" ||
		it.Fields["applies_to"] != `{"kinds":["failed_backup"]}` || it.Fields["fix"] != "" || it.Body != "Rerun it." || len(it.Hash) != 64 {
		t.Fatalf("item: %+v", it)
	}
	with := fs["triage"].Pack
	write(t, d, "triage/sops/backup-stale/SKILL.md", strings.Replace(sopSkill, "Rerun it.", "Rerun it twice.", 1))
	fs2, err := loadFleets(t, d)
	if err != nil || fs2["triage"].Pack == with {
		t.Fatalf("a changed item left the fleet's pack as it was: %v", err)
	}
	// a new file under the folder is a new version too, a loose one included
	write(t, d, "triage/sops/rules.json", "{}")
	fs3, _ := loadFleets(t, d)
	if fs3["triage"].Pack == fs2["triage"].Pack {
		t.Fatal("a loose file under the knowledge folder left the pack as it was")
	}

	// the same fleet without a knowledge block: the pack is the routines' hash alone, as before this field existed
	write(t, d, "triage/fleet.yaml", "fleet: triage\ndescription: x\n")
	routines, _ := Load(d)
	fs4, err := LoadFleets(d, routines)
	if err != nil {
		t.Fatal(err)
	}
	var hs []string
	for _, j := range routines {
		hs = append(hs, j.Routine+"="+j.Hash)
	}
	if want := sha256hex(strings.Join(hs, "\n")); fs4["triage"].Pack != want || with == want {
		t.Fatalf("pack without knowledge %s, want the routines' hash %s (with knowledge: %s)", fs4["triage"].Pack, want, with)
	}
}

func sha256hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// The repo's own fleets: each one that declares no knowledge keeps the pack hash it had before the field existed (the routines'
// hashes alone), and the two that declare it (operator, triage) load and carry their items.
func TestRepoFleetsKnowledgeAndPacks(t *testing.T) {
	dir := filepath.Join("..", "..", "fleets")
	routines, err := LoadVersions(dir)
	if err != nil {
		t.Fatal(err)
	}
	fs, err := LoadFleets(dir, routines)
	if err != nil {
		t.Fatal(err)
	}
	byFleet := map[string][]string{}
	for _, j := range routines {
		byFleet[j.Fleet] = append(byFleet[j.Fleet], j.Routine+"="+j.Hash)
	}
	plain := 0
	for name, hs := range byFleet {
		if len(fs[name].Knowledge) > 0 {
			continue
		}
		sort.Strings(hs)
		if want := sha256hex(strings.Join(hs, "\n")); fs[name].Pack != want {
			t.Errorf("fleet %s declares no knowledge and its pack moved: %s, the routines' hash is %s", name, fs[name].Pack, want)
		}
		plain++
	}
	if plain < 5 {
		t.Fatalf("only %d fleets without knowledge were compared", plain)
	}
	if k := fs["operator"].Knowledge; len(k) != 1 || k[0].Name != "sops" || !k[0].Proposals || len(k[0].Items) == 0 || k[0].Items[0].Fields["rung"] == "" {
		t.Fatalf("operator's knowledge: %+v", k)
	}
	if k := fs["triage"].Knowledge; len(k) != 2 || len(k[0].Items) == 0 || len(k[1].Items) < 4 {
		t.Fatalf("triage's knowledge: %+v", k)
	}
}

func TestKnowledgeRefusals(t *testing.T) {
	block := func(body string) string {
		return "fleet: triage\ndescription: x\nknowledge:\n  - name: sops\n    title: T\n    format: skill\n" + body
	}
	cases := map[string]struct {
		fleet string
		files map[string]string
		want  string
	}{
		"missing path":    {block("    path: nope\n"), nil, "no such folder in the fleet"},
		"parent path":     {block("    path: ../sops\n"), nil, "no .., not absolute"},
		"inner parent":    {block("    path: sops/../../x\n"), nil, "no .., not absolute"},
		"absolute path":   {block("    path: /etc\n"), nil, "no .., not absolute"},
		"no path":         {block(""), nil, "path \"\""},
		"unknown field":   {block("    path: sops\n    colour: red\n"), nil, "field colour not found"},
		"bad format":      {strings.Replace(block("    path: sops\n"), "skill", "html", 1), nil, `format "html"`},
		"declared twice":  {block("    path: sops\n") + "  - name: sops\n    title: T\n    format: skill\n    path: sops\n", nil, "declared twice"},
		"field no item":   {block("    path: sops\n    fields: [colour]\n"), nil, `field "colour": no item has it`},
		"bad field name":  {block("    path: sops\n    fields: [Rung]\n"), nil, `field "Rung"`},
		"name not folder": {block("    path: sops\n"), map[string]string{"triage/sops/backup-stale/SKILL.md": strings.Replace(sopSkill, "name: backup-stale", "name: other", 1)}, "not the folder's name"},
		"no frontmatter":  {block("    path: sops\n"), map[string]string{"triage/sops/backup-stale/SKILL.md": "just text"}, "must start with a --- frontmatter line"},
		"no body":         {block("    path: sops\n"), map[string]string{"triage/sops/backup-stale/SKILL.md": "---\nname: backup-stale\ndescription: d\n---\n"}, "no instructions"},
		"bad front line":  {block("    path: sops\n"), map[string]string{"triage/sops/backup-stale/SKILL.md": "---\nname: backup-stale\ndescription: d\nnot a pair\n---\nx\n"}, "is not `key: value`"},
		"key twice":       {block("    path: sops\n"), map[string]string{"triage/sops/backup-stale/SKILL.md": "---\nname: backup-stale\ndescription: d\nrung: 1\nrung: 2\n---\nx\n"}, "rung is given twice"},
		"no skill file":   {block("    path: sops\n"), map[string]string{"triage/sops/empty/note.txt": "x"}, "empty/SKILL.md"},
		"fields on pages": {strings.Replace(block("    path: sops\n    fields: [a]\n"), "skill", "markdown", 1), nil, "no front matter"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			d := knowTree(t, c.fleet)
			for rel, body := range c.files {
				write(t, d, rel, body)
			}
			_, err := loadFleets(t, d)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v, want it to contain %q", err, c.want)
			}
		})
	}
}

func TestKnowledgeRefusesASymlink(t *testing.T) {
	d := knowTree(t, knowFleet)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(d, "triage", "escape")); err != nil {
		t.Skip("no symlinks here")
	}
	write(t, d, "triage/fleet.yaml", strings.Replace(knowFleet, "path: sops/", "path: escape", 1))
	if _, err := loadFleets(t, d); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("a symlinked folder: %v", err)
	}
	write(t, d, "triage/fleet.yaml", knowFleet)
	if err := os.Symlink(outside, filepath.Join(d, "triage", "sops", "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := loadFleets(t, d); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("a symlink inside the folder: %v", err)
	}
}

func TestKnowledgeMarkdown(t *testing.T) {
	d := t.TempDir()
	write(t, d, "triage/cycle.yaml", good)
	write(t, d, "triage/fleet.yaml", "fleet: triage\ndescription: x\nknowledge:\n  - name: playbooks\n    title: Playbooks\n    path: playbooks\n    format: markdown\n")
	write(t, d, "triage/playbooks/triage.md", "# The triager\n\nbody <b>text</b>\n")
	write(t, d, "triage/playbooks/other.txt", "ignored")
	fs, err := loadFleets(t, d)
	if err != nil {
		t.Fatal(err)
	}
	items := fs["triage"].Knowledge[0].Items
	if len(items) != 1 || items[0].ID != "triage" || items[0].Title != "The triager" || !strings.Contains(items[0].Body, "<b>text</b>") {
		t.Fatalf("items: %+v", items)
	}
	write(t, d, "triage/playbooks/blank.md", "\n\nx")
	if _, err := loadFleets(t, d); err == nil || !strings.Contains(err.Error(), "title") {
		t.Fatalf("a page without a title line: %v", err)
	}
}
