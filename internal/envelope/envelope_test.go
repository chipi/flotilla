package envelope

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)

func keypair(t testing.TB, seed byte) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	priv := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{seed}, ed25519.SeedSize))
	return priv.Public().(ed25519.PublicKey), priv
}

func exe() Execution {
	return Execution{ID: "exec-1", CaseID: "case-1", Host: "mini", Action: "backup.rerun_observability",
		Params: map[string]string{"host": "mini"}, PlanHash: "abc", Approved: "user:ada", SignedAt: t0, ExpiresAt: t0.Add(10 * time.Minute)}
}

func signed(t testing.TB, priv ed25519.PrivateKey, e Execution) []byte {
	t.Helper()
	raw, err := Sign(priv, "k1", e)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestVerify(t *testing.T) {
	pub, priv := keypair(t, 1)
	other, otherPriv := keypair(t, 2)
	keys := map[string]ed25519.PublicKey{"k1": pub}
	good := signed(t, priv, exe())

	e, payload, err := Verify(good, keys, "mini", t0.Add(time.Minute))
	if err != nil || e.ID != "exec-1" || e.Params["host"] != "mini" || len(payload) == 0 {
		t.Fatalf("a good envelope: %+v %v", e, err)
	}
	// Two keys during a rotation (ADR-0006 §11): either verifies.
	if _, _, err := Verify(signed(t, otherPriv, exe()), map[string]ed25519.PublicKey{"k1": other, "k0": pub}, "mini", t0); err != nil {
		t.Fatalf("the new key during a rotation: %v", err)
	}

	var w wire
	_ = json.Unmarshal(good, &w)
	payloadBytes, _ := base64.StdEncoding.DecodeString(w.Payload)
	reencode := func(mut func(*wire)) []byte {
		c := w
		mut(&c)
		b, _ := json.Marshal(c)
		return b
	}
	flipped := append([]byte{}, payloadBytes...)
	flipped[len(flipped)/2] ^= 1
	sig, _ := base64.StdEncoding.DecodeString(w.Sig)
	badSig := append([]byte{}, sig...)
	badSig[0] ^= 1

	for name, c := range map[string]struct {
		raw  []byte
		keys map[string]ed25519.PublicKey
		host string
		now  time.Time
		want error
	}{
		"a changed payload byte": {reencode(func(x *wire) { x.Payload = base64.StdEncoding.EncodeToString(flipped) }), keys, "mini", t0, ErrSignature},
		"a changed signature":    {reencode(func(x *wire) { x.Sig = base64.StdEncoding.EncodeToString(badSig) }), keys, "mini", t0, ErrSignature},
		"another key":            {signed(t, otherPriv, exe()), keys, "mini", t0, ErrSignature},
		"an unknown key id":      {reencode(func(x *wire) { x.Key = "k9" }), keys, "mini", t0, ErrKey},
		"no keys at all":         {good, map[string]ed25519.PublicKey{}, "mini", t0, ErrKey},
		"another host":           {good, keys, "dgx", t0, ErrHost},
		"expired":                {good, keys, "mini", t0.Add(10 * time.Minute), ErrExpired},
		"signed in the future":   {good, keys, "mini", t0.Add(-11 * time.Minute), ErrFuture},
		"not an envelope":        {[]byte(`{"payload": 1}`), keys, "mini", t0, ErrMalformed},
		"an unknown outer field": {[]byte(strings.Replace(string(good), `"key"`, `"run":"now","key"`, 1)), keys, "mini", t0, ErrMalformed},
		"trailing data":          {append(append([]byte{}, good...), []byte(` {}`)...), keys, "mini", t0, ErrMalformed},
		"too big":                {append(bytes.Repeat([]byte(" "), MaxBytes), good...), keys, "mini", t0, ErrMalformed},
		"a short signature":      {reencode(func(x *wire) { x.Sig = base64.StdEncoding.EncodeToString(sig[:10]) }), keys, "mini", t0, ErrMalformed},
	} {
		if _, _, err := Verify(c.raw, c.keys, c.host, c.now); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", name, err, c.want)
		}
	}
}

// A signed payload is checked after the signature: a signer that writes an unknown field, a long
// validity or a missing id is refused even with a good signature.
func TestSignedButBadPayload(t *testing.T) {
	pub, priv := keypair(t, 1)
	keys := map[string]ed25519.PublicKey{"k1": pub}
	raw := func(payload string) []byte {
		b, _ := json.Marshal(wire{Key: "k1", Sig: base64.StdEncoding.EncodeToString(ed25519.Sign(priv, []byte(payload))),
			Payload: base64.StdEncoding.EncodeToString([]byte(payload))})
		return b
	}
	good, _ := json.Marshal(exe())
	long := exe()
	long.ExpiresAt = t0.Add(11 * time.Minute)
	longB, _ := json.Marshal(long)
	noID := exe()
	noID.ID = ""
	noIDB, _ := json.Marshal(noID)
	for name, c := range map[string]struct {
		payload string
		want    error
	}{
		"an unknown field":            {strings.Replace(string(good), `"id"`, `"shell":"rm -rf /","id"`, 1), ErrMalformed},
		"valid for longer than 10min": {string(longB), ErrValidity},
		"no id":                       {string(noIDB), ErrMalformed},
		"two objects":                 {string(good) + string(good), ErrMalformed},
	} {
		if _, _, err := Verify(raw(c.payload), keys, "mini", t0); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", name, err, c.want)
		}
	}
	if _, err := Sign(priv, "k1", long); !errors.Is(err, ErrValidity) {
		t.Fatalf("Sign wrote a long-lived execution: %v", err)
	}
	if _, err := Sign(priv, "Key One", exe()); err == nil {
		t.Fatal("Sign took a bad key id")
	}
}

// The payload the agent records is the exact signed bytes: an identical resend has the same bytes, a
// re-signed variant has different ones (the agent store's forgery check).
func TestPayloadIsTheSignedBytes(t *testing.T) {
	pub, priv := keypair(t, 1)
	keys := map[string]ed25519.PublicKey{"k1": pub}
	a := signed(t, priv, exe())
	_, pa, _ := Verify(a, keys, "mini", t0)
	_, pa2, _ := Verify(a, keys, "mini", t0)
	v := exe()
	v.Params = map[string]string{"host": "mini", "force": "yes"}
	_, pv, _ := Verify(signed(t, priv, v), keys, "mini", t0)
	if !bytes.Equal(pa, pa2) || bytes.Equal(pa, pv) {
		t.Fatal("the recorded payload does not tell a resend from a variant")
	}
}

// Whatever arrives, Verify returns an error or a good execution: it never panics.
func FuzzVerify(f *testing.F) {
	pub, priv := keypair(f, 1)
	keys := map[string]ed25519.PublicKey{"k1": pub}
	f.Add(signed(f, priv, exe()))
	f.Add([]byte(`{"key":"k1","sig":"","payload":""}`))
	f.Add([]byte(`{}`))
	f.Fuzz(func(t *testing.T, raw []byte) {
		e, _, err := Verify(raw, keys, "mini", t0)
		if err == nil && (e.Host != "mini" || e.ID == "") {
			t.Fatalf("accepted %+v", e)
		}
	})
}

// PeekID reads the id of an envelope that did not verify (to label its refusal), and nothing else.
func TestPeekID(t *testing.T) {
	_, priv := keypair(t, 1)
	if id := PeekID(signed(t, priv, exe())); id != "exec-1" {
		t.Fatalf("a good envelope: %q", id)
	}
	long := exe()
	long.ID = strings.Repeat("x", 129)
	for name, raw := range map[string][]byte{
		"not JSON":       []byte("garbage"),
		"a bad payload":  []byte(`{"key":"k1","sig":"","payload":"!!"}`),
		"no id":          []byte(`{"key":"k1","sig":"","payload":"` + base64.StdEncoding.EncodeToString([]byte(`{"host":"mini"}`)) + `"}`),
		"an id too long": signed(t, priv, long),
		"too big": []byte(`{"key":"k1","sig":"","payload":"` + base64.StdEncoding.EncodeToString(
			[]byte(`{"id":"exec-1","pad":"`+strings.Repeat("x", MaxBytes)+`"}`)) + `"}`),
	} {
		if id := PeekID(raw); id != "" {
			t.Errorf("%s: %q", name, id)
		}
	}
}
