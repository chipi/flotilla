package sched

import (
	"encoding/json"
	"path"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/chipi/flotilla/internal/models"
	"github.com/chipi/flotilla/internal/spec"
)

// A routine that moved from a flat spec to a folder (step 5) is handed the same run environment: for every version the
// flat spec had, RunContract returns the same map, live and shadow, once the one thing a move changes on purpose is read
// the same on both sides: a prompt's path names the file's new place (its role and its hash stay).
func TestMovedRoutinesGetTheSameRunEnvironment(t *testing.T) {
	old, err := spec.Load(filepath.Join("..", "spec", "testdata", "flat-before-step5"))
	if err != nil {
		t.Fatal(err)
	}
	now, err := spec.Load(filepath.Join("..", "..", "fleets"))
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]*spec.Routine{}
	for _, j := range now {
		byID[j.ID()] = j
	}
	cfg := &models.Config{Endpoints: map[string]models.Endpoint{"gw": {BaseURL: "http://gw/v1"}}, Aliases: map[string]models.Alias{}}
	for _, j := range old {
		for _, m := range j.Models {
			cfg.Aliases[m] = models.Alias{Endpoint: "gw", Model: "m-" + m}
		}
	}
	compared, moved, stationVersions := 0, 0, 0
	for _, o := range old {
		n := byID[o.ID()]
		if n == nil {
			continue
		}
		if o.Versions[0].Legacy && !n.Versions[0].Legacy {
			moved++
		}
		for _, ov := range o.Versions {
			oj, err := o.WithVersion(ov.Number)
			if err != nil {
				t.Fatal(err)
			}
			nj, err := n.WithVersion(ov.Number)
			if err != nil {
				t.Fatalf("%s v%d: %v", o.ID(), ov.Number, err)
			}
			for _, stage := range []string{"shadow", "live"} {
				p := EnvParams{RunID: "run-1", RoutineKey: o.ID(), Stage: stage, BudgetLeft: 1.5, Traceparent: "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01",
					OTLPTracesURL: "http://otlp.example/v1/traces", Environment: "test"}
				want, werr := RunContract(oj, cfg, p)
				got, gerr := RunContract(nj, cfg, p)
				if (werr == nil) != (gerr == nil) {
					t.Errorf("%s v%d: models error %v, was %v", o.ID(), ov.Number, gerr, werr)
				}
				if len(nj.Stations) > 0 && len(oj.Stations) == 0 {
					// a version that became a station version (triage/cycle v1): its model step is declared in FLOTILLA_STATIONS instead of
					// FLOTILLA_HARNESS and FLOTILLA_PROMPTS, and its model alias is the station's. Every other variable is what it was.
					if got["FLOTILLA_STATIONS"] == "" || got["FLOTILLA_HARNESS"] != "" || got["FLOTILLA_PROMPTS"] != "" {
						t.Errorf("%s v%d: a station version has FLOTILLA_STATIONS and neither FLOTILLA_HARNESS nor FLOTILLA_PROMPTS", o.ID(), ov.Number)
					}
					for _, k := range []string{"FLOTILLA_STATIONS", "FLOTILLA_HARNESS", "FLOTILLA_PROMPTS", "FLOTILLA_MODELS"} {
						delete(got, k)
						delete(want, k)
					}
					stationVersions++
				}
				if !reflect.DeepEqual(promptsRead(t, got), promptsRead(t, want)) {
					t.Errorf("%s v%d: FLOTILLA_PROMPTS %s, was %s", o.ID(), ov.Number, got["FLOTILLA_PROMPTS"], want["FLOTILLA_PROMPTS"])
				}
				delete(got, "FLOTILLA_PROMPTS")
				delete(want, "FLOTILLA_PROMPTS")
				if !reflect.DeepEqual(got, want) {
					t.Errorf("%s v%d (%s): run environment\n now  %v\n flat %v", o.ID(), ov.Number, stage, got, want)
				}
				compared++
			}
		}
	}
	if stationVersions != 2 { // triage/cycle v1, live and shadow
		t.Errorf("%d station environments compared, want triage/cycle v1's two", stationVersions)
	}
	if moved == 0 || compared == 0 {
		t.Fatalf("compared %d environments of %d moved routines: nothing proven", compared, moved)
	}
	t.Logf("%d environments compared, %d routines moved", compared, moved)
}

// promptsRead is FLOTILLA_PROMPTS as role -> (file name, hash), the part of it a move leaves alone.
func promptsRead(t *testing.T, env map[string]string) map[string]string {
	t.Helper()
	raw := env["FLOTILLA_PROMPTS"]
	if raw == "" {
		return nil
	}
	var m map[string]struct{ Path, SHA256 string }
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for role, e := range m {
		out[role] = path.Base(e.Path) + " " + e.SHA256
	}
	return out
}
