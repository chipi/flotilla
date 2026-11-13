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
