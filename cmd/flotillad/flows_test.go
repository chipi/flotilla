package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chipi/flotilla/internal/registry"
)

func copyDir(t *testing.T, from, to string) {
	t.Helper()
	err := filepath.Walk(from, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(from, p)
		if info.IsDir() {
			return os.MkdirAll(filepath.Join(to, rel), 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(to, rel), b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// -check refuses folders that differ from their flows' expansion, naming the file, and passes them once they match.
func TestFlowLinesRefuseDriftAndTheRegistryGaps(t *testing.T) {
	reg, err := registry.Load(filepath.Join("..", "..", "host", "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	specs := t.TempDir()
	copyDir(t, filepath.Join("..", "..", "fleets", "jobs"), filepath.Join(specs, "jobs"))
	cfg := &Config{SpecsDir: specs, BlocksDir: filepath.Join("..", "..", "blocks")}
	lines, err := flowLines(cfg, reg)
	if err != nil || len(lines) != 1 || !strings.Contains(lines[0], "committed folders equal the generator's output") {
		t.Fatalf("%v %v", lines, err)
	}
	p := filepath.Join(specs, "jobs", "backup-monitor", "routine.yaml")
	raw, _ := os.ReadFile(p)
	if err := os.WriteFile(p, []byte(strings.Replace(string(raw), "freshness: 25m", "freshness: 40m", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := flowLines(cfg, reg); err == nil || !strings.Contains(err.Error(), "backup-monitor/routine.yaml: differs") {
		t.Fatalf("drift is refused with the file named: %v", err)
	}
	if err := os.WriteFile(p, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	fp := filepath.Join(specs, "jobs", "flows", "backup", "flow.yaml")
	flow, _ := os.ReadFile(fp)
	if err := os.WriteFile(fp, []byte(strings.Replace(string(flow), "action: backup.rerun,", "action: backup.missing,", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := flowLines(cfg, reg); err == nil || !strings.Contains(err.Error(), "the registry has no action backup.missing") {
		t.Fatalf("an action the registry lacks is refused: %v", err)
	}
}

func TestFlowLinesNeedNoBlocksWhenNoFleetHasFlows(t *testing.T) {
	reg, err := registry.Load(filepath.Join("..", "..", "host", "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	specs := t.TempDir()
	copyDir(t, filepath.Join("..", "..", "fleets", "review"), filepath.Join(specs, "review"))
	lines, err := flowLines(&Config{SpecsDir: specs, BlocksDir: filepath.Join(specs, "no-such-library")}, reg)
	if err != nil || len(lines) != 0 {
		t.Fatalf("a fleet without flows reads no library: %v %v", lines, err)
	}
}
