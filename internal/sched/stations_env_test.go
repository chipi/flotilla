package sched

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chipi/flotilla/internal/models"
	"github.com/chipi/flotilla/internal/spec"
)

const stationRoutine = `fleet: triage
routine: cycle
description: one pass
enabled: true
worker: triage
trigger: {every: 10m}
autonomy: shadow
budget: {day_usd: 2}
freshness: 35m
`

const stationVersion = `run: {command: "python3 x.py", workdir: signal-fleet/mvp, timeout: 20m}
stations:
  decide:
    harness: pi
    model: flash
    prompts: {task: task.md}
    tools: read
    limits: {max_turns: 5, timeout: 2m}
  plan:
    harness: flotilla
    model: pro
    prompts: {task: task.md}
    tools: all
    tool_names: [probe]
    gates: []
limits: {max_probes: 3}
`

func stationFleets(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	for p, body := range map[string]string{"triage/cycle/routine.yaml": stationRoutine, "triage/cycle/v1/spec.yaml": stationVersion, "triage/cycle/v1/task.md": "task {{x}}\n"} {
		fp := filepath.Join(d, p)
		if err := os.MkdirAll(filepath.Dir(fp), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fp, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return d
}

func stationModels() *models.Config {
	return &models.Config{Endpoints: map[string]models.Endpoint{"gw": {BaseURL: "http://gw/v1"}},
		Aliases: map[string]models.Alias{"flash": {Endpoint: "gw", Model: "f"}, "pro": {Endpoint: "gw", Model: "p"}, "other": {Endpoint: "gw", Model: "o"}}}
}

// A version with stations gets FLOTILLA_STATIONS, and FLOTILLA_MODELS holds exactly the aliases its stations name. Pinned byte for byte.
func TestRunContractOfAStationVersion(t *testing.T) {
	rs, err := spec.Load(stationFleets(t))
	if err != nil {
		t.Fatal(err)
	}
	env, err := RunContract(rs[0], stationModels(), EnvParams{RunID: "r1", RoutineKey: "triage/cycle", Stage: "shadow"})
	if err != nil {
		t.Fatal(err)
	}
	sha := rs[0].StationText["decide"]["task"].SHA256
	want := `{"schema_version":1,"version_limits":{"max_probes":3},"stations":{` +
		`"decide":{"harness":"pi","model":"flash","tools":"read","limits":{"max_turns":5,"timeout":"2m","max_output_tokens":0},"texts":{"task":{"path":"cycle/v1/task.md","sha256":"` + sha + `"}},"tool_names":[],"gates":[],"hooks":null,"skills":[]},` +
		`"plan":{"harness":"flotilla","model":"pro","tools":"all","limits":{"max_turns":0,"timeout":"","max_output_tokens":0},"texts":{"task":{"path":"cycle/v1/task.md","sha256":"` + sha + `"}},"tool_names":["probe"],"gates":[],"hooks":null,"skills":[]}}}`
	if env["FLOTILLA_STATIONS"] != want {
		t.Fatalf("FLOTILLA_STATIONS\n got %s\nwant %s", env["FLOTILLA_STATIONS"], want)
	}
	if m := env["FLOTILLA_MODELS"]; !strings.Contains(m, `"flash"`) || !strings.Contains(m, `"pro"`) || strings.Contains(m, `"other"`) {
		t.Fatalf("the models subset is the stations' aliases only: %s", m)
	}
	if _, ok := env["FLOTILLA_PROMPTS"]; ok {
		t.Fatal("a station version has no FLOTILLA_PROMPTS: its texts are in FLOTILLA_STATIONS")
	}
}

// The scheduler's runEnv, for a live run and a shadow run, is RunContract: the same bytes offline and live.
func TestRunEnvIsRunContractForLiveAndShadow(t *testing.T) {
	rs, err := spec.Load(stationFleets(t))
	if err != nil {
		t.Fatal(err)
	}
	s := &Scheduler{Models: stationModels()}
	p := EnvParams{RunID: "r1", RoutineKey: "triage/cycle", Stage: "shadow", BudgetLeft: 1.5}
	want, _ := RunContract(rs[0], s.Models, p)
	live := s.runEnv(rs[0], "r1", "triage/cycle", "shadow", 1.5, "")
	shadow := s.runEnv(rs[0], "r1", "triage/cycle", "shadow", 1.5, "")
	for k, v := range want {
		if live[k] != v || shadow[k] != v {
			t.Errorf("%s: live %q shadow %q contract %q", k, live[k], shadow[k], v)
		}
	}
	if live["FLOTILLA_STATIONS"] == "" || len(live) != len(want) {
		t.Fatalf("live %v", live)
	}
}

func TestOnlyAVersionWithStationsGetsFlotillaStations(t *testing.T) {
	j := &spec.Routine{Fleet: "f", Routine: "r", Version: 1}
	env, _ := RunContract(j, nil, EnvParams{})
	if _, ok := env["FLOTILLA_STATIONS"]; ok {
		t.Fatal("a routine with no stations gets none")
	}
}
