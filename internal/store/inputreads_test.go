package store

import (
	"context"
	"encoding/json"
	"testing"
)

func TestInputReadsAreKeptAfterTheRunAndJoinedToItsInput(t *testing.T) {
	s, ctx := openTest(t), context.Background()
	if err := s.NewInput(ctx, Input{Key: "live-1", Routine: "triage/cycle", Kind: InputTick, StartedAt: "t"}); err != nil {
		t.Fatal(err)
	}
	if in, err := s.Input(ctx, "live-1"); err != nil || in.Reads != nil || in.ReadsComplete {
		t.Fatalf("an input before its run's reads are written has none: %+v %v", in, err)
	}
	lines := []json.RawMessage{
		json.RawMessage(`{"station":"decide","tool":"wiki.get_page","args":{"id":1},"result":"page","ok":true,"chars":4,"cut":false}`),
		json.RawMessage(`{"station":"decide","tool":"wiki.get_page","args":{"id":2},"result":"<read failed: x>","ok":false}`),
	}
	if err := s.RecordInputReads(ctx, "live-1", lines, true); err != nil {
		t.Fatal(err)
	}
	in, err := s.Input(ctx, "live-1")
	if err != nil || !in.ReadsComplete || len(in.Reads) != 2 || string(in.Reads[0]) != string(lines[0]) {
		t.Fatalf("the input reads back with its reads, in order, as written: %+v %v", in, err)
	}
	// the second write of a run replaces the first, and an incomplete record says so
	if err := s.RecordInputReads(ctx, "live-1", lines[:1], false); err != nil {
		t.Fatal(err)
	}
	r, ok, err := s.InputReadsOf(ctx, "live-1")
	if err != nil || !ok || r.Complete || len(r.Lines) != 1 {
		t.Fatalf("%+v %v %v", r, ok, err)
	}
	if _, ok, err := s.InputReadsOf(ctx, "live-2"); ok || err != nil {
		t.Fatalf("a run with no record has none: %v %v", ok, err)
	}
	// an incomplete record can have no lines at all (a file the worker could not open)
	if err := s.RecordInputReads(ctx, "live-3", nil, false); err != nil {
		t.Fatal(err)
	}
	if r, ok, _ := s.InputReadsOf(ctx, "live-3"); !ok || r.Complete || len(r.Lines) != 0 {
		t.Fatalf("%+v %v", r, ok)
	}
	for name, l := range map[string][]json.RawMessage{"not an object": {json.RawMessage(`"x"`)}, "not JSON": {json.RawMessage(`{`)}} {
		if err := s.RecordInputReads(ctx, "live-4", l, true); err == nil {
			t.Errorf("%s: a record is never half of what was sent", name)
		}
	}
	if err := s.RecordInputReads(ctx, "", lines, true); err == nil {
		t.Error("reads need a run id")
	}
}
