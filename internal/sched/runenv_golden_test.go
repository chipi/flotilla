package sched

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"testing"

	"github.com/chipi/flotilla/internal/models"
	"github.com/chipi/flotilla/internal/spec"
)

var updateGolden = flag.Bool("update-golden", false, "rewrite the golden file")

// TestRunEnvOfShippedSpecsIsGolden pins the whole environment runEnv hands every shipped routine version (the
// example fleets and the e2e fleets), captured before the stations work. A change to runEnv that adds a key to a
// routine that declares no stations, or moves one, fails here: nothing live changes until a routine opts in
// (v2-design.md section 7, steps 3 and 4). Regenerate on purpose with -update-golden and read the diff.
func TestRunEnvOfShippedSpecsIsGolden(t *testing.T) {
	got := map[string]map[string]string{}
	var all []*spec.Routine
	for _, d := range []string{filepath.Join("..", "..", "fleets"), filepath.Join("..", "..", "deploy", "e2e", "fleets")} {
		rs, err := spec.Load(d)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, rs...)
	}
	cfg := &models.Config{Endpoints: map[string]models.Endpoint{"gw": {BaseURL: "http://gw/v1"}}, Aliases: map[string]models.Alias{}}
	for _, j := range all {
		for _, v := range j.Versions {
			vj, err := j.WithVersion(v.Number)
			if err != nil {
				continue // a retired or broken version has no run
			}
			for _, m := range vj.Models {
				cfg.Aliases[m] = models.Alias{Endpoint: "gw", Model: "m-" + m}
			}
		}
	}
	s := &Scheduler{Models: cfg, OTLPTracesURL: "http://otlp.example/v1/traces", Environment: "test"}
	for _, j := range all {
		for _, v := range j.Versions {
			vj, err := j.WithVersion(v.Number)
			if err != nil {
				continue
			}
			key := j.ID() + "@v" + strconv.Itoa(v.Number)
			if _, dup := got[key]; dup {
				t.Fatalf("%s loaded twice", key)
			}
			got[key] = s.runEnv(vj, "run-1", j.ID(), "shadow", 1.5, "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01")
		}
	}
	if len(got) == 0 {
		t.Fatal("no shipped routine loaded")
	}
	raw, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw, '\n')
	path := filepath.Join("testdata", "runenv_shipped.json")
	if *updateGolden {
		if err := os.WriteFile(path, raw, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	wantRaw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var want map[string]map[string]string
	if err := json.Unmarshal(wantRaw, &want); err != nil {
		t.Fatal(err)
	}
	var keys []string
	for k := range want {
		keys = append(keys, k)
	}
	for k := range got {
		if _, ok := want[k]; !ok {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		w, g := want[k], got[k]
		for name, wv := range w {
			if gv, ok := g[name]; !ok {
				t.Errorf("%s: %s missing, golden %q", k, name, wv)
			} else if gv != wv {
				t.Errorf("%s: %s is %q, golden %q", k, name, gv, wv)
			}
		}
		for name, gv := range g {
			if _, ok := w[name]; !ok {
				t.Errorf("%s: %s=%q is new (not in the golden)", k, name, gv)
			}
		}
	}
}
