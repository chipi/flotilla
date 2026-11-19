package spec

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"testing"
)

var updateGolden = flag.Bool("update-golden", false, "rewrite the golden file")

// TestShippedManifestsAreGolden pins, for every version of every shipped routine, its frozen hash and the manifest
// the hash is made of (file or field -> sha256), plus the routine's recorded hash. golden_hashes.txt pins the flat
// routines' hashes and golden_folder_hashes.txt the folder ones' first version; this adds every manifest entry, so
// a change that moves what a version's hash covers (the stations work adds files and keys to some versions) names the
// entry that moved. Regenerate on purpose with -update-golden.
func TestShippedManifestsAreGolden(t *testing.T) {
	type rec struct {
		RoutineHash string            `json:"routine_hash"`
		VersionHash string            `json:"version_hash"`
		Legacy      bool              `json:"legacy"`
		Manifest    map[string]string `json:"manifest"`
	}
	got := map[string]rec{}
	for _, d := range []string{filepath.Join("..", "..", "fleets"), filepath.Join("..", "..", "deploy", "e2e", "fleets")} {
		rs, err := Load(d)
		if err != nil {
			t.Fatal(err)
		}
		for _, j := range rs {
			for _, v := range j.Versions {
				got[j.ID()+"@v"+strconv.Itoa(v.Number)] = rec{v.RoutineHash, v.Hash, v.Legacy, v.Manifest}
			}
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
	path := filepath.Join("testdata", "golden_manifests.json")
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
	var want map[string]rec
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
		w, wok := want[k]
		g, gok := got[k]
		switch {
		case !wok:
			t.Errorf("%s: a shipped version not in golden_manifests.json", k)
		case !gok:
			t.Errorf("%s: in golden_manifests.json, not loaded", k)
		default:
			if g.RoutineHash != w.RoutineHash || g.VersionHash != w.VersionHash || g.Legacy != w.Legacy {
				t.Errorf("%s: hashes %s/%s legacy=%v, golden %s/%s legacy=%v", k, g.RoutineHash, g.VersionHash, g.Legacy, w.RoutineHash, w.VersionHash, w.Legacy)
			}
			for name, wv := range w.Manifest {
				if gv, ok := g.Manifest[name]; !ok {
					t.Errorf("%s: manifest entry %q gone", k, name)
				} else if gv != wv {
					t.Errorf("%s: manifest entry %q changed", k, name)
				}
			}
			for name := range g.Manifest {
				if _, ok := w.Manifest[name]; !ok {
					t.Errorf("%s: manifest entry %q is new", k, name)
				}
			}
		}
	}
}
