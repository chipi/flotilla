package spec

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func shippedCatalogue(t *testing.T) *Catalogue {
	t.Helper()
	c, err := LoadExamples(filepath.Join("..", "..", "fleets"))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// The examples the repo ships load as a catalogue: every id carries the prefix, every chain link names another example, and the folder's
// own fleet descriptions are kept.
func TestRepoExamplesLoadAsACatalogue(t *testing.T) {
	c := shippedCatalogue(t)
	ex := c.Examples()
	if len(ex) < 30 {
		t.Fatalf("%d examples", len(ex))
	}
	ids := map[string]bool{}
	for _, e := range ex {
		if !strings.HasPrefix(e.ID, ExamplePrefix) || !strings.HasPrefix(e.Fleet, ExamplePrefix) || e.ID != e.Fleet+"/"+e.Routine {
			t.Errorf("%+v: not an example id", e)
		}
		ids[e.ID] = true
	}
	for _, e := range ex {
		for _, l := range e.Chain {
			if !ids[l] {
				t.Errorf("%s: chain link %s is not in the catalogue", e.ID, l)
			}
		}
	}
	if !c.Has("example-triage/cycle") || c.Has("triage/cycle") {
		t.Error("example-triage/cycle is an example; triage/cycle is not")
	}
	if f := c.Fleets()["example-triage"]; f.Description == "" || f.Pack == "" {
		t.Errorf("the fleet's description and pack: %+v", f)
	}
	for _, e := range ex {
		if e.ID == "example-triage/cycle" {
			if len(e.Versions) != 2 || strings.Join(e.Versions[0].Stations, ",") != "decide" || len(e.Chain) != 4 || e.Chain[0] != "example-bugfix/intake" {
				t.Errorf("example-triage/cycle: %+v", e)
			}
		}
	}
}

// An installation's own triage/cycle and the catalogue's example-triage/cycle load side by side: two loads, two namespaces, no clash,
// and the one-acting-routine-per-kind rule is checked inside each.
func TestACatalogueAndAnInstallationsOwnFleetsDoNotMeet(t *testing.T) {
	own := t.TempDir()
	write(t, own, "triage/cycle.yaml", good)
	mine, err := Load(own)
	if err != nil {
		t.Fatal(err)
	}
	c := shippedCatalogue(t)
	if len(mine) != 1 || mine[0].ID() != "triage/cycle" || !c.Has("example-triage/cycle") {
		t.Fatalf("%v %v", mine, c.Examples()[:1])
	}
	for _, e := range c.Examples() {
		if e.ID == mine[0].ID() {
			t.Fatalf("the catalogue holds the installation's id %s", e.ID)
		}
	}
}

// What a catalogue cannot do is also pinned: it is not a list of routines, an example's display copy is not the loaded routine, and a nil
// catalogue is an empty one.
func TestACatalogueIsReadOnlyAndNilSafe(t *testing.T) {
	var none *Catalogue
	if none.Has("example-a/b") || none.Examples() != nil || none.Fleets() != nil || none.ForDisplay("example-a/b") != nil || none.Dir() != "" {
		t.Error("a nil catalogue is empty")
	}
	c := shippedCatalogue(t)
	d := c.ForDisplay("example-sites/site-check")
	if d == nil || d.ID() != "example-sites/site-check" {
		t.Fatalf("display copy: %+v", d)
	}
	d.Fleet = "changed"
	if !c.Has("example-sites/site-check") || c.ForDisplay("example-sites/site-check").Fleet != "example-sites" {
		t.Error("changing a display copy changed the catalogue")
	}
	f := c.Fleets()
	delete(f, "example-sites")
	if _, ok := c.Fleets()["example-sites"]; !ok {
		t.Error("the catalogue's fleets map was handed out")
	}
}

// A folder with a routine that does not load is not a catalogue: the image's own copy of fleets/ is broken.
func TestACatalogueWithABrokenRoutineIsRefused(t *testing.T) {
	d := t.TempDir()
	write(t, d, "triage/cycle.yaml", strings.Replace(good, "worker: triage", "worker: Bad Worker!", 1))
	if _, err := LoadExamples(d); err == nil || !strings.Contains(err.Error(), "examples "+d) {
		t.Fatalf("got %v", err)
	}
}

// An example may name a worker or a model alias an installation does not have: loading a catalogue checks the folder against itself only.
func TestAnExampleNamingAnUnknownWorkerLoads(t *testing.T) {
	d := t.TempDir()
	write(t, d, "triage/cycle.yaml", strings.Replace(good, "worker: triage", "worker: no-such-worker-anywhere", 1))
	c, err := LoadExamples(d)
	if err != nil || len(c.Examples()) != 1 || c.Examples()[0].Worker != "no-such-worker-anywhere" {
		t.Fatalf("%v %v", c, err)
	}
}

// Only these files may read the catalogue. The scheduler, the version reconciler, the shadows, dispatch, the budgets, the brief, the metrics and
// the model and worker checks are routine consumers: none of them is on the list, and a new reader of the catalogue has to be added to it on
// purpose (a Catalogue is not a []*Routine, so it cannot be passed to them by accident either).
func TestNoRunningCodeTakesTheCatalogue(t *testing.T) {
	allowed := map[string]string{
		"internal/spec/examples.go":     "the catalogue itself",
		"internal/api/api.go":           "the status lists the examples; Server.Examples",
		"internal/api/actions.go":       "routineByID refuses an example's id with its own message",
		"internal/ui/examples.go":       "the Examples page",
		"internal/ui/fleets.go":         "the fleets page counts them for its link",
		"cmd/flotillad/main.go":         "loads the catalogue and hands it to the API",
		"cmd/flotilla/main.go":          "the CLI lists the status's examples",
		"internal/api/examples_test.go": "tests",
	}
	re := regexp.MustCompile(`\bCatalogue\b|LoadExamples|ForDisplay|\.Examples\b|ExamplePrefix`)
	root := filepath.Join("..", "..")
	var hits []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", ".claude", ".check-tmp", "web", "site", "node_modules", "testdata", ".e2e":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if !re.Match(b) {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		if allowed[filepath.ToSlash(rel)] == "" {
			hits = append(hits, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) > 0 {
		t.Errorf("code outside the examples allow-list reads the catalogue: %v (an example is never a routine of this installation: add it to the list only if it only lists)", hits)
	}
}

// A folder version's manifest carries the bytes of every skill file its rules and stations name (skill-file:<name>), as a flat v1's did: a
// change to a fleet-level skill moves the version's hash and the freeze names the file.
func TestAFolderVersionsManifestCoversItsSkillFiles(t *testing.T) {
	v := strings.Replace(stationsGood, "    prompts: {task: task.md}\n    tools: all\n    tool_names: [probe]\n    gates: []\n",
		"    prompts: {task: task.md}\n    tools: all\n    tool_names: [probe]\n    gates: []\n    skills: [hint-a]\n", 1) +
		"skills:\n  rule:\n    - when: {class: x}\n      load: [hint-b]\n"
	d := stationsTree(t, v)
	skill := func(name, body string) {
		write(t, d, "triage/skills/"+name+"/SKILL.md", "---\nname: "+name+"\ndescription: d\n---\n"+body+"\n")
	}
	skill("hint-a", "one")
	skill("hint-b", "two")
	load := func() *Version {
		rs, err := Load(d)
		if err != nil {
			t.Fatal(err)
		}
		return rs[0].Versions[0]
	}
	before := load()
	for _, k := range []string{"skill-file:hint-a", "skill-file:hint-b"} {
		if len(before.Manifest[k]) != 64 {
			t.Fatalf("manifest lacks %s: %v", k, before.Manifest)
		}
	}
	skill("hint-a", "one, edited")
	after := load()
	if after.Hash == before.Hash || strings.Join(ChangedFiles(before.Manifest, after.Manifest), ",") != "skill-file:hint-a (changed)" {
		t.Fatalf("hash %v, changed %v", after.Hash != before.Hash, ChangedFiles(before.Manifest, after.Manifest))
	}
}
