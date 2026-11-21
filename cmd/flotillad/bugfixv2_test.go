package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chipi/flotilla/internal/spec"
)

// bugfix/fix2.py reads these; the example is the version a run of it is given (fleets/bugfix/fix/v2, ADR-0020's step 6 amendment).
const bugfixModels = `{"endpoints": {"gw": {"base_url": "http://gw/v1"}}, "aliases": {"fix-flash": {"endpoint": "gw", "model": "f", "tools": "native"}, "fix-reviewer": {"endpoint": "gw", "model": "r", "tools": "native"}}}`

func fileSHA(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func TestTheBugfixFixV2ExampleLoadsAndPrintsTheStationsItsChainReads(t *testing.T) {
	fleets := filepath.Join("..", "..", "fleets")
	if _, err := spec.Load(fleets); err != nil {
		t.Fatalf("-check over the examples: %v", err)
	}
	d := t.TempDir()
	writeTree(t, d, map[string]string{"models.json": bugfixModels})
	var out bytes.Buffer
	if err := printStations(&out, filepath.Join(fleets, "bugfix", "fix", "v2"), filepath.Join(d, "models.json")); err != nil {
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
	if got.Version != 2 || !strings.Contains(got.Command, "bugfix/fix2.py") || strings.Contains(got.Command, "model") {
		t.Fatalf("the command names fix2 and no model: %+v", got)
	}
	var doc struct {
		VersionLimits map[string]float64 `json:"version_limits"`
		Stations      map[string]struct {
			Harness string
			Model   string
			Tools   string
			Limits  struct {
				MaxTurns        int    `json:"max_turns"`
				Timeout         string `json:"timeout"`
				MaxOutputTokens int    `json:"max_output_tokens"`
			}
			Texts map[string]struct{ Path, SHA256 string }
		}
	}
	if err := json.Unmarshal([]byte(got.Env["FLOTILLA_STATIONS"]), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Stations) != 5 || doc.VersionLimits["kickback_max"] != 2 || doc.VersionLimits["review_kickback_max"] != 1 {
		t.Fatalf("%+v", doc)
	}
	want := map[string][3]string{"fix": {"fix-flash", "all", "20m"}, "repro": {"fix-flash", "all", "20m"}, "review": {"fix-reviewer", "none", "10m"},
		"verdict": {"fix-flash", "read", "15m"}, "advisor": {"fix-reviewer", "read", "10m"}}
	for name, w := range want {
		s := doc.Stations[name]
		if s.Harness != "pi" || [3]string{s.Model, s.Tools, s.Limits.Timeout} != w || s.Limits.MaxOutputTokens != 32768 {
			t.Fatalf("station %s: %+v", name, s)
		}
		if s.Texts["rules"].Path != "fix/v2/rules.md" || s.Texts["rules"].SHA256 != fileSHA(t, filepath.Join("..", "..", "bugfix", "fleet_rules.md")) {
			t.Fatalf("station %s's rules are bugfix/fleet_rules.md, hashed: %+v", name, s.Texts)
		}
	}
	if doc.Stations["fix"].Limits.MaxTurns != 90 || doc.Stations["repro"].Limits.MaxTurns != 40 {
		t.Fatalf("the turn caps v1's flags gave: %+v", doc.Stations)
	}
	if a := doc.Stations["verdict"].Texts["agent"]; a.Path != "fix/v2/triage.md" || a.SHA256 != fileSHA(t, filepath.Join("..", "..", "bugfix-fleet", "agents", "triage.md")) {
		t.Fatalf("the verdict's text is agents/triage.md: %+v", a)
	}
	if a := doc.Stations["repro"].Texts["agent"]; a.Path != "fix/v2/repro.md" || a.SHA256 != fileSHA(t, filepath.Join("..", "..", "bugfix-fleet", "agents", "repro.md")) {
		t.Fatalf("the reproducer's text is agents/repro.md: %+v", a)
	}
}

func TestAStationOfTheBugfixV2ExampleNamingAToolsValueItsHarnessDoesNotServeIsRefused(t *testing.T) {
	src := filepath.Join("..", "..", "fleets", "bugfix", "fix")
	for _, c := range []struct{ name, from, to, why string }{
		{"a tools value no harness serves", "    tools: none\n", "    tools: write\n", "stations.review.tools: \"write\" is all, read or none"},
		{"a harness that does not serve it", "    harness: pi\n    model: fix-reviewer\n    prompts: {rules: rules.md}\n    tools: none\n",
			"    harness: flotilla\n    model: fix-reviewer\n    prompts: {rules: rules.md}\n    tools: read\n", "harness flotilla does not serve tools: read"},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := t.TempDir()
			files := map[string]string{"models.json": bugfixModels}
			for _, f := range []string{"routine.yaml", "v2/spec.yaml", "v2/rules.md", "v2/repro.md", "v2/triage.md"} {
				b, err := os.ReadFile(filepath.Join(src, f))
				if err != nil {
					t.Fatal(err)
				}
				files[filepath.Join("fleets", "bugfix", "fix", f)] = string(b)
			}
			spec2 := files[filepath.Join("fleets", "bugfix", "fix", "v2/spec.yaml")]
			if !strings.Contains(spec2, c.from) {
				t.Fatalf("the example has no %q to change", c.from)
			}
			files[filepath.Join("fleets", "bugfix", "fix", "v2/spec.yaml")] = strings.Replace(spec2, c.from, c.to, 1)
			writeTree(t, d, files)
			err := printStations(&bytes.Buffer{}, filepath.Join(d, "fleets", "bugfix", "fix", "v2"), filepath.Join(d, "models.json"))
			if err == nil || !strings.Contains(err.Error(), c.why) {
				t.Fatalf("got %v, want it to say %q", err, c.why)
			}
		})
	}
}
