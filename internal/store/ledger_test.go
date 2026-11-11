package store

import (
	"context"
	"fmt"
	"math/rand"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func filing(fp, issue, group, norm, alert string) map[string]string {
	return map[string]string{"fingerprint": fp, "repo": "o/r", "issue": issue, "group_key": group, "filed_at": "t",
		"last_comment_day": "", "norm_key": norm, "alert_key": alert}
}

func fps(fs []Filing) string {
	var out []string
	for _, f := range fs {
		out = append(out, f.Row["fingerprint"]+"#"+f.Row["issue"])
	}
	return strings.Join(out, ",")
}

func TestFilings(t *testing.T) {
	s, ctx := openTest(t), context.Background()
	for _, f := range []map[string]string{
		filing("a", "1", "g1", "n1", ""), filing("b", "2", "", "n1", "al"), filing("c", "3", "g2", "", "al"),
	} {
		if _, err := s.WriteFiling(ctx, "ns", f, "run-1"); err != nil {
			t.Fatal(err)
		}
	}
	// a rewrite of "a" (a new comment day, a new norm key) becomes its current row, at the end
	if _, err := s.WriteFiling(ctx, "ns", filing("a", "1", "g1", "n9", ""), "run-2"); err != nil {
		t.Fatal(err)
	}
	all, _ := s.Filings(ctx, "ns", nil)
	if got := fps(all); got != "b#2,c#3,a#1" {
		t.Fatalf("current filings in ledger order: %s", got)
	}
	for keys, want := range map[string]string{
		"norm_key=n1":                   "b#2", // a's old key no longer matches
		"norm_key=n9":                   "a#1",
		"alert_key=al":                  "b#2,c#3",
		"fingerprint=c,group_key=g1":    "c#3,a#1",
		"group_key=,norm_key=":          "", // empty values never match
		"fingerprint=zz,group_key=nope": "",
	} {
		m := map[string]string{}
		for _, kv := range strings.Split(keys, ",") {
			k, v, _ := strings.Cut(kv, "=")
			m[k] = v
		}
		got, err := s.Filings(ctx, "ns", m)
		if err != nil {
			t.Fatal(err)
		}
		if fps(got) != want {
			t.Errorf("%s: got %q, want %q", keys, fps(got), want)
		}
	}
	if _, err := s.Filings(ctx, "ns", map[string]string{"title": "x"}); err == nil {
		t.Error("an unknown key must be refused")
	}
	if other, _ := s.Filings(ctx, "other", nil); len(other) != 0 {
		t.Error("namespaces must not share filings")
	}
	if _, err := s.WriteFiling(ctx, "ns", map[string]string{"repo": "x"}, ""); err == nil {
		t.Error("a filing without a fingerprint must be refused")
	}
}

// The store's current filings equal what filed.tsv holds after the same writes: filing.ledger_upsert
// drops the fingerprint's row and appends the new one.
func TestFilingsMatchTheTSVUpsert(t *testing.T) {
	s, ctx := openTest(t), context.Background()
	r := rand.New(rand.NewSource(7))
	var tsv []map[string]string
	for i := 0; i < 400; i++ {
		f := filing(fmt.Sprintf("fp%d", r.Intn(40)), fmt.Sprint(i), fmt.Sprintf("g%d", r.Intn(5)), fmt.Sprintf("n%d", r.Intn(8)), "")
		if _, err := s.WriteFiling(ctx, "ns", f, ""); err != nil {
			t.Fatal(err)
		}
		var kept []map[string]string
		for _, x := range tsv {
			if x["fingerprint"] != f["fingerprint"] {
				kept = append(kept, x)
			}
		}
		tsv = append(kept, f)
	}
	all, _ := s.Filings(ctx, "ns", nil)
	var got []map[string]string
	for _, f := range all {
		got = append(got, f.Row)
	}
	if !reflect.DeepEqual(got, tsv) {
		t.Fatalf("store and TSV differ: %d rows against %d", len(got), len(tsv))
	}
	for n := 0; n < 8; n++ {
		key := fmt.Sprintf("n%d", n)
		var want []map[string]string
		for _, x := range tsv {
			if x["norm_key"] == key {
				want = append(want, x)
			}
		}
		sub, _ := s.Filings(ctx, "ns", map[string]string{"norm_key": key})
		var gotSub []map[string]string
		for _, f := range sub {
			gotSub = append(gotSub, f.Row)
		}
		if !reflect.DeepEqual(gotSub, want) {
			t.Fatalf("norm_key %s: the store's subset differs from the TSV's", key)
		}
	}
}

func TestEntriesAndImport(t *testing.T) {
	s, ctx := openTest(t), context.Background()
	entries := []Entry{
		{Kind: "decision", Row: map[string]string{"ts": "1", "occurrence_id": "o1", "fingerprint": "f", "disposition": "file"}},
		{Kind: "recurrence", Row: map[string]string{"ts": "2", "occurrence_id": "o1", "fingerprint": "f", "disposition": "recurrence"}},
		{Kind: "decision", Row: map[string]string{"ts": "3", "occurrence_id": "o2", "fingerprint": "g", "disposition": "dismiss"}},
	}
	c, err := s.ImportLedger(ctx, "ns", []map[string]string{filing("f", "1", "", "n", "")}, entries)
	if err != nil {
		t.Fatal(err)
	}
	if c != (LedgerCounts{Filings: 1, Decisions: 2, Recurrences: 1}) {
		t.Fatalf("counts: %+v", c)
	}
	if _, err := s.ImportLedger(ctx, "ns", nil, entries[:1]); err == nil || !strings.Contains(err.Error(), "once") {
		t.Fatalf("a second import must be refused, got %v", err)
	}
	if _, err := s.AppendEntry(ctx, "ns", "decision", map[string]string{"ts": "4", "occurrence_id": "o3"}, "run-9"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendEntry(ctx, "ns", "verdict", map[string]string{}, ""); err == nil {
		t.Error("an unknown entry kind must be refused")
	}
	all, _ := s.Entries(ctx, "ns", 0, 0)
	if len(all) != 4 || all[1].Kind != "recurrence" || !all[0].Imported || all[3].Imported || all[3].Row["ts"] != "4" {
		t.Fatalf("entries: %+v", all)
	}
	page, _ := s.Entries(ctx, "ns", all[1].Seq, 1)
	if len(page) != 1 || page[0].Seq != all[2].Seq {
		t.Fatalf("a page after seq %d: %+v", all[1].Seq, page)
	}
}
