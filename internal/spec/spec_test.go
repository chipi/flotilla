package spec

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const good = `fleet: triage
job: cycle
description: one pass
enabled: true
worker: triage
trigger: {every: 10m}
run: {command: "python3 x.py", workdir: signal-fleet/mvp, timeout: 20m}
stage: shadow
budget: {day_usd: 2}
freshness: 35m
`

func write(t *testing.T, dir, rel, body string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadGood(t *testing.T) {
	d := t.TempDir()
	write(t, d, "triage/cycle.yaml", good)
	jobs, err := Load(d)
	if err != nil {
		t.Fatal(err)
	}
	j := jobs[0]
	if j.ID() != "triage/cycle" || j.Every.String() != "10m0s" || j.Limit.String() != "20m0s" || len(j.Hash) != 12 {
		t.Fatalf("got %+v", j)
	}
}

func TestRejects(t *testing.T) {
	cases := map[string]struct{ rel, body, want string }{
		"unknown field":         {"triage/cycle.yaml", good + "retries: 3\n", "field retries not found"},
		"fleet not its folder":  {"other/cycle.yaml", good, `fleet "triage", but the file is under other/`},
		"job not its file name": {"triage/pass.yaml", good, `job "cycle", but the file is pass.yaml`},
		"bad stage":             {"triage/cycle.yaml", strings.Replace(good, "stage: shadow", "stage: yolo", 1), "stage \"yolo\""},
		"bad interval":          {"triage/cycle.yaml", strings.Replace(good, "every: 10m", "every: soon", 1), "trigger.every"},
		"interval too short":    {"triage/cycle.yaml", strings.Replace(good, "every: 10m", "every: 5s", 1), "at least 1m0s"},
		"timeout too long":      {"triage/cycle.yaml", strings.Replace(good, "timeout: 20m", "timeout: 2h", 1), "more than three intervals"},
		"always stale":          {"triage/cycle.yaml", strings.Replace(good, "freshness: 35m", "freshness: 5m", 1), "always look stale"},
		"negative budget":       {"triage/cycle.yaml", strings.Replace(good, "day_usd: 2", "day_usd: -1", 1), "must not be negative"},
		"no command":            {"triage/cycle.yaml", strings.Replace(good, `command: "python3 x.py", `, "", 1), "run.command is required"},
		"no description":        {"triage/cycle.yaml", strings.Replace(good, "description: one pass\n", "", 1), "description is required"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			d := t.TempDir()
			write(t, d, c.rel, c.body)
			_, err := Load(d)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want error containing %q, got %v", c.want, err)
			}
		})
	}
}

func TestOneBadSpecFailsTheLoad(t *testing.T) {
	d := t.TempDir()
	write(t, d, "triage/cycle.yaml", good)
	write(t, d, "triage/broken.yaml", "fleet: triage\njob: broken\n")
	if _, err := Load(d); err == nil {
		t.Fatal("a bad spec must fail the whole load, not drop that job")
	}
}

func TestEmptyDir(t *testing.T) {
	if _, err := Load(t.TempDir()); err == nil || !strings.Contains(err.Error(), "no job specs") {
		t.Fatalf("got %v", err)
	}
}

// The specs shipped in this repo must load: a broken one is a broken deploy.
func TestRepoSpecs(t *testing.T) {
	jobs, err := Load(filepath.Join("..", "..", "fleets"))
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]*Job{}
	for _, j := range jobs {
		ids[j.ID()] = j
	}
	tri, ok := ids["triage/cycle"]
	if !ok {
		t.Fatal("triage/cycle missing")
	}
	if tri.Stage != StageShadow {
		t.Fatalf("triage/cycle must default to shadow (the switch raises it with a control), got %s", tri.Stage)
	}
	if _, ok := ids["platform/heartbeat"]; !ok {
		t.Fatal("platform/heartbeat missing")
	}
}
