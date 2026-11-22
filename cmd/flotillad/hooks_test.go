package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chipi/flotilla/internal/spec"
)

func hookSecretFile(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "meeting.secret")
	if err := os.WriteFile(p, []byte(strings.Repeat("s", 40)), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestHooksConfig(t *testing.T) {
	p := hookSecretFile(t)
	c, err := load(t, `hooks_listen: 0.0.0.0:8488
hooks:
  - name: meeting
    secret_file: `+p+`
    kind: meeting
    title: "A meeting's notes arrived"
    project: meetings
    target: file
    target_pattern: '[A-Za-z0-9._-]{1,120}'
    fields: {file: {type: text, max: 120}, text: {type: text, max: 20000}}
  - name: smoke
    secret_file: `+p+`
    kind: smoke
    title: "The plumbing's smoke test"
    target: probe
    target_pattern: '[a-z0-9-]{1,40}'
    fields: {probe: {type: text, max: 40}}
    close_on_arrival: true
`)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Hooks) != 2 || hooksByName(c.Hooks)["smoke"] == nil {
		t.Fatalf("hooks: %+v", c.Hooks)
	}
	lines := hookLines(c, []*spec.Routine{{Fleet: "meetings", Routine: "notes", Enabled: true, Trigger: spec.Trigger{Case: &spec.CaseTrigger{Kind: "meeting"}}}})
	if len(lines) != 2 || !strings.Contains(lines[0], "[meetings/notes]") || !strings.Contains(lines[1], "closes it on arrival") {
		t.Fatalf("-check lines: %q", lines)
	}
	if lines := hookLines(c, nil); !strings.HasPrefix(lines[0], "WARNING hook meeting") {
		t.Fatalf("no routine on the kind: %q", lines)
	}
}

func TestAHookWithoutItsSecretFailsTheConfig(t *testing.T) {
	_, err := load(t, `hooks_listen: 0.0.0.0:8488
hooks:
  - {name: meeting, secret_file: /nonexistent, kind: meeting, title: t, target: file, target_pattern: 'x', fields: {file: {type: text, max: 5}}}
`)
	if err == nil || !strings.Contains(err.Error(), "hooks: meeting: secret_file") {
		t.Fatalf("a missing secret: %v", err)
	}
}

func TestHooksNeedTheirOwnListener(t *testing.T) {
	p := hookSecretFile(t)
	_, err := load(t, `hooks:
  - {name: meeting, secret_file: `+p+`, kind: meeting, title: t, target: file, target_pattern: 'x', fields: {file: {type: text, max: 5}}}
`)
	if err == nil || !strings.Contains(err.Error(), "hooks_listen") {
		t.Fatalf("hooks without hooks_listen: %v", err)
	}
}
