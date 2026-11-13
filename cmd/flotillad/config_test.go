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
	if _, err := load(t, "case_timers: {detected_alrt: 5m}\n"); err == nil {
		t.Fatal("a misspelled key was accepted")
	}
}
