package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chipi/flotilla/internal/api"
	"github.com/chipi/flotilla/internal/store"
)

func peopleFile(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "people.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// RFC-0014 §4.6: -check counts the people and warns on a pinned Slack user the people file does not name.
func TestCheckDirectory(t *testing.T) {
	if lines, err := directoryCheckLines(&Config{}); err != nil || !strings.Contains(lines[0], "directory: none") {
		t.Fatalf("%v %v", lines, err)
	}
	p := peopleFile(t, "people:\n  - {email: ada@example.org, name: Ada, ids: {slack: U0ADA}}\n")
	cfg := &Config{Directory: &DirectoryConfig{Source: "file", Path: p, Every: 3600e9},
		Slack: &api.SlackConfig{Users: map[string]string{"U0ADA": "ada", "U0GONE": "gone"}}}
	lines, err := directoryCheckLines(cfg)
	if err != nil {
		t.Fatal(err)
	}
	all := strings.Join(lines, "\n")
	if !strings.Contains(all, "1 people") || !strings.Contains(all, "WARN directory: the pinned Slack user U0GONE (gone) is not in the people file") ||
		strings.Contains(all, "U0ADA (ada)") {
		t.Errorf("%s", all)
	}
	if err := os.Chmod(p, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := directoryCheckLines(cfg); err == nil {
		t.Error("-check passes a people file others can read")
	}
}

// A people file that cannot be read keeps the copy as it was: a directory outage never stops a case.
func TestSyncDirectoryKeepsTheCopyOnAnOutage(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "f.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	p := peopleFile(t, "people:\n  - {email: ada@example.org, name: Ada, ids: {slack: U0ADA}}\n")
	cfg := &DirectoryConfig{Source: "file", Path: p}
	syncDirectory(ctx, cfg, st, log)
	if _, ok, _ := st.PersonBy(ctx, "slack", "U0ADA"); !ok {
		t.Fatal("the first sync did not record Ada")
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	syncDirectory(ctx, cfg, st, log)
	if _, ok, _ := st.PersonBy(ctx, "slack", "U0ADA"); !ok {
		t.Error("a missing people file emptied the copy")
	}
}

// The config refuses a source kind that has no reader and a schedule under a minute, and defaults to hourly.
func TestDirectoryConfig(t *testing.T) {
	for body, want := range map[string]string{
		"directory: {source: ldap, path: /x}":             "source file",
		"directory: {source: file, path: /x, every: 10s}": "at least 1m",
	} {
		p := filepath.Join(t.TempDir(), "flotillad.yaml")
		if err := os.WriteFile(p, []byte(body+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadConfig(p); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", body, err, want)
		}
	}
}
