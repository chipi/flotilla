package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chipi/flotilla/internal/sched"
	"github.com/chipi/flotilla/internal/spec"
)

func writeTree(t *testing.T, d string, files map[string]string) {
	t.Helper()
	for p, body := range files {
		fp := filepath.Join(d, p)
		if err := os.MkdirAll(filepath.Dir(fp), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fp, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

const (
	cmdRoutine = "fleet: triage\nroutine: cycle\ndescription: d\nenabled: true\nworker: triage\ntrigger: {every: 10m}\nautonomy: shadow\nbudget: {day_usd: 2}\nfreshness: 35m\n"
	cmdVersion = "run: {command: \"python3 x.py\", workdir: w, timeout: 20m}\nstations:\n  decide: {harness: pi, model: flash, prompts: {task: task.md}, tools: read}\n"
	cmdModels  = `{"endpoints": {"gw": {"base_url": "http://gw/v1"}}, "aliases": {"flash": {"endpoint": "gw", "model": "f", "tools": "native"}, "x": {"endpoint": "gw", "model": "x"}}}`
)

func TestStationsFlagPrintsTheContractFlotilladGivesARun(t *testing.T) {
	d := t.TempDir()
	writeTree(t, d, map[string]string{"fleets/triage/cycle/routine.yaml": cmdRoutine, "fleets/triage/cycle/v1/spec.yaml": cmdVersion,
		"fleets/triage/cycle/v1/task.md": "task\n", "models.json": cmdModels})
	var out bytes.Buffer
	if err := printStations(&out, filepath.Join(d, "fleets/triage/cycle/v1/"), filepath.Join(d, "models.json")); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Command string
		Version int
		Env     map[string]string
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	rs, err := spec.Load(filepath.Join(d, "fleets"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Command != "python3 x.py" || got.Version != 1 {
		t.Fatalf("%+v", got)
	}
	want, _ := sched.RunContract(rs[0], nil, sched.EnvParams{})
	if got.Env["FLOTILLA_STATIONS"] != want["FLOTILLA_STATIONS"] || got.Env["FLOTILLA_STATIONS"] == "" {
		t.Fatalf("-stations is not the live contract:\n%s\n%s", got.Env["FLOTILLA_STATIONS"], want["FLOTILLA_STATIONS"])
	}
	if got.Env["FLOTILLA_SHADOW"] != "1" || strings.Contains(got.Env["FLOTILLA_MODELS"], `"x"`) || !strings.Contains(got.Env["FLOTILLA_MODELS"], `"flash"`) {
		t.Fatalf("env %v", got.Env)
	}
	for k := range got.Env {
		if strings.Contains(k, "API_URL") || strings.Contains(k, "TOKEN") {
			t.Fatalf("an offline contract has no flotillad address or token: %s", k)
		}
	}
}

func TestStationsFlagRefusesWhatLoadRefuses(t *testing.T) {
	d := t.TempDir()
	bad := strings.Replace(cmdVersion, "model: flash", "model: x", 1) // alias x has no tools capability: tools: read is refused
	writeTree(t, d, map[string]string{"fleets/triage/cycle/routine.yaml": cmdRoutine, "fleets/triage/cycle/v1/spec.yaml": bad,
		"fleets/triage/cycle/v1/task.md": "task\n", "models.json": cmdModels})
	err := printStations(&bytes.Buffer{}, filepath.Join(d, "fleets/triage/cycle/v1"), filepath.Join(d, "models.json"))
	if err == nil || !strings.Contains(err.Error(), "tools capability is none") {
		t.Fatalf("got %v", err)
	}
	write2 := map[string]string{"fleets/triage/cycle/v1/task.md": "  \n"}
	writeTree(t, d, write2)
	writeTree(t, d, map[string]string{"fleets/triage/cycle/v1/spec.yaml": cmdVersion})
	err = printStations(&bytes.Buffer{}, filepath.Join(d, "fleets/triage/cycle/v1"), filepath.Join(d, "models.json"))
	if err == nil || !strings.Contains(err.Error(), "is empty") {
		t.Fatalf("an empty text is refused as at load: %v", err)
	}
}
