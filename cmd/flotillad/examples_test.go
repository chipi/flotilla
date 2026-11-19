package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chipi/flotilla/internal/spec"
	"github.com/chipi/flotilla/internal/store"
	"github.com/chipi/flotilla/internal/versions"
)

func TestExamplesDirConfig(t *testing.T) {
	if c, err := load(t, ""); err != nil || c.ExamplesDir != "" {
		t.Fatalf("unset: %+v %v", c, err)
	}
	if c, err := load(t, "examples_dir: /opt/flotilla/examples\n"); err != nil || c.ExamplesDir != "/opt/flotilla/examples" {
		t.Fatalf("set: %+v %v", c, err)
	}
	if _, err := load(t, "examples_dir: /s/\n"); err == nil || !strings.Contains(err.Error(), "examples_dir is the specs_dir") {
		t.Fatalf("the catalogue is not the fleets it sits beside: %v", err)
	}
	if _, err := load(t, "example_dir: /x\n"); err == nil {
		t.Fatal("a misspelt key is refused (KnownFields): examples_dir is a deliberate change")
	}
}

// An installation with its own triage/cycle and an examples catalogue that holds the image's: both load, the routines are the installation's
// alone, the catalogue holds example-triage/cycle, the example needs no worker or alias of the installation's, and what the start writes to
// the store (every routine's first version) has no row for an example.
func TestACatalogueBesideAnInstallationsOwnFleets(t *testing.T) {
	own := t.TempDir()
	writeTree(t, own, map[string]string{"triage/cycle.yaml": "fleet: triage\nroutine: cycle\ndescription: mine\nenabled: true\nworker: only-worker\n" +
		"trigger: {every: 10m}\nrun: {command: \"true\", workdir: ., timeout: 1m}\nautonomy: record\nbudget: {day_usd: 1}\nfreshness: 35m\n"})
	cfg := &Config{SpecsDir: own, ExamplesDir: filepath.Join("..", "..", "fleets")}
	routines, err := spec.LoadVersions(cfg.SpecsDir)
	if err != nil {
		t.Fatal(err)
	}
	cat, err := loadExamples(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(routines) != 1 || routines[0].ID() != "triage/cycle" || routines[0].Worker != "only-worker" || !cat.Has("example-triage/cycle") {
		t.Fatalf("%d routines, catalogue %v", len(routines), cat.Has("example-triage/cycle"))
	}
	for _, e := range cat.Examples() {
		if e.Worker == "only-worker" {
			t.Fatalf("the example %s is the installation's", e.ID)
		}
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, _, err := versions.Reconcile(context.Background(), st, routines); err != nil {
		t.Fatal(err)
	}
	rows, err := st.AllVersions(context.Background())
	if err != nil || len(rows) != 1 || rows[0].Routine != "triage/cycle" {
		t.Fatalf("the store after a start: %+v %v", rows, err)
	}
	if line := examplesLine(cfg, cat); !strings.Contains(line, "never run") || !strings.Contains(line, "fleets from") {
		t.Errorf("-check's line: %s", line)
	}
	if line := examplesLine(&Config{}, nil); !strings.Contains(line, "examples_dir is not set") {
		t.Errorf("-check's line without a catalogue: %s", line)
	}
	if none, err := loadExamples(&Config{}); none != nil || err != nil {
		t.Errorf("unset: %v %v", none, err)
	}
	if _, err := loadExamples(&Config{ExamplesDir: t.TempDir()}); err == nil {
		t.Error("an empty examples folder is not a catalogue")
	}
}

// The image carries the examples twice from the same source, and the second copy is the one an installation's mounted fleets do not replace:
// two COPY lines of one folder, so the catalogue cannot drift from the examples the image runs by default.
func TestTheImageCarriesTheExamplesOutOfTheFleetsMountsReach(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "deploy", "docker", "flotillad.Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	var fleets, examples bool
	for _, l := range strings.Split(string(raw), "\n") {
		f := strings.Fields(l)
		if len(f) == 3 && f[0] == "COPY" && f[1] == "fleets" {
			fleets = fleets || f[2] == "/opt/flotilla/fleets"
			examples = examples || f[2] == "/opt/flotilla/examples"
		}
	}
	if !fleets || !examples {
		t.Errorf("flotillad.Dockerfile must COPY fleets to /opt/flotilla/fleets (%v) and to /opt/flotilla/examples (%v)", fleets, examples)
	}
	for _, f := range []string{"worker-triage", "worker-ops", "worker-bugfix", "worker-marketing"} {
		w, err := os.ReadFile(filepath.Join("..", "..", "deploy", "docker", f+".Dockerfile"))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(w), "/opt/flotilla/examples") {
			t.Errorf("%s carries the examples: an example never runs, so no worker image needs them", f)
		}
	}
}
