package main

import (
	"os"
	"path/filepath"
	"testing"
)

// A Slack token file must be its owner's only and hold one token of the right kind.
func TestReadToken(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string, mode os.FileMode) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
		return p
	}
	if tok, err := readToken(write("ok", "xoxb-123\n", 0o600), "xoxb-"); err != nil || tok != "xoxb-123" {
		t.Fatalf("a good file: %q %v", tok, err)
	}
	for what, p := range map[string]string{
		"readable by others": write("open", "xoxb-123\n", 0o644),
		"the other token":    write("app", "xapp-123\n", 0o600),
		"two tokens":         write("two", "xoxb-1 xoxb-2\n", 0o600),
		"missing":            filepath.Join(dir, "none"),
	} {
		if _, err := readToken(p, "xoxb-"); err == nil {
			t.Errorf("%s: accepted", what)
		}
	}
}
