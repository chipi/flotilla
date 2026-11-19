package api

import (
	"context"
	"strings"
	"testing"

	"github.com/chipi/flotilla/internal/runtoken"
	"github.com/chipi/flotilla/internal/store"
)

// A shadow reads the live ledger as of its input's marks plus its own rows, and where both filed one fingerprint its own is current;
// it writes only its own namespace, and a decision it appends is also a pair record under the input's key.
func TestAShadowReadsTheLedgerAsOfItsInput(t *testing.T) {
	ts, srv, reg := runServer(t)
	ctx := context.Background()
	live := "triage/cycle"
	srv.Store.WriteFiling(ctx, live, map[string]string{"fingerprint": "a", "repo": "o/r", "issue": "1", "group_key": "ga"}, "old")
	srv.Store.WriteFiling(ctx, live, map[string]string{"fingerprint": "b", "repo": "o/r", "issue": "2", "group_key": "gb"}, "old")
	srv.Store.AppendEntry(ctx, live, "decision", map[string]string{"fingerprint": "a", "disposition": "file"}, "old")
	srv.Store.AppendEntry(ctx, live, "recurrence", map[string]string{"fingerprint": "a"}, "old")
	mf, me, _ := srv.Store.LedgerMark(ctx, live)
	if err := srv.Store.NewInput(ctx, store.Input{Key: "live-1", Routine: live, Kind: store.InputTick, StartedAt: "2026-11-19T10:00:00.000000000Z", MarkFilings: mf, MarkEntries: me}); err != nil {
		t.Fatal(err)
	}
	// the live run goes on writing after the mark
	srv.Store.WriteFiling(ctx, live, map[string]string{"fingerprint": "c", "repo": "o/r", "issue": "3"}, "live-1")
	srv.Store.AppendEntry(ctx, live, "decision", map[string]string{"fingerprint": "c"}, "live-1")

	tok := issue(t, reg, runtoken.Run{ID: "shadow-1", Routine: live, Version: 2, Shadow: true, InputKey: "live-1"})
	count := func() (float64, float64, float64) {
		c, out := call(t, ts, "GET", "/v1/ledger/counts", tok, "")
		if c != 200 {
			t.Fatalf("counts %d %v", c, out)
		}
		return out["filings"].(float64), out["decisions"].(float64), out["recurrences"].(float64)
	}
	if f, d, r := count(); f != 2 || d != 1 || r != 1 {
		t.Fatalf("the shadow sees the ledger as of the input: %v %v %v", f, d, r)
	}
	if c, out := call(t, ts, "GET", "/v1/ledger/filings?group_key=gc", tok, ""); c != 200 || len(out["filings"].([]any)) != 0 {
		t.Fatalf("a row the live run wrote after the mark is not seen: %d %v", c, out)
	}
	if c, out := call(t, ts, "GET", "/v1/ledger/filings?group_key=ga", tok, ""); c != 200 || len(out["filings"].([]any)) != 1 {
		t.Fatalf("a row as of the mark is: %d %v", c, out)
	}
	// it files the same fingerprint: its own row is the current one for it, and the live ledger is as it was
	if c, out := call(t, ts, "POST", "/v1/ledger/filings", tok, `{"row":{"fingerprint":"a","repo":"o/r","issue":"77","group_key":"ga"}}`); c != 200 {
		t.Fatalf("%d %v", c, out)
	}
	_, out := call(t, ts, "GET", "/v1/ledger/filings?fingerprint=a", tok, "")
	fs := out["filings"].([]any)
	if len(fs) != 1 || fs[0].(map[string]any)["row"].(map[string]any)["issue"] != "77" {
		t.Fatalf("its own row is current: %v", out)
	}
	if cur, _ := srv.Store.Filings(ctx, live, map[string]string{"fingerprint": "a"}); len(cur) != 1 || cur[0].Row["issue"] != "1" {
		t.Fatalf("the live ledger is untouched: %+v", cur)
	}
	// a decision it makes is a ledger row of its own and a pair record under the input's key, by the row's subject
	if c, out := call(t, ts, "POST", "/v1/ledger/entries", tok, `{"kind":"decision","row":{"fingerprint":"fp-9","occurrence_id":"occ-9","disposition":"dismiss"}}`); c != 200 {
		t.Fatalf("%d %v", c, out)
	}
	call(t, ts, "POST", "/v1/ledger/entries", tok, `{"kind":"recurrence","row":{"fingerprint":"fp-9"}}`)
	recs, _ := srv.Store.ShadowRecords(ctx, store.ShadowRecordFilter{InputKey: "live-1"})
	if len(recs) != 1 || recs[0].Kind != "decision" || recs[0].Subject != "occ-9" || recs[0].Routine != "triage/cycle@v2" || recs[0].RunID != "shadow-1" || !strings.Contains(string(recs[0].Payload), "dismiss") {
		t.Fatalf("one pair record, for the decision only: %+v", recs)
	}
	if f, d, r := count(); f != 2 || d != 2 || r != 2 {
		t.Fatalf("counts as of the input plus its own: %v %v %v", f, d, r)
	}
	c, out := call(t, ts, "GET", "/v1/ledger/entries", tok, "")
	if es := out["entries"].([]any); c != 200 || len(es) != 4 {
		t.Fatalf("entries as of the mark, then its own: %d %v", c, out)
	}
}

func TestAShadowWhoseInputIsGoneCannotReadTheLedger(t *testing.T) {
	ts, _, reg := runServer(t)
	tok := issue(t, reg, runtoken.Run{ID: "shadow-1", Routine: "triage/cycle", Version: 2, Shadow: true, InputKey: "no-such-input"})
	for _, p := range []string{"/v1/ledger/filings", "/v1/ledger/entries", "/v1/ledger/counts"} {
		if c, out := call(t, ts, "GET", p, tok, ""); c != 409 {
			t.Errorf("%s: %d %v, want 409", p, c, out)
		}
	}
}

// A live run's ledger reads are what they were.
func TestALiveRunStillReadsItsOwnLedgerPlain(t *testing.T) {
	ts, srv, reg := runServer(t)
	srv.Store.WriteFiling(context.Background(), "triage/cycle", map[string]string{"fingerprint": "a", "issue": "1"}, "old")
	tok := issue(t, reg, runtoken.Run{ID: "run-1"})
	if c, out := call(t, ts, "GET", "/v1/ledger/counts", tok, ""); c != 200 || out["filings"] != float64(1) {
		t.Fatalf("%d %v", c, out)
	}
}
