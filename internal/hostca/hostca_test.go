package hostca

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

var now = time.Date(2026, 11, 13, 21, 0, 0, 0, time.UTC)

type pki struct {
	root, inter *CA
	hostKey     ed25519.PrivateKey
	host        *x509.Certificate
}

func newPKI(t *testing.T, at ...time.Time) pki {
	t.Helper()
	when := now
	if len(at) > 0 {
		when = at[0]
	}
	root, err := NewRoot(when)
	if err != nil {
		t.Fatal(err)
	}
	inter, err := NewIntermediate(root, when)
	if err != nil {
		t.Fatal(err)
	}
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	csr, err := HostRequest(key, "mini")
	if err != nil {
		t.Fatal(err)
	}
	host, err := inter.IssueHost(csr, "mini", when)
	if err != nil {
		t.Fatal(err)
	}
	return pki{root, inter, key, host}
}

func TestChain(t *testing.T) {
	p := newPKI(t)
	name, serial, err := VerifyHost([]*x509.Certificate{p.host, p.inter.Cert}, p.root.Cert, now)
	if err != nil || name != "mini" || serial != p.host.SerialNumber.Text(16) {
		t.Fatalf("verify: %q %q %v", name, serial, err)
	}
	if !p.host.NotAfter.Equal(now.Add(HostValidity)) || p.inter.Cert.MaxPathLen != 0 || !p.inter.Cert.MaxPathLenZero {
		t.Fatalf("validity %s, path len %d", p.host.NotAfter, p.inter.Cert.MaxPathLen)
	}
	// A clock a little behind still accepts the certificate; one past its 30 days does not.
	if _, _, err := VerifyHost([]*x509.Certificate{p.host, p.inter.Cert}, p.root.Cert, now.Add(-30*time.Second)); err != nil {
		t.Errorf("30 s behind: %v", err)
	}
	if _, _, err := VerifyHost([]*x509.Certificate{p.host, p.inter.Cert}, p.root.Cert, now.Add(HostValidity+time.Second)); err == nil {
		t.Error("an expired certificate verified")
	}
}

func TestRefusals(t *testing.T) {
	p := newPKI(t)
	other := newPKI(t)
	csr, _ := HostRequest(p.hostKey, "mini")
	ec, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	ecCSR, _ := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: "mini"}}, ec)
	tampered := append([]byte(nil), csr...)
	tampered[len(tampered)-1] ^= 1 // the signature's last byte
	for name, c := range map[string]struct {
		csr  []byte
		host string
	}{
		"another host's request":   {csr, "dgx"},
		"a bad host name":          {csr, "Mini;rm"},
		"a request not Ed25519":    {ecCSR, "mini"},
		"a tampered request":       {tampered, "mini"},
		"not a request at all":     {[]byte("garbage"), "mini"},
		"a request for a bad name": {must(HostRequest(p.hostKey, "a b")), "a b"},
	} {
		if _, err := p.inter.IssueHost(c.csr, c.host, now); err == nil {
			t.Errorf("%s: issued", name)
		}
	}
	srvPub, _, _ := ed25519.GenerateKey(rand.Reader)
	server, _ := p.inter.IssueServer(srvPub, []string{"flotilla"}, now)
	badName, err := issue(&x509.Certificate{Subject: pkix.Name{CommonName: "a b"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}, p.inter, srvPub, p.inter.Key)
	if err != nil {
		t.Fatal(err)
	}
	for name, chain := range map[string][]*x509.Certificate{
		"another root's host":              {other.host, other.inter.Cert},
		"another intermediate":             {p.host, other.inter.Cert},
		"a server certificate as a host":   {server, p.inter.Cert},
		"the intermediate itself":          {p.inter.Cert},
		"no certificate":                   nil,
		"a bad name in a valid chain":      {badName, p.inter.Cert},
		"the leaf without its issuer":      {p.host},
		"a host signed by a non-CA (leaf)": {mustIssue(t, p, &CA{Cert: p.host, Key: p.hostKey}), p.host, p.inter.Cert},
	} {
		if _, _, err := VerifyHost(chain, p.root.Cert, now); err == nil {
			t.Errorf("%s: verified", name)
		}
	}
	if _, err := p.inter.IssueServer(srvPub, nil, now); err == nil {
		t.Error("a server certificate with no name")
	}
	// An intermediate outliving its root is refused.
	if _, err := NewIntermediate(p.root, now.Add(RootValidity-IntermediateValidity+time.Hour)); err == nil {
		t.Error("an intermediate past its root's expiry")
	}
}

func must(b []byte, err error) []byte {
	if err != nil {
		panic(err)
	}
	return b
}

// mustIssue has a host's own (non-CA) certificate sign another host certificate.
func mustIssue(t *testing.T, p pki, fake *CA) *x509.Certificate {
	t.Helper()
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	c, err := fake.IssueHost(must(HostRequest(key, "dgx")), "dgx", now)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestPEMAndLoad(t *testing.T) {
	p := newPKI(t)
	keyPEM, err := KeyPEM(p.inter.Key)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := Load(CertPEM(p.inter.Cert), keyPEM)
	if err != nil || !ca.Cert.Equal(p.inter.Cert) || !ca.Key.Equal(p.inter.Key) {
		t.Fatalf("load: %v", err)
	}
	otherKey, _ := KeyPEM(p.root.Key)
	hostKey, _ := KeyPEM(p.hostKey)
	for name, c := range map[string][2][]byte{
		"another CA's key":     {CertPEM(p.inter.Cert), otherKey},
		"a host's certificate": {CertPEM(p.host), hostKey},
		"a key as the cert":    {keyPEM, keyPEM},
		"a certificate as key": {CertPEM(p.inter.Cert), CertPEM(p.inter.Cert)},
		"garbage":              {[]byte("x"), []byte("y")},
	} {
		if _, err := Load(c[0], c[1]); err == nil {
			t.Errorf("%s: loaded", name)
		}
	}
}

// A real handshake: flotillad's endpoint requires a client certificate and checks it with VerifyHost;
// the agent pins the root and checks flotillad's name.
func TestTLS(t *testing.T) {
	p := newPKI(t, time.Now()) // TLS checks validity against the real clock
	srvPub, srvKey, _ := ed25519.GenerateKey(rand.Reader)
	srvCert, err := p.inter.IssueServer(srvPub, []string{"flotilla"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	// certificates for the real clock (TLS checks time.Now)
	_, hk, _ := ed25519.GenerateKey(rand.Reader)
	hostCert, err := p.inter.IssueHost(must(HostRequest(hk, "mini")), "mini", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(p.root.Cert)
	got := make(chan string, 1)
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{{Certificate: [][]byte{srvCert.Raw, p.inter.Cert.Raw}, PrivateKey: srvKey, Leaf: srvCert}},
		ClientAuth:   tls.RequireAnyClientCert,
		VerifyConnection: func(cs tls.ConnectionState) error {
			name, _, err := VerifyHost(cs.PeerCertificates, p.root.Cert, time.Now())
			got <- name
			return err
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { c.(*tls.Conn).Handshake(); io.Copy(io.Discard, c); c.Close() }()
		}
	}()
	dial := func(cert tls.Certificate, serverName string) error {
		c, err := tls.Dial("tcp", ln.Addr().String(), &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots,
			ServerName: serverName, Certificates: []tls.Certificate{cert}})
		if err != nil {
			return err
		}
		defer c.Close()
		// TLS 1.3: the server's verdict on the client arrives with the first read
		c.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, err = c.Read(make([]byte, 1))
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			return nil
		}
		return err
	}
	good := tls.Certificate{Certificate: [][]byte{hostCert.Raw, p.inter.Cert.Raw}, PrivateKey: hk}
	if err := dial(good, "flotilla"); err != nil || <-got != "mini" {
		t.Fatalf("the agent's connection: %v", err)
	}
	other := newPKI(t, time.Now())
	_, ok, _ := ed25519.GenerateKey(rand.Reader)
	otherCert, _ := other.inter.IssueHost(must(HostRequest(ok, "mini")), "mini", time.Now())
	if err := dial(tls.Certificate{Certificate: [][]byte{otherCert.Raw, other.inter.Cert.Raw}, PrivateKey: ok}, "flotilla"); err == nil {
		t.Error("another CA's agent connected")
	} else {
		<-got
	}
	if err := dial(good, "not-flotilla"); err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Errorf("the agent accepted a server under another name: %v", err)
	}
}
