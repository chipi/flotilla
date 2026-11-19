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
	good := "slack: {connector_host: host-01, team_id: T0001, ui_url: 'https://f', users: {U0OPER1: ada}, daily_cap: 5}\n"
	c, err := load(t, hosts+good+"brief_at: '06:30'\n")
	if err != nil {
		t.Fatal(err)
	}
	if c.Slack.Users["U0OPER1"] != "ada" || c.Slack.Cap() != 5 || c.BriefAt != "06:30" {
		t.Fatalf("parsed: %+v %q", c.Slack, c.BriefAt)
	}
	for what, extra := range map[string]string{
		"slack without hosts":   good,
		"no pinned user":        hosts + "slack: {connector_host: host-01, team_id: T0001, ui_url: 'https://f'}\n",
		"a user id that is not": hosts + "slack: {connector_host: host-01, team_id: T0001, ui_url: 'https://f', users: {ada: ada}}\n",
		"a cap over 100":        hosts + "slack: {connector_host: host-01, team_id: T0001, ui_url: 'https://f', users: {U0OPER1: ada}, daily_cap: 500}\n",
		"brief_at not HH:MM":    "brief_at: 7am\n",
		"no connector host":     hosts + "slack: {team_id: T0001, ui_url: 'https://f', users: {U0OPER1: ada}}\n",
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
	c, err := load(t, hosts+"workers:\n  bugfix: {remote: w-bugfix-worker-host}\n  ops: {url: 'http://o:8081', token_file: /t}\n")
	if err != nil {
		t.Fatal(err)
	}
	if c.Workers["bugfix"].Remote != "w-bugfix-worker-host" || c.Workers["ops"].URL == "" {
		t.Fatalf("parsed: %+v", c.Workers)
	}
	for what, extra := range map[string]string{
		"remote without hosts": "workers:\n  bugfix: {remote: w-bugfix-worker-host}\n",
		"not a w- identity":    hosts + "workers:\n  bugfix: {remote: worker-host}\n",
		"remote and a url":     hosts + "workers:\n  bugfix: {remote: w-bugfix-worker-host, url: 'http://b:8081'}\n",
		"one identity twice":   hosts + "workers:\n  a: {remote: w-a-worker-host}\n  b: {remote: w-a-worker-host}\n",
		"neither":              "workers:\n  bugfix: {}\n",
	} {
		if _, err := load(t, extra); err == nil {
			t.Errorf("%s: accepted", what)
		}
	}
}

// routine_actions.max_autonomy: propose (the default, so deploying the option changes no decision) or auto; -check says which.
func TestRoutineActionsConfig(t *testing.T) {
	for extra, want := range map[string]string{"": "propose", "routine_actions: {}\n": "propose", "routine_actions: {max_autonomy: propose}\n": "propose",
		"routine_actions: {max_autonomy: auto}\n": "auto"} {
		c, err := load(t, extra)
		if err != nil || c.RoutineActions.MaxAutonomy != want {
			t.Errorf("%q: max_autonomy %q %v, want %q", extra, c.RoutineActions.MaxAutonomy, err, want)
			continue
		}
		if line := routineActionsLine(c); !strings.Contains(line, "max autonomy "+want) {
			t.Errorf("%q: -check says %q", extra, line)
		}
	}
	for _, bad := range []string{"routine_actions: {max_autonomy: record}\n", "routine_actions: {max_autonomy: Auto}\n", "routine_actions: {max_autonomy: yes}\n",
		"routine_actions: {max: auto}\n"} {
		if _, err := load(t, bad); err == nil || !strings.Contains(err.Error(), "routine_actions") && !strings.Contains(err.Error(), "max") {
			t.Errorf("%q: %v", bad, err)
		}
	}
}

// run_tokens: accept (the default) or required.
func TestRunTokensConfig(t *testing.T) {
	for extra, want := range map[string]string{"": RunTokensAccept, "run_tokens: accept\n": RunTokensAccept, "run_tokens: required\n": RunTokensRequired} {
		c, err := load(t, extra)
		if err != nil || c.RunTokens != want {
			t.Errorf("%q: run_tokens %q %v, want %q", extra, c.RunTokens, err, want)
		}
	}
	for _, bad := range []string{"run_tokens: optional\n", "run_tokens: true\n", "run_tokens: Required\n"} {
		if _, err := load(t, bad); err == nil || !strings.Contains(err.Error(), "run_tokens") {
			t.Errorf("%q: %v", bad, err)
		}
	}
}
