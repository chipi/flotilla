package hostlink

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"log/slog"
	"math/big"
	"net"
	"testing"
	"time"

	"github.com/chipi/flotilla/internal/hostwire"
)

// tlsPair is a connected pair of TLS connections over a pipe: flotillad's side (what conn writes to) and the
// worker's link side (what the test reads).
func tlsPair(t *testing.T) (srv, cli *tls.Conn) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "x"}, NotBefore: time.Now().Add(-time.Hour),
		NotAfter: time.Now().Add(time.Hour), DNSNames: []string{"x"}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	pool := x509.NewCertPool()
	leaf, _ := x509.ParseCertificate(der)
	pool.AddCert(leaf)
	a, b := net.Pipe()
	srv = tls.Server(a, &tls.Config{Certificates: []tls.Certificate{cert}})
	cli = tls.Client(b, &tls.Config{RootCAs: pool, ServerName: "x"})
	errs := make(chan error, 2)
	go func() { errs <- srv.Handshake() }()
	go func() { errs <- cli.Handshake() }()
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { srv.Close(); cli.Close() })
	return srv, cli
}

func readCancel(t *testing.T, cli *tls.Conn) hostwire.CancelBody {
	t.Helper()
	cli.SetReadDeadline(time.Now().Add(5 * time.Second))
	f, err := hostwire.Read(cli)
	if err != nil {
		t.Fatal(err)
	}
	var cb hostwire.CancelBody
	if f.Type != hostwire.Cancel || f.Decode(&cb) != nil {
		t.Fatalf("frame %q", f.Type)
	}
	return cb
}

// A run the previous flotillad left in flight at a remote worker is cancelled with the stop frame, though this
// server never saw the run: at once when the link is connected, else when it connects.
func TestCancelOrphan(t *testing.T) {
	var buf bytes.Buffer
	s := &Server{Log: slog.New(slog.NewTextHandler(&buf, nil))}
	queued, err := s.CancelOrphan("remote-w", "run-1")
	if !queued || err != nil {
		t.Fatalf("no link yet: queued %v, %v", queued, err)
	}
	if _, err := s.CancelOrphan("other-w", "run-9"); err != nil {
		t.Fatal(err)
	}
	if got := s.orphans["remote-w"]; len(got) != 1 || got[0] != "run-1" {
		t.Fatalf("kept: %v", s.orphans)
	}

	srv, cli := tlsPair(t)
	c := &conn{host: "w-remote-w-node", tc: srv, closed: make(chan struct{})}
	go s.cancelOrphans("remote-w", c, s.orphans["remote-w"])
	if cb := readCancel(t, cli); cb.RunID != "run-1" || !cb.Stop {
		t.Fatalf("the frame on connect: %+v", cb)
	}

	s.wconns["remote-w"] = c
	go func() {
		if q, err := s.CancelOrphan("remote-w", "run-2"); q || err != nil {
			t.Errorf("connected: queued %v, %v", q, err)
		}
	}()
	if cb := readCancel(t, cli); cb.RunID != "run-2" || !cb.Stop {
		t.Fatalf("the frame when connected: %+v", cb)
	}
}
