package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const baseConfig = `listen: 127.0.0.1:8484
specs_dir: /s
state_dir: /v
callers_file: /c
environment: test
`

func load(t *testing.T, extra string) (*Config, error) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "flotillad.yaml")
	if err := os.WriteFile(p, []byte(baseConfig+extra), 0o600); err != nil {
		t.Fatal(err)
	}
	return loadConfig(p)
}

func TestCaseTimersConfig(t *testing.T) {
	c, err := load(t, "case_timers: {detected_alert: 45m, handed_over_escalate: 36h}\n")
	if err != nil {
		t.Fatal(err)
	}
	if c.CaseTimers.DetectedAlert != 45*time.Minute || c.CaseTimers.HandedOverEscalate != 36*time.Hour {
		t.Fatalf("case_timers: %+v", c.CaseTimers)
	}
	if c, err := load(t, ""); err != nil || c.CaseTimers.DetectedAlert != 0 {
		t.Fatalf("unset (the defaults apply in casetimer): %+v %v", c, err)
	}
	if _, err := load(t, "case_timers: {detected_alert: -5m}\n"); err == nil || !strings.Contains(err.Error(), "negative") {
		t.Fatalf("a negative limit: %v", err)
	}
}

// G9 and G11: the slack section needs the host endpoint, the pins and the UI's address; brief_at is HH:MM.
func TestSlackAndBriefConfig(t *testing.T) {
	hosts := "hosts: {listen: ':8487', ca_dir: /ca, server_names: [f]}\n"
	good := "slack: {connector_host: mini, team_id: T0001, ui_url: 'https://f', users: {U0MARKO1: marko}, daily_cap: 5}\n"
	c, err := load(t, hosts+good+"brief_at: '06:30'\n")
	if err != nil {
		t.Fatal(err)
	}
	if c.Slack.Users["U0MARKO1"] != "marko" || c.Slack.Cap() != 5 || c.BriefAt != "06:30" {
		t.Fatalf("parsed: %+v %q", c.Slack, c.BriefAt)
	}
	for what, extra := range map[string]string{
		"slack without hosts":   good,
		"no pinned user":        hosts + "slack: {connector_host: mini, team_id: T0001, ui_url: 'https://f'}\n",
		"a user id that is not": hosts + "slack: {connector_host: mini, team_id: T0001, ui_url: 'https://f', users: {marko: marko}}\n",
		"a cap over 100":        hosts + "slack: {connector_host: mini, team_id: T0001, ui_url: 'https://f', users: {U0MARKO1: marko}, daily_cap: 500}\n",
		"brief_at not HH:MM":    "brief_at: 7am\n",
		"no connector host":     hosts + "slack: {team_id: T0001, ui_url: 'https://f', users: {U0MARKO1: marko}}\n",
	} {
		if _, err := load(t, extra); err == nil {
			t.Errorf("%s: accepted", what)
		}
	}
	if _, err := load(t, "case_timers: {detected_alrt: 5m}\n"); err == nil {
		t.Fatal("a misspelled key was accepted")
	}
}

// RFC-0011: a remote worker is an identity w-<worker>-<node> and needs the host endpoint; never both a
// url and remote; one identity per worker.
func TestRemoteWorkersConfig(t *testing.T) {
	hosts := "hosts: {listen: ':8487', ca_dir: /ca, server_names: [f]}\n"
	c, err := load(t, hosts+"workers:\n  bugfix: {remote: w-bugfix-x3}\n  ops: {url: 'http://o:8081', token_file: /t}\n")
	if err != nil {
		t.Fatal(err)
	}
	if c.Workers["bugfix"].Remote != "w-bugfix-x3" || c.Workers["ops"].URL == "" {
		t.Fatalf("parsed: %+v", c.Workers)
	}
	for what, extra := range map[string]string{
		"remote without hosts": "workers:\n  bugfix: {remote: w-bugfix-x3}\n",
		"not a w- identity":    hosts + "workers:\n  bugfix: {remote: x3}\n",
		"remote and a url":     hosts + "workers:\n  bugfix: {remote: w-bugfix-x3, url: 'http://b:8081'}\n",
		"one identity twice":   hosts + "workers:\n  a: {remote: w-a-x3}\n  b: {remote: w-a-x3}\n",
		"neither":              "workers:\n  bugfix: {}\n",
	} {
		if _, err := load(t, extra); err == nil {
			t.Errorf("%s: accepted", what)
		}
	}
}

// RFC-0002 §7: the knowledge section is validated at load, and a misspelled field is refused.
func TestKnowledgeConfig(t *testing.T) {
	good := "knowledge:\n  homelab-handbook:\n    kind: files\n    volume: flotilla-knowledge\n    paths: [docs/recipes]\n" +
		"    maps:\n      by_service: {litellm: docs/recipes/litellm.md}\n"
	c, err := load(t, good)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Knowledge["homelab-handbook"].Maps["by_service"]["litellm"]; got != "docs/recipes/litellm.md" {
		t.Fatalf("parsed: %+v", c.Knowledge)
	}
	if c, err := load(t, ""); err != nil || len(c.Knowledge) != 0 {
		t.Fatalf("unset: %+v %v", c.Knowledge, err)
	}
	for what, extra := range map[string]string{
		"a glob in paths":          strings.Replace(good, "[docs/recipes]", "[docs/**]", 1),
		"a key outside the paths":  strings.Replace(good, "docs/recipes/litellm.md", "infra/litellm.md", 1),
		"a kind that is not files": strings.Replace(good, "kind: files", "kind: git", 1),
		"a misspelled field":       strings.Replace(good, "volume:", "volum:", 1),
	} {
		if _, err := load(t, extra); err == nil {
			t.Errorf("%s: accepted", what)
		}
	}
}
