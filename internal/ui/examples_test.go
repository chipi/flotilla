package ui

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/chipi/flotilla/internal/spec"
)

// The Examples page lists the catalogue read-only: each example with its id, a plain "example, not running" label and its flow, and nothing of
// the store (no run, no cost). The fleets page links to it only when there is a catalogue.
func TestExamplesPageListsTheCatalogueAndTheFleetsPageLinksToIt(t *testing.T) {
	f := setup(t)
	c, err := spec.LoadExamples(filepath.Join("..", "..", "fleets"))
	if err != nil {
		t.Fatal(err)
	}
	ck, _ := f.csrfOf(f.addOperator())

	_, page := f.do("GET", "/fleets", ck, nil, nil)
	if strings.Contains(page, "/examples") {
		t.Error("the fleets page links to examples without a catalogue")
	}
	resp, page := f.do("GET", "/examples", ck, nil, nil)
	if resp.StatusCode != 200 || !strings.Contains(page, "no examples catalogue") {
		t.Fatalf("without a catalogue: %d %.200s", resp.StatusCode, page)
	}

	f.ui.API.Examples = c
	_, page = f.do("GET", "/fleets", ck, nil, nil)
	if !strings.Contains(page, `href="/examples"`) {
		t.Error("the fleets page does not link to the catalogue")
	}
	resp, page = f.do("GET", "/examples", ck, nil, nil)
	if resp.StatusCode != 200 {
		t.Fatal(resp.StatusCode)
	}
	for _, want := range []string{"example-triage/cycle", "example-bugfix/fix", "example-jobs/backup-monitor", "example, not running", "Its flow",
		"station decide", "nothing here is scheduled", "copy its folder into your fleets repo"} {
		if !strings.Contains(page, want) {
			t.Errorf("the Examples page lacks %q", want)
		}
	}
	for _, not := range []string{"Run now", "Promote", "<form", "chip--ok"} {
		if strings.Contains(page, not) {
			t.Errorf("the Examples page has %q: an example takes no action", not)
		}
	}
	if resp, _ := f.do("GET", "/fleets/example-triage", ck, nil, nil); resp.StatusCode == 200 {
		t.Error("an example fleet has no fleet page of its own")
	}
	if resp, _ := f.do("GET", "/fleets/example-triage/cycle", ck, nil, nil); resp.StatusCode == 200 {
		t.Error("an example has no routine page: that page is the store's")
	}
}
