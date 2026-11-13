package hostaudit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// hostLog writes entries to a real chained log and returns its bytes and head.
func hostLog(t *testing.T, entries ...Entry) ([]byte, string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "audit.jsonl")
	l, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		e.Time = t0
		if _, err := l.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	head := l.Head()
	l.Close()
	b, _ := os.ReadFile(p)
	return b, head
}

func ran(id, status string) []Entry {
	return []Entry{{Event: Started, ExecutionID: id}, {Event: Ended, ExecutionID: id, Status: status}}
}

func bads(fs []Finding) string {
	var out []string
	for _, f := range fs {
		if f.Bad {
			out = append(out, f.Detail)
		}
	}
	return strings.Join(out, " | ")
}

func TestCompareAgrees(t *testing.T) {
	entries := append(ran("x1", "done"), ran("x2", "failed")...)
	entries = append(entries, Entry{Event: Refused, ExecutionID: "x3", Status: "refused", Detail: "its limit"})
	entries = append(entries, Entry{Event: Refused, ExecutionID: "forged-1", Status: "refused", Detail: "signature"})
	entries = append(entries, ran("x4", "done")...) // flotillad lost the connection: it holds x4 as unknown
	log, head := hostLog(t, entries...)
	fs := Compare(log, []Record{{"x1", "done"}, {"x2", "failed"}, {"x3", "refused"}, {"x4", "unknown"}, {"x5", "not_sent"}}, head)
	if Failed(fs) {
		t.Fatalf("an agreeing log failed: %s", bads(fs))
	}
	all := ""
	for _, f := range fs {
		all += f.String() + "\n"
	}
	for _, want := range []string{"x4: flotillad holds it as unknown; the host's log says done", "forged-1: refused by the host", "x1: done on both sides"} {
		if !strings.Contains(all, want) {
			t.Errorf("the audit lacks %q:\n%s", want, all)
		}
	}
}

func TestCompareDisagrees(t *testing.T) {
	log, head := hostLog(t, append(ran("x1", "done"), ran("x9", "done")...)...)
	for name, c := range map[string]struct {
		records []Record
		head    string
		want    string
	}{
		"the host ran one flotillad never sent":  {[]Record{{"x1", "done"}}, head, "x9: the host ran it, and flotillad has no record"},
		"results differ":                         {[]Record{{"x1", "failed"}, {"x9", "done"}}, head, "flotillad recorded failed, the host logged done"},
		"a result the host never logged":         {[]Record{{"x1", "done"}, {"x9", "done"}, {"x7", "done"}}, head, "x7: flotillad recorded done, and the host's log has no result"},
		"never sent, and the host logged it":     {[]Record{{"x1", "done"}, {"x9", "not_sent"}}, head, "x9: flotillad recorded it as never sent"},
		"the head flotillad stored is not there": {[]Record{{"x1", "done"}, {"x9", "done"}}, strings.Repeat("a", 64), "does not lead to the head flotillad stored"},
	} {
		fs := Compare(log, c.records, c.head)
		if !Failed(fs) || !strings.Contains(bads(fs), c.want) {
			t.Errorf("%s: %s", name, bads(fs))
		}
	}
	// a log rewritten in the middle
	broken := strings.Replace(string(log), `"status":"done"`, `"status":"failed"`, 1)
	if fs := Compare([]byte(broken), []Record{{"x1", "done"}, {"x9", "done"}}, head); !Failed(fs) || !strings.Contains(bads(fs), "chain") {
		t.Errorf("a rewritten log: %s", bads(fs))
	}
}
