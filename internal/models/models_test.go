package models

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func decode(t *testing.T, raw []byte) (*Config, error) {
	t.Helper()
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	var c Config
	if err := d.Decode(&c); err != nil {
		return nil, err
	}
	return &c, c.Validate()
}

// The fixtures are shared with runtime/test_model_client.py: both sides refuse the same sections.
func TestSharedFixtures(t *testing.T) {
	raw, err := os.ReadFile("../../runtime/testdata/models-valid.json")
	if err != nil {
		t.Fatal(err)
	}
	c, err := decode(t, raw)
	if err != nil {
		t.Fatalf("valid fixture refused: %v", err)
	}
	if len(c.Aliases) != 3 {
		t.Fatalf("aliases: %v", c.Aliases)
	}
	raw, err = os.ReadFile("../../runtime/testdata/models-invalid.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases map[string]json.RawMessage
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) < 10 {
		t.Fatalf("only %d invalid cases", len(cases))
	}
	for name, cfg := range cases {
		if _, err := decode(t, cfg); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// The YAML form (flotillad.yaml) carries the capabilities inline on an alias, and FLOTILLA_MODELS
// carries them flat: what Python's load() reads.
func TestYAMLToRunJSON(t *testing.T) {
	src := `
endpoints:
  gateway: {base_url: "http://litellm:4000/v1", kind: litellm, key_env: OPENROUTER_API_KEY, defaults: {tools: native, json: object, cost: reported}}
aliases:
  local-dgx: {endpoint: gateway, model: qwen, tools: none, cost: zero}
`
	d := yaml.NewDecoder(strings.NewReader(src))
	d.KnownFields(true)
	var c Config
	if err := d.Decode(&c); err != nil {
		t.Fatal(err)
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	j, err := c.JSON()
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]map[string]map[string]any
	if err := json.Unmarshal([]byte(j), &back); err != nil {
		t.Fatal(err)
	}
	a := back["aliases"]["local-dgx"]
	if a["tools"] != "none" || a["cost"] != "zero" || a["model"] != "qwen" || a["json"] != nil {
		t.Fatalf("alias in the run's JSON: %v (from %s)", a, j)
	}
	if back["endpoints"]["gateway"]["key_env"] != "OPENROUTER_API_KEY" {
		t.Fatalf("endpoint: %v", back["endpoints"]["gateway"])
	}
	d = yaml.NewDecoder(strings.NewReader(strings.Replace(src, "tools: none", "tools: none, fallback: x", 1)))
	d.KnownFields(true)
	if err := d.Decode(&Config{}); err == nil {
		t.Fatal("an unknown alias field in YAML was accepted")
	}
}
