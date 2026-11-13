package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestHistoryReads(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "h.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	add := func(ns, kind, fp, disp string) {
		t.Helper()
		if _, err := s.AppendEntry(ctx, ns, kind, map[string]string{"fingerprint": fp, "occurrence_id": fp + "@1", "disposition": disp}, ""); err != nil {
			t.Fatal(err)
		}
	}
	add("triage/cycle", "decision", "a", "dismiss") // seq 1
	add("triage/cycle", "recurrence", "a", "recurrence")
	add("triage/cycle", "recurrence", "a", "recurrence")
	add("triage/cycle", "decision", "b", "file")
	add("triage/cycle", "decision", "a", "file") // a decided again: the two recurrences belong to the first
	add("triage/cycle", "recurrence", "a", "recurrence")
	add("triage/cycle-shadow", "decision", "c", "escalate")

	d, err := s.RecentDecisions(ctx, "triage/cycle", 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	got := []string{}
	for _, x := range d {
		got = append(got, x.Row["fingerprint"]+":"+x.Row["disposition"]+":"+string(rune('0'+x.Recurrences)))
	}
	want := "a:file:1 b:file:0 a:dismiss:2"
	if join(got) != want {
		t.Fatalf("decisions newest first with their recurrences: %q, want %q", join(got), want)
	}
	if page, _ := s.RecentDecisions(ctx, "triage/cycle", d[0].Seq, 1); len(page) != 1 || page[0].Row["fingerprint"] != "b" {
		t.Fatalf("paging before a seq: %+v", page)
	}
	ns, _ := s.Namespaces(ctx)
	if join(ns) != "triage/cycle triage/cycle-shadow" {
		t.Fatalf("namespaces: %v", ns)
	}
	if r, _ := s.Run(ctx, "nope"); r != nil {
		t.Fatal("a run that does not exist")
	}
}

func join(xs []string) string {
	out := ""
	for i, x := range xs {
		if i > 0 {
			out += " "
		}
		out += x
	}
	return out
}
