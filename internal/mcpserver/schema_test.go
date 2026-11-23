package mcpserver

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Every read tool's structured result matches the output schema the server lists for it (2026-11-23: a caller reported status
// failing its own schema, "data/routines/0 must NOT have additional properties").
// brief_show needs a written brief, and ledger_query lists no output schema (its answer is a namespace list or decisions).
func TestEachToolsResultMatchesItsOwnOutputSchema(t *testing.T) {
	_, ts := world(t)
	cs := session(t, ts.URL, agentTok)
	tools, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	schemas := map[string]any{}
	for _, tl := range tools.Tools {
		schemas[tl.Name] = tl.OutputSchema
	}
	for _, name := range []string{"status", "runs_list", "cases_list", "approvals_list", "hosts_list", "models_calls"} {
		t.Run(name, func(t *testing.T) {
			raw, err := json.Marshal(schemas[name])
			if err != nil || string(raw) == "null" {
				t.Fatalf("no output schema listed: %v", err)
			}
			var s jsonschema.Schema
			if err := json.Unmarshal(raw, &s); err != nil {
				t.Fatal(err)
			}
			rs, err := s.Resolve(nil)
			if err != nil {
				t.Fatal(err)
			}
			r, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: map[string]any{}})
			if err != nil || r.IsError {
				t.Fatalf("call: %v %+v", err, r)
			}
			b, _ := json.Marshal(r.StructuredContent)
			var inst any
			_ = json.Unmarshal(b, &inst)
			if err := rs.Validate(inst); err != nil {
				t.Fatalf("the result does not match the listed schema: %v\nresult: %.600s", err, b)
			}
		})
	}
}
