package spec

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestPackWithoutFlowsIsGolden pins the pack hash of every shipped fleet recorded before flows existed
// (RFC-0016): a fleet that declares no flows keeps its pack byte for byte. Regenerate on purpose with
// -update-golden.
func TestPackWithoutFlowsIsGolden(t *testing.T) {
	var lines []string
	for _, d := range []string{filepath.Join("..", "..", "fleets"), filepath.Join("..", "..", "deploy", "e2e", "fleets")} {
		rs, err := LoadVersions(d)
		if err != nil {
			t.Fatal(err)
		}
		fs, err := LoadFleets(d, rs)
		if err != nil {
			t.Fatal(err)
		}
		for name, f := range fs {
			if _, err := os.Stat(filepath.Join(d, name, "flows")); err == nil {
				continue // a fleet with flows has a pack of its own (flow_test.go)
			}
			lines = append(lines, name+" "+f.Pack)
		}
	}
	sort.Strings(lines)
	got := strings.Join(lines, "\n") + "\n"
	path := filepath.Join("testdata", "golden_packs.txt")
	if *updateGolden {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(want) != got {
		t.Fatalf("a fleet's pack hash moved (a fleet without flows must keep its pack):\nwant:\n%s\ngot:\n%s", want, got)
	}
}
