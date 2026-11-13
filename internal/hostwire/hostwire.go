// Package hostwire is the frame format between a host agent and flotillad (ADR-0006 §2): over the
// agent's one outbound TLS connection, each frame is a 4-byte big-endian length and that many bytes of
// JSON, {"type": ..., "body": ...}. No dependency (ADR-0006 named a websocket library only if one is
// used).
package hostwire

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

// MaxFrame bounds a frame: an execution's envelope, or a result with its 16 KiB of output, fits many
// times over; a peer announcing more is cut off before anything is read.
const MaxFrame = 1 << 20

// Frame types.
const (
	// agent -> flotillad
	Enrol     = "enrol"     // {token, host, csr}: on a connection without a client certificate, the only frame
	Hello     = "hello"     // {version, clock, chain_head}: first on an authenticated connection
	Heartbeat = "heartbeat" // {clock, chain_head}
	Result    = "result"    // hostexec.Result
	Renew     = "renew"     // {csr}: a request for the agent's next certificate
	// flotillad -> agent
	Welcome  = "welcome"  // {clock}
	Enrolled = "enrolled" // {chain}: the certificate and the intermediate, PEM
	Renewed  = "renewed"  // {chain}
	Execute  = "execute"  // {envelope}: the signed execution, its exact bytes
	Refusal  = "refusal"  // {detail}: and the connection closes
)

type Frame struct {
	Type string          `json:"type"`
	Body json.RawMessage `json:"body,omitempty"`
}

// Write sends one frame of type t with body v.
func Write(w io.Writer, t string, v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b, err := json.Marshal(Frame{Type: t, Body: body})
	if err != nil {
		return err
	}
	if len(b) > MaxFrame {
		return fmt.Errorf("a %s frame of %d bytes, over %d", t, len(b), MaxFrame)
	}
	buf := make([]byte, 4+len(b))
	binary.BigEndian.PutUint32(buf, uint32(len(b)))
	copy(buf[4:], b)
	_, err = w.Write(buf)
	return err
}

// Read reads one frame.
func Read(r io.Reader) (Frame, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return Frame{}, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n == 0 || n > MaxFrame {
		return Frame{}, fmt.Errorf("a frame of %d bytes", n)
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return Frame{}, err
	}
	var f Frame
	if err := json.Unmarshal(b, &f); err != nil {
		return Frame{}, fmt.Errorf("a frame: %v", err)
	}
	if f.Type == "" {
		return Frame{}, errors.New("a frame with no type")
	}
	return f, nil
}

// Decode reads f's body into v, strictly.
func (f Frame) Decode(v any) error {
	dec := json.NewDecoder(bytes.NewReader(f.Body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("a %s frame: %v", f.Type, err)
	}
	return nil
}

// Bodies. A result is hostexec.Result.

type EnrolBody struct {
	Host  string `json:"host"`
	Token string `json:"token"`
	CSR   []byte `json:"csr"` // DER
}

type HelloBody struct {
	Version   string    `json:"version"`
	Clock     time.Time `json:"clock"`
	ChainHead string    `json:"chain_head"`
}

type HeartbeatBody struct {
	Clock     time.Time `json:"clock"`
	ChainHead string    `json:"chain_head"`
}

type WelcomeBody struct {
	Clock time.Time `json:"clock"`
}

type RenewBody struct {
	CSR []byte `json:"csr"` // DER
}

type ChainBody struct {
	Chain string `json:"chain"` // PEM, the certificate then the intermediate
}

type ExecuteBody struct {
	Envelope []byte `json:"envelope"` // the signed execution's exact bytes
}

type RefusalBody struct {
	Detail string `json:"detail"`
}
