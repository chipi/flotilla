package api

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/chipi/flotilla/internal/hostca"
	"github.com/chipi/flotilla/internal/spec"
	"github.com/chipi/flotilla/internal/store"
)

func hostServer(t *testing.T) (*Server, *httptest.Server, *hostca.CA) {
	t.Helper()
	srv := newServer(t, func(*spec.Routine) {})
	root, err := hostca.NewRoot(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	inter, err := hostca.NewIntermediate(root, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	srv.HostCA, srv.HostRoot = inter, root.Cert
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return srv, ts, root
}

// enrol makes a key on "the host", a request for it, and enrols with tok; it returns the chain.
func enrol(t *testing.T, srv *Server, host, tok, csrName string) ([]*x509.Certificate, error) {
	t.Helper()
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	csr, err := hostca.HostRequest(key, csrName)
	if err != nil {
		t.Fatal(err)
	}
	b, err := srv.EnrolHost(context.Background(), host, tok, csr)
	if err != nil {
		return nil, err
	}
	var chain []*x509.Certificate
	for blk, rest := pem.Decode(b); blk != nil; blk, rest = pem.Decode(rest) {
		c, err := x509.ParseCertificate(blk.Bytes)
		if err != nil {
			t.Fatal(err)
		}
		chain = append(chain, c)
	}
	return chain, nil
}

func mint(t *testing.T, ts *httptest.Server, host string) string {
	t.Helper()
	c, out := call(t, ts, "POST", "/v1/hosts/"+host+"/enrolment", opTok, "")
	tok := fmt.Sprint(out["token"])
	if c != 201 || !strings.HasPrefix(tok, "fle_") || len(tok) != 68 {
		t.Fatalf("mint: %d %v", c, out)
	}
	return tok
}

func TestHostEnrolAuthenticateRevoke(t *testing.T) {
	ctx := context.Background()
	srv, ts, _ := hostServer(t)
	// only an operator mints and revokes
	if c, _ := call(t, ts, "POST", "/v1/hosts/mini/enrolment", agentTok, ""); c != 403 {
		t.Fatalf("an agent minting: %d", c)
	}
	if c, _ := call(t, ts, "POST", "/v1/hosts/Mini;x/enrolment", opTok, ""); c != 400 {
		t.Fatalf("a bad host name: %d", c)
	}
	// the functions check the role themselves (the UI calls them without the route's check)
	agent := Caller{Name: "claude-homelab", Role: RoleAgent}
	if _, _, err := srv.MintEnrolment(ctx, agent, "ui", "mini"); err == nil {
		t.Fatal("an agent minted through the function")
	}
	tok := mint(t, ts, "mini")
	// a request naming another host is refused, and does not spend the token
	if _, err := enrol(t, srv, "mini", tok, "dgx"); err == nil {
		t.Fatal("a request naming another host enrolled")
	}
	if _, err := enrol(t, srv, "dgx", tok, "dgx"); err == nil {
		t.Fatal("a token used for another host")
	}
	chain, err := enrol(t, srv, "mini", tok, "mini")
	if err != nil || len(chain) != 2 || chain[0].Subject.CommonName != "mini" || !chain[1].Equal(srv.HostCA.Cert) {
		t.Fatalf("enrol: %d certificates, %v", len(chain), err)
	}
	if _, err := enrol(t, srv, "mini", tok, "mini"); err == nil {
		t.Fatal("a token used twice")
	}
	if host, err := srv.AuthenticateHost(ctx, chain); err != nil || host != "mini" {
		t.Fatalf("authenticate: %q %v", host, err)
	}
	if err := srv.RevokeHost(ctx, agent, "ui", "mini"); err == nil {
		t.Fatal("an agent revoked through the function")
	}
	if _, err := srv.AuthenticateHost(ctx, chain); err != nil {
		t.Fatalf("after an agent's refused revoke: %v", err)
	}
	if c, out := call(t, ts, "GET", "/v1/hosts", agentTok, ""); c != 200 || !strings.Contains(fmt.Sprint(out["hosts"]), hostca.Fingerprint(chain[0])) {
		t.Fatalf("hosts: %d %v", c, out)
	}
	// another CA's certificate for the same name
	other, _, _ := hostServer(t)
	otherTok, _, err := other.MintEnrolment(ctx, Caller{Name: "operator", Role: RoleOperator}, "test", "mini")
	if err != nil {
		t.Fatal(err)
	}
	otherChain, err := enrol(t, other, "mini", otherTok, "mini")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.AuthenticateHost(ctx, otherChain); err == nil {
		t.Fatal("another CA's agent authenticated")
	}
	// a second enrolment replaces the certificate: the first stops working
	chain2, err := enrol(t, srv, "mini", mint(t, ts, "mini"), "mini")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.AuthenticateHost(ctx, chain); !errors.Is(err, store.ErrNotCurrent) {
		t.Fatalf("a replaced certificate: %v", err)
	}
	// revoke: refused at connect, with no access to the host
	if c, _ := call(t, ts, "POST", "/v1/hosts/mini/revoke", agentTok, ""); c != 403 {
		t.Fatalf("an agent revoking: %d", c)
	}
	if c, _ := call(t, ts, "POST", "/v1/hosts/nohost/revoke", opTok, ""); c != 404 {
		t.Fatalf("revoking an unknown host: %d", c)
	}
	if c, _ := call(t, ts, "POST", "/v1/hosts/mini/revoke", opTok, ""); c != 200 {
		t.Fatalf("revoke: %d", c)
	}
	if _, err := srv.AuthenticateHost(ctx, chain2); !errors.Is(err, store.ErrNotCurrent) {
		t.Fatalf("a revoked host authenticated: %v", err)
	}
}

func TestNoHostCA(t *testing.T) {
	srv := newServer(t, func(*spec.Routine) {})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	if c, _ := call(t, ts, "POST", "/v1/hosts/mini/enrolment", opTok, ""); c != 503 {
		t.Fatalf("minting without a CA: %d", c)
	}
	if _, err := srv.EnrolHost(context.Background(), "mini", "fle_x", nil); err == nil {
		t.Fatal("enrolling without a CA")
	}
	if _, err := srv.AuthenticateHost(context.Background(), nil); err == nil {
		t.Fatal("authenticating without a CA")
	}
}

// The agents pin the root so the intermediate can rotate (ADR-0006 §3): a certificate the old
// intermediate issued still authenticates after flotillad switches to a new one under the same root.
func TestIntermediateRotation(t *testing.T) {
	ctx := context.Background()
	srv, ts, root := hostServer(t)
	chain, err := enrol(t, srv, "mini", mint(t, ts, "mini"), "mini")
	if err != nil {
		t.Fatal(err)
	}
	next, err := hostca.NewIntermediate(root, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	srv.HostCA = next
	if host, err := srv.AuthenticateHost(ctx, chain); err != nil || host != "mini" {
		t.Fatalf("after rotating the intermediate: %q %v", host, err)
	}
}

// A certificate's serial is not a secret (flotillad logs it), so it cannot be what makes a certificate
// the host's current one: a retired or leaked intermediate under the same root, or the current one used
// outside flotillad, can issue a certificate for the host with that serial and another key. flotillad
// accepts exactly the certificate it issued and recorded.
func TestHostCertificateWithTheCurrentSerialRefused(t *testing.T) {
	ctx := context.Background()
	srv, ts, root := hostServer(t)
	chain, err := enrol(t, srv, "mini", mint(t, ts, "mini"), "mini")
	if err != nil {
		t.Fatal(err)
	}
	other, err := hostca.NewIntermediate(root, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	for name, ca := range map[string]*hostca.CA{"another intermediate": other, "the current intermediate": srv.HostCA} {
		der, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
			SerialNumber: chain[0].SerialNumber, Subject: chain[0].Subject,
			NotBefore: chain[0].NotBefore, NotAfter: chain[0].NotAfter,
			KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		}, ca.Cert, pub, ca.Key)
		if err != nil {
			t.Fatal(err)
		}
		leaf, err := x509.ParseCertificate(der)
		if err != nil {
			t.Fatal(err)
		}
		if host, err := srv.AuthenticateHost(ctx, []*x509.Certificate{leaf, ca.Cert}); !errors.Is(err, store.ErrNotCurrent) {
			t.Fatalf("%s: a certificate with mini's serial and another key: %q %v", name, host, err)
		}
	}
	if host, err := srv.AuthenticateHost(ctx, chain); err != nil || host != "mini" {
		t.Fatalf("the issued certificate: %q %v", host, err)
	}
}

// flotillad's side of an audit: what it sent a host and what came back, and the head it last heard.
func TestHostExecutionsRoute(t *testing.T) {
	ctx := context.Background()
	srv, ts, _ := hostServer(t)
	if _, err := enrol(t, srv, "mini", mint(t, ts, "mini"), "mini"); err != nil {
		t.Fatal(err)
	}
	if err := srv.Store.RecordHostContact(ctx, store.HostContact{Host: "mini", Connected: true, ChainHead: "head-abc",
		ConnectedAt: time.Now(), LastContact: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := srv.Store.StartHostExecution(ctx, store.HostExecution{ID: "case-1-p1", CaseID: "case-1", PlanID: 1, Host: "mini",
		Action: "host.true", Approved: "user:ada", Envelope: []byte("x")}); err != nil {
		t.Fatal(err)
	}
	if err := srv.Store.EndHostExecution(ctx, "case-1-p1", "done", 0, "sha", "", "head-abc"); err != nil {
		t.Fatal(err)
	}
	c, out := call(t, ts, "GET", "/v1/hosts/mini/executions", agentTok, "")
	xs, _ := out["executions"].([]any)
	if c != 200 || out["chain_head"] != "head-abc" || len(xs) != 1 || xs[0].(map[string]any)["status"] != "done" {
		t.Fatalf("executions: %d %v", c, out)
	}
	if _, has := xs[0].(map[string]any)["envelope"]; has {
		t.Fatal("the envelope's bytes are in the listing")
	}
	if c, _ := call(t, ts, "GET", "/v1/hosts/nohost/executions", agentTok, ""); c != 404 {
		t.Fatalf("an unknown host: %d", c)
	}
}
