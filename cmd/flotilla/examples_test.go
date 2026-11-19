package main

import (
	"bytes"
	"strings"
	"testing"
	"text/tabwriter"
)

// `flotilla status` lists the examples catalogue after the routines with the state example, and none of the columns a routine that runs has.
func TestStatusPrintsExamplesAsExamples(t *testing.T) {
	var buf bytes.Buffer
	w := tabwriter.NewWriter(&buf, 0, 2, 2, ' ', 0)
	exampleRows(w, []map[string]any{{"id": "example-triage/cycle", "trigger": "every 10m"}, {"id": "example-bugfix/fix", "trigger": "case handed over: code_bug"}})
	w.Flush()
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("%q", buf.String())
	}
	f := strings.Fields(lines[0])
	if f[0] != "example-triage/cycle" || f[len(f)-1] != "example" || !strings.Contains(lines[0], "every 10m") || strings.Contains(lines[0], "$") {
		t.Errorf("%q", lines[0])
	}
}
