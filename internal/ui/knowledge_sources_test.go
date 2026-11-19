package ui

import (
	"github.com/chipi/flotilla/internal/spec"
	"path/filepath"
	"strings"
	"testing"
)

// Sources lists what fleets keep in git for agents: the fleet-level playbooks and skills, and the texts of each version's own folder (the bugfix
// rules, triage's system text), which moved there with the folder layout.
func TestKnowledgeSourcesListVersionFolderTexts(t *testing.T) {
	f := setup(t)
	f.ui.SpecsDir = filepath.Join("..", "..", "fleets")
	ck, _ := f.csrfOf(f.addOperator())
	resp, page := f.do("GET", "/knowledge?tab=sources", ck, nil, nil)
	if resp.StatusCode != 200 {
		t.Fatal(resp.StatusCode)
	}
	for _, want := range []string{"fleets/bugfix/fix/v1/rules.md", "fleets/triage/cycle/v1/system.md", "fleets/triage/cycle/v2/system.md",
		"fleets/triage/playbooks/triage.md", "fleets/triage/skills/hint-external", "fleets/platform/heartbeat/v2/probe.md"} {
		if !strings.Contains(page, want) {
			t.Errorf("Sources lacks %s", want)
		}
	}
}

// A station version's page explains its model step, gates and texts from its stations, as a flat spec's harness block did.
func TestRoutinePageBlocksOfAStationVersion(t *testing.T) {
	rs, err := spec.Load(filepath.Join("..", "..", "fleets"))
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range rs {
		if j.ID() != "triage/cycle" {
			continue
		}
		have := map[string]rBlock{}
		for _, l := range specBlocks(j, mixCounts{}, func(string) string { return "" }) {
			for _, b := range l.Blocks {
				have[b.ID] = b
			}
		}
		for _, id := range []string{"station.decide", "prompts", "gate.intent", "gate.cleanup", "gate.dismiss_corroborated", "gate.edge"} {
			if have[id].Explain == "" {
				t.Errorf("block %s has no explanation from the spec: %+v", id, have[id])
			}
		}
		return
	}
	t.Fatal("triage/cycle not found")
}
