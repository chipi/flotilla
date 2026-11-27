package hostwire

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
	"time"
)

func TestRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	at := time.Date(2026, 11, 13, 12, 0, 0, 5, time.UTC)
	if err := Write(&buf, Heartbeat, HeartbeatBody{Clock: at, ChainHead: "abc"}); err != nil {
		t.Fatal(err)
	}
	if err := Write(&buf, Execute, ExecuteBody{Envelope: []byte{0, 1, 2}}); err != nil {
		t.Fatal(err)
	}
	f, err := Read(&buf)
	var hb HeartbeatBody
	if err != nil || f.Type != Heartbeat || f.Decode(&hb) != nil || !hb.Clock.Equal(at) || hb.ChainHead != "abc" {
		t.Fatalf("heartbeat: %+v %+v %v", f, hb, err)
	}
	f, err = Read(&buf)
	var eb ExecuteBody
	if err != nil || f.Decode(&eb) != nil || !bytes.Equal(eb.Envelope, []byte{0, 1, 2}) {
		t.Fatalf("execute: %+v %v", eb, err)
	}
}

func frame(n uint32, body string) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, n)
	return append(b, body...)
}

func TestRefusals(t *testing.T) {
	for name, raw := range map[string][]byte{
		"over the maximum": frame(MaxFrame+1, ""),
		"empty":            frame(0, ""),
		"truncated":        frame(20, `{"type":"x"}`),
		"not JSON":         frame(3, "abc"),
		"no type":          frame(2, "{}"),
		"a short header":   {0, 0},
	} {
		if _, err := Read(bytes.NewReader(raw)); err == nil {
			t.Errorf("%s: read", name)
		}
	}
	// over the maximum: refused from the header alone, before reading a body
	if _, err := Read(bytes.NewReader(frame(MaxFrame+1, strings.Repeat("x", MaxFrame+1)))); err == nil || !strings.Contains(err.Error(), "a frame of") {
		t.Errorf("an oversize frame: %v", err)
	}
	// a body with a field the receiver does not know
	var buf bytes.Buffer
	Write(&buf, Hello, map[string]any{"version": "1", "shell": true})
	f, _ := Read(&buf)
	var hb HelloBody
	if err := f.Decode(&hb); err == nil {
		t.Error("an unknown field decoded")
	}
	// a frame over the maximum is not written
	if err := Write(&buf, Result, map[string]string{"output": strings.Repeat("x", MaxFrame)}); err == nil {
		t.Error("an oversize frame written")
	}
}

// A remote worker's result with the worker's whole cap of calls is a few MiB: one frame carries it.
func TestFrameCarriesAFullCallsResult(t *testing.T) {
	var buf bytes.Buffer
	body := RunResultBody{RunID: "r", Result: []byte(`{"detail":"` + strings.Repeat("d", 3<<20) + `"}`)}
	if err := Write(&buf, RunResult, body); err != nil {
		t.Fatal(err)
	}
	f, err := Read(&buf)
	var back RunResultBody
	if err != nil || f.Decode(&back) != nil || len(back.Result) != len(body.Result) {
		t.Fatalf("%+v %v", f.Type, err)
	}
	if err := Write(&buf, RunResult, RunResultBody{RunID: "r", Result: []byte(`"` + strings.Repeat("d", MaxFrame) + `"`)}); err == nil {
		t.Fatal("a frame over MaxFrame must be refused")
	}
}

// A notify frame from before answers decodes with none, and one with them round-trips; a reader of the older body
// refuses the new fields (the decoding is strict), which is why flotillad and its connector's agent upgrade together.
func TestNotifyBodyOldAndNew(t *testing.T) {
	old := Frame{Type: Notify, Body: []byte(`{"nonce":"n","case":"c","title":"t","plan":"p","plan_hash":"h","url":"u","approvable":true}`)}
	var n NotifyBody
	if err := old.Decode(&n); err != nil || n.Kind != "" || len(n.Answers) != 0 || n.Repeat || !n.Approvable {
		t.Fatalf("an old notify body: %+v %v", n, err)
	}
	var buf bytes.Buffer
	want := NotifyBody{Nonce: "n", Case: "c", Title: "t", Plan: "p", URL: "u", Kind: KindNotice, Repeat: true,
		Answers: []NotifyAnswer{{ID: "acknowledge", Label: "Acknowledge", Allowed: true}, {ID: "approve", Label: "Approve", Style: "primary", Why: "high risk"}}}
	if err := Write(&buf, Notify, want); err != nil {
		t.Fatal(err)
	}
	f, err := Read(&buf)
	var got NotifyBody
	if err != nil || f.Decode(&got) != nil || got.Kind != KindNotice || !got.Repeat || len(got.Answers) != 2 || got.Answers[1] != want.Answers[1] || got.Answers[0] != want.Answers[0] {
		t.Fatalf("a new notify body: %+v %v", got, err)
	}
	type before struct { // NotifyBody as C3 had it
		Nonce, Case, Title, Plan string
		PlanHash                 string `json:"plan_hash"`
		URL                      string `json:"url"`
		Approvable               bool   `json:"approvable"`
		Why                      string `json:"why,omitempty"`
	}
	if err := f.Decode(&before{}); err == nil {
		t.Fatal("a reader that does not know the new fields accepted them")
	}
}
