// Package models validates the installation's `models` section (flotillad.yaml) and hands it to jobs.
//
// The section maps model aliases (what specs name) to OpenAI-compatible endpoints (ADR-0004, RFC-0002
// §3). flotillad validates it at load, and every run gets it as JSON in FLOTILLA_MODELS; the Python
// runtime's model client (runtime/model_client.py) reads it from there and applies the same rules, so a
// bad section fails flotillad's -check before any job sees it. Both sides are tested against the same
// fixtures (runtime/testdata/models-*.json). Keys are named (key_env, key_file), never held here.
package models

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
)

type Config struct {
	Endpoints map[string]Endpoint `yaml:"endpoints" json:"endpoints"`
	Aliases   map[string]Alias    `yaml:"aliases" json:"aliases"`
	Prices    map[string]Price    `yaml:"prices,omitempty" json:"prices,omitempty"`
}

type Endpoint struct {
	BaseURL  string       `yaml:"base_url" json:"base_url"`
	Kind     string       `yaml:"kind,omitempty" json:"kind,omitempty"`
	KeyEnv   string       `yaml:"key_env,omitempty" json:"key_env,omitempty"`
	KeyFile  string       `yaml:"key_file,omitempty" json:"key_file,omitempty"`
	Defaults Capabilities `yaml:"defaults,omitempty" json:"defaults,omitempty"`
}

type Capabilities struct {
	Tools string `yaml:"tools,omitempty" json:"tools,omitempty"`
	JSON  string `yaml:"json,omitempty" json:"json,omitempty"`
	Cost  string `yaml:"cost,omitempty" json:"cost,omitempty"`
}

type Alias struct {
	Endpoint     string `yaml:"endpoint" json:"endpoint"`
	Model        string `yaml:"model" json:"model"`
	Capabilities `yaml:",inline" json:",inline"`
}

// MarshalJSON flattens the capabilities into the alias, as the YAML has them.
func (a Alias) MarshalJSON() ([]byte, error) {
	m := map[string]string{"endpoint": a.Endpoint, "model": a.Model}
	for k, v := range map[string]string{"tools": a.Tools, "json": a.JSON, "cost": a.Cost} {
		if v != "" {
			m[k] = v
		}
	}
	return json.Marshal(m)
}

// UnmarshalJSON reads the flat form back (the shared fixtures are JSON).
func (a *Alias) UnmarshalJSON(b []byte) error {
	var m map[string]string
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	for k := range m {
		switch k {
		case "endpoint", "model", "tools", "json", "cost":
		default:
			return fmt.Errorf("unknown field %q", k)
		}
	}
	*a = Alias{Endpoint: m["endpoint"], Model: m["model"], Capabilities: Capabilities{Tools: m["tools"], JSON: m["json"], Cost: m["cost"]}}
	return nil
}

type Price struct {
	InputPerMTok  *float64 `yaml:"input_per_mtok" json:"input_per_mtok"`
	OutputPerMTok *float64 `yaml:"output_per_mtok" json:"output_per_mtok"`
}

var (
	tools = map[string]bool{"native": true, "json": true, "none": true}
	jsons = map[string]bool{"schema": true, "object": true, "none": true}
	costs = map[string]bool{"reported": true, "table": true, "zero": true, "unknown": true}
	kinds = map[string]bool{"litellm": true, "openai": true}
)

func or(v, d string) string {
	if v == "" {
		return d
	}
	return v
}

// Validate applies runtime/model_client.load's rules. Errors are sorted, so -check output is stable.
func (c *Config) Validate() error {
	var errs []string
	for name, e := range c.Endpoints {
		if e.BaseURL == "" {
			errs = append(errs, fmt.Sprintf("endpoint %s: base_url is required", name))
		}
		if e.KeyEnv != "" && e.KeyFile != "" {
			errs = append(errs, fmt.Sprintf("endpoint %s: key_file or key_env, not both", name))
		}
		if !kinds[or(e.Kind, "openai")] {
			errs = append(errs, fmt.Sprintf("endpoint %s: kind %q: litellm or openai", name, e.Kind))
		}
	}
	for name, a := range c.Aliases {
		e, ok := c.Endpoints[a.Endpoint]
		if !ok {
			errs = append(errs, fmt.Sprintf("alias %s: endpoint %q is not defined", name, a.Endpoint))
			continue
		}
		if a.Model == "" {
			errs = append(errs, fmt.Sprintf("alias %s: model is required", name))
		}
		t, j, k := or(a.Tools, or(e.Defaults.Tools, "none")), or(a.JSON, or(e.Defaults.JSON, "none")), or(a.Cost, or(e.Defaults.Cost, "unknown"))
		if !tools[t] || !jsons[j] || !costs[k] {
			errs = append(errs, fmt.Sprintf("alias %s: tools %q / json %q / cost %q not allowed", name, t, j, k))
		}
		if k == "table" {
			p, ok := c.Prices[name]
			if !ok || p.InputPerMTok == nil || p.OutputPerMTok == nil || *p.InputPerMTok < 0 || *p.OutputPerMTok < 0 {
				errs = append(errs, fmt.Sprintf("alias %s: cost: table needs prices.%s.input_per_mtok and output_per_mtok", name, name))
			}
		}
	}
	if len(errs) == 0 {
		return nil
	}
	sort.Strings(errs)
	return errors.New("models: " + fmt.Sprint(errs))
}

// JSON is what a run gets in FLOTILLA_MODELS.
func (c *Config) JSON() (string, error) {
	b, err := json.Marshal(c)
	return string(b), err
}
