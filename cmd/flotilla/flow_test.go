package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func copyTree(t *testing.T, from, to string) {
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

func TestFlowExpandCheckAndWrite(t *testing.T) {
	blocks := filepath.Join("..", "..", "blocks")
	reg := filepath.Join("..", "..", "host", "registry.json")
	jobs := filepath.Join("..", "..", "fleets", "jobs")
	var out bytes.Buffer
	if err := flowCmd(&out, []string{"expand", "-check", "-blocks", blocks, "-registry", reg, jobs}); err != nil || !strings.Contains(out.String(), "committed folders equal") {
		t.Fatalf("the example fleet's folders equal its flow: %v %q", err, out.String())
	}

	// A copy with a folder hand-edited: -check names the file and the line, a plain expand repairs it, and writes only that.
	dir := filepath.Join(t.TempDir(), "jobs")
	copyTree(t, jobs, dir)
	p := filepath.Join(dir, "rerun-backup", "v1", "spec.yaml")
	raw, _ := os.ReadFile(p)
	if err := os.WriteFile(p, []byte(strings.Replace(string(raw), "timeout: 1m", "timeout: 9m", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	err := flowCmd(&out, []string{"expand", "-check", "-blocks", blocks, "-registry", reg, dir})
	if err == nil || !strings.Contains(err.Error(), `rerun-backup/v1/spec.yaml: differs at line 4: committed "  timeout: 9m", generated "  timeout: 1m"`) {
		t.Fatalf("drift is named: %v", err)
	}
	if got, _ := os.ReadFile(p); !strings.Contains(string(got), "9m") {
		t.Fatal("-check writes nothing")
	}
	out.Reset()
	if err := flowCmd(&out, []string{"expand", "-blocks", blocks, "-registry", reg, dir}); err != nil || !strings.Contains(out.String(), "(1 changed)") {
		t.Fatalf("%v %q", err, out.String())
	}
	if got, _ := os.ReadFile(p); string(got) != string(raw) {
		t.Fatal("the expansion is back to what the generator writes")
	}
}

func TestFlowExpandRefusals(t *testing.T) {
	var out bytes.Buffer
	if err := flowCmd(&out, nil); err == nil {
		t.Fatal("usage")
	}
	if err := flowCmd(&out, []string{"expand"}); err == nil {
		t.Fatal("a fleet dir is required")
	}
	if err := flowCmd(&out, []string{"expand", "-blocks", "blocks", t.TempDir()}); err == nil || !strings.Contains(err.Error(), "no flows folder") {
		t.Fatalf("a fleet without flows: %v", err)
	}
}
