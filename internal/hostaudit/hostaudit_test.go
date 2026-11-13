package hostaudit

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 13, 20, 0, 0, 0, time.UTC)

// A log of three lines, written, closed and reopened.
func written(t *testing.T) (string, []string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	l, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	var heads []string
	for i, e := range []Entry{
		{Event: Started, ExecutionID: "x1", Action: "backup.rerun_observability", Approved: "user:marko", Argv: []string{"/usr/bin/sudo", "-n", "/bin/launchctl", "kickstart", "system/com.homelab.db-backup"}},
		{Event: Ended, ExecutionID: "x1", Status: "done", OutputSHA: strings.Repeat("a", 64)},
		{Event: Refused, ExecutionID: "x2", Detail: "past its expiry"},
	} {
		e.Time = t0.Add(time.Duration(i) * time.Second)
		h, err := l.Append(e)
		if err != nil {
			t.Fatal(err)
		}
		heads = append(heads, h)
	}
	if l.Head() != heads[2] {
		t.Fatalf("head %s, want %s", l.Head(), heads[2])
	}
	l.Close()
	return path, heads
}

func TestAppendReopenContinue(t *testing.T) {
	path, heads := written(t)
	l, err := Open(path)
	if err != nil || l.Head() != heads[2] {
		t.Fatalf("reopened: head %q %v", l.Head(), err)
	}
	h, err := l.Append(Entry{Time: t0, Event: Resent, ExecutionID: "x1"})
	l.Close()
	if err != nil || h == heads[2] {
		t.Fatalf("appended after reopen: %q %v", h, err)
	}
	f, _ := os.Open(path)
	defer f.Close()
	hashes, err := Verify(f)
	if err != nil || len(hashes) != 4 || hashes[2] != heads[2] || hashes[3] != h {
		t.Fatalf("verify: %v %v", hashes, err)
	}
	for i, head := range append(heads, h, "") {
		if !Leads(hashes, head) {
			t.Errorf("does not lead to head %d", i)
		}
	}
}

// Each way of changing a written log breaks it, or shows against the head flotillad stored.
func TestTampering(t *testing.T) {
	path, heads := written(t)
	good, _ := os.ReadFile(path)
	lines := strings.SplitAfter(string(good), "\n")[:3]
	for name, body := range map[string]string{
		"a line rewritten":       strings.Replace(string(good), `"status":"done"`, `"status":"failed"`, 1),
		"a line removed":         lines[0] + lines[2],
		"the first line removed": lines[1] + lines[2],
		"lines swapped":          lines[1] + lines[0] + lines[2],
		"a line repeated":        lines[0] + lines[0] + lines[1] + lines[2],
		"a torn last line":       strings.TrimSuffix(string(good), "\n"),
		"an unknown field":       strings.Replace(string(good), `"event":"refused"`, `"event":"refused","x":1`, 1),
		"not JSON":               lines[0] + "garbage\n",
	} {
		if body == string(good) {
			t.Fatalf("%s: the test's edit did not apply", name)
		}
		if _, err := Verify(strings.NewReader(body)); err == nil {
			t.Errorf("%s: verified", name)
		}
		p := filepath.Join(t.TempDir(), "audit.jsonl")
		os.WriteFile(p, []byte(body), 0o640)
		if _, err := Open(p); err == nil {
			t.Errorf("%s: opened", name)
		}
	}
	// The last line rewritten whole, or removed: the log still verifies, but no longer leads to the head
	// flotillad stored.
	rest := lines[0] + lines[1]
	hashes, err := Verify(strings.NewReader(rest))
	if err != nil || Leads(hashes, heads[2]) {
		t.Errorf("the last line removed: %v, leads %v", err, Leads(hashes, heads[2]))
	}
	rewritten := rest + strings.Replace(lines[2], "past its expiry", "a different reason", 1)
	hashes, err = Verify(strings.NewReader(rewritten))
	if err != nil || Leads(hashes, heads[2]) {
		t.Errorf("the last line rewritten: %v, leads %v", err, Leads(hashes, heads[2]))
	}
}

func TestEmpty(t *testing.T) {
	hashes, err := Verify(bytes.NewReader(nil))
	if err != nil || len(hashes) != 0 || !Leads(hashes, "") || Leads(hashes, strings.Repeat("a", 64)) {
		t.Fatalf("empty: %v %v", hashes, err)
	}
}
