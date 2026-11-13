// Package envelope is the signed execution flotillad sends a host agent (ADR-0006 §4, decided 3): an
// execution signed with Ed25519 over the exact bytes sent, in a small envelope of our own, shared by
// flotillad and the agent. The agent verifies the signature over the bytes it received and only then
// decodes them; neither side canonicalizes.
//
// On the wire, one JSON object: {"key": "<key id>", "sig": "<base64>", "payload": "<base64>"}. The
// signature is over the payload's bytes as decoded from base64, which are the execution's JSON exactly
// as the signer wrote it.
//
// Security T13: a forged, altered, replayed-late, future-dated or misdirected execution is refused here.
// A seen id is the agent store's check (an identical resend returns the recorded result).
package envelope

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"
)

// MaxValidity is the longest an execution may be valid: flotillad signs after the approval, so the
// window is short (ADR-0006 §4). An envelope valid for longer is refused, whoever signed it.
const MaxValidity = 10 * time.Minute

// MaxFuture is how far in the future an execution's signing time may be (the two clocks' skew).
const MaxFuture = 10 * time.Minute

// MaxBytes bounds an envelope, read before anything is decoded.
const MaxBytes = 64 * 1024

var (
	ErrMalformed = errors.New("envelope: malformed")
	ErrKey       = errors.New("envelope: not signed by an accepted key")
	ErrSignature = errors.New("envelope: bad signature")
	ErrExpired   = errors.New("envelope: expired")
	ErrFuture    = errors.New("envelope: signed in the future")
	ErrValidity  = errors.New("envelope: valid for longer than allowed")
	ErrHost      = errors.New("envelope: for another host")
)

// Execution is what a host agent is asked to run.
type Execution struct {
	ID        string            `json:"id"`
	CaseID    string            `json:"case_id"`
	Host      string            `json:"host"`
	Action    string            `json:"action"`
	Params    map[string]string `json:"params"`
	PlanHash  string            `json:"plan_hash"`
	Approved  string            `json:"approved"` // "auto", or the operator who approved it ("user:<name>")
	SignedAt  time.Time         `json:"signed_at"`
	ExpiresAt time.Time         `json:"expires_at"`
}

type wire struct {
	Key     string `json:"key"`
	Sig     string `json:"sig"`
	Payload string `json:"payload"`
}

var keyIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)

// Sign writes e signed with priv under keyID. It refuses an execution that Verify would refuse for its
// own shape (no id, host or action; valid for longer than MaxValidity).
func Sign(priv ed25519.PrivateKey, keyID string, e Execution) ([]byte, error) {
	if !keyIDRe.MatchString(keyID) {
		return nil, fmt.Errorf("envelope: key id %q", keyID)
	}
	if err := shape(e); err != nil {
		return nil, err
	}
	payload, err := json.Marshal(e)
	if err != nil {
		return nil, err
	}
	sig := ed25519.Sign(priv, payload)
	return json.Marshal(wire{Key: keyID, Sig: base64.StdEncoding.EncodeToString(sig), Payload: base64.StdEncoding.EncodeToString(payload)})
}

func shape(e Execution) error {
	if e.ID == "" || e.Host == "" || e.Action == "" || e.Approved == "" {
		return fmt.Errorf("%w: id, host, action and approved are required", ErrMalformed)
	}
	if e.SignedAt.IsZero() || !e.ExpiresAt.After(e.SignedAt) {
		return fmt.Errorf("%w: expires_at must follow signed_at", ErrMalformed)
	}
	if e.ExpiresAt.Sub(e.SignedAt) > MaxValidity {
		return ErrValidity
	}
	return nil
}

// Verify checks raw, the bytes as received, for the agent of host at now: the signature first, over the
// payload's exact bytes, with one of keys (by key id: two during a rotation, ADR-0006 §11), and only then
// the decoded execution: its shape, its host, its time. It returns the execution and the payload's bytes
// (what the agent records, to tell an identical resend from a forgery).
func Verify(raw []byte, keys map[string]ed25519.PublicKey, host string, now time.Time) (Execution, []byte, error) {
	var e Execution
	if len(raw) > MaxBytes {
		return e, nil, fmt.Errorf("%w: over %d bytes", ErrMalformed, MaxBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var w wire
	if err := dec.Decode(&w); err != nil {
		return e, nil, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	if dec.More() {
		return e, nil, fmt.Errorf("%w: trailing data", ErrMalformed)
	}
	pub, ok := keys[w.Key]
	if !ok || len(pub) != ed25519.PublicKeySize {
		return e, nil, ErrKey
	}
	sig, err := base64.StdEncoding.DecodeString(w.Sig)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return e, nil, fmt.Errorf("%w: signature encoding", ErrMalformed)
	}
	payload, err := base64.StdEncoding.DecodeString(w.Payload)
	if err != nil {
		return e, nil, fmt.Errorf("%w: payload encoding", ErrMalformed)
	}
	if !ed25519.Verify(pub, payload, sig) {
		return e, nil, ErrSignature
	}
	// Only now, signed bytes: decode them strictly.
	pdec := json.NewDecoder(bytes.NewReader(payload))
	pdec.DisallowUnknownFields()
	if err := pdec.Decode(&e); err != nil {
		return Execution{}, nil, fmt.Errorf("%w: payload: %v", ErrMalformed, err)
	}
	if pdec.More() {
		return Execution{}, nil, fmt.Errorf("%w: payload trailing data", ErrMalformed)
	}
	if err := shape(e); err != nil {
		return Execution{}, nil, err
	}
	switch {
	case e.Host != host:
		return Execution{}, nil, fmt.Errorf("%w: %q, this is %q", ErrHost, e.Host, host)
	case e.SignedAt.After(now.Add(MaxFuture)):
		return Execution{}, nil, ErrFuture
	case !now.Before(e.ExpiresAt):
		return Execution{}, nil, ErrExpired
	}
	return e, payload, nil
}

// PeekID reads an envelope's execution id without verifying anything, for one use only: labelling the
// refusal of an envelope that did not verify, so that flotillad can match it to what it sent. It is never
// trusted for anything else; "" when there is none to read.
func PeekID(raw []byte) string {
	if len(raw) > MaxBytes {
		return ""
	}
	var w wire
	if json.Unmarshal(raw, &w) != nil {
		return ""
	}
	payload, err := base64.StdEncoding.DecodeString(w.Payload)
	if err != nil {
		return ""
	}
	var e struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(payload, &e) != nil || len(e.ID) > 128 {
		return ""
	}
	return e.ID
}
