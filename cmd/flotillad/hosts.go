package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/chipi/flotilla/internal/hostalert"
	"github.com/chipi/flotilla/internal/hostca"
	"github.com/chipi/flotilla/internal/hostlink"
	"github.com/chipi/flotilla/internal/store"
)

// HostsConfig is the host agents' endpoint (ADR-0006 §2, decided 1): its own port with mutual TLS,
// reached however the installation chooses (this homelab: its tailnet, a TCP passthrough).
type HostsConfig struct {
	Listen string `yaml:"listen"`
	// intermediate.crt, intermediate.key and root.crt, from `flotilla ca intermediate` (the root's key
	// never comes here)
	CADir string `yaml:"ca_dir"`
	// the names agents dial flotillad by; its server certificate carries them
	ServerNames []string `yaml:"server_names"`
	// the agents' heartbeat interval (their -heartbeat); silent for three, a connection is dropped.
	// Unset: 30s.
	Heartbeat time.Duration `yaml:"heartbeat"`
}

func (h HostsConfig) validate() error {
	if h.Listen == "" {
		return nil
	}
	if h.CADir == "" || len(h.ServerNames) == 0 {
		return errors.New("hosts: listen needs ca_dir and server_names")
	}
	if h.Heartbeat < 0 {
		return errors.New("hosts: heartbeat cannot be negative")
	}
	return nil
}

// loadHostCA reads the intermediate and the root, and refuses an intermediate the root did not sign.
func loadHostCA(dir string) (*hostca.CA, *x509.Certificate, error) {
	read := func(name string) ([]byte, error) { return os.ReadFile(filepath.Join(dir, name)) }
	certPEM, err := read("intermediate.crt")
	if err != nil {
		return nil, nil, err
	}
	keyPEM, err := read("intermediate.key")
	if err != nil {
		return nil, nil, err
	}
	rootPEM, err := read("root.crt")
	if err != nil {
		return nil, nil, err
	}
	inter, err := hostca.Load(certPEM, keyPEM)
	if err != nil {
		return nil, nil, fmt.Errorf("the intermediate: %w", err)
	}
	root, err := hostca.ParseCert(rootPEM)
	if err != nil {
		return nil, nil, fmt.Errorf("the root: %w", err)
	}
	if err := inter.Cert.CheckSignatureFrom(root); err != nil {
		return nil, nil, fmt.Errorf("the intermediate is not the root's: %w", err)
	}
	return inter, root, nil
}

// watchHosts logs a host alert once when it starts (an ERROR, which reaches GlitchTip) and once when it
// clears (ADR-0006 §10).
func watchHosts(ctx context.Context, st *store.Store, w *hostalert.Watcher, now time.Time, log *slog.Logger) {
	hs, err := st.Hosts(ctx)
	if err != nil {
		log.Error("host alerts: the store", "err", err)
		return
	}
	started, cleared := w.Update(hostalert.Check(hs, now, hostalert.Defaults))
	for _, a := range started {
		log.Error("host alert", "host", a.Host, "kind", a.Kind, "detail", a.Detail)
	}
	for _, a := range cleared {
		log.Info("host alert cleared", "host", a.Host, "kind", a.Kind)
	}
}

// recordContact keeps what the endpoint heard from a host in the store.
func recordContact(st *store.Store, log *slog.Logger) func(hostlink.HostState) {
	return func(h hostlink.HostState) {
		if err := st.RecordHostContact(context.Background(), store.HostContact{Host: h.Host, Connected: h.Connected, Version: h.Version,
			ConnectedAt: h.ConnectedAt, LastContact: h.LastContact, ClockOffset: h.ClockOffset, ChainHead: h.ChainHead}); err != nil {
			log.Error("host contact: the store", "host", h.Host, "err", err)
		}
	}
}

// serverCert is flotillad's certificate for the endpoint: a key made in memory at start, issued by the
// intermediate, and issued again when less than renewServer is left (flotillad may run longer than one
// certificate lasts).
type serverCert struct {
	ca    *hostca.CA
	names []string
	now   func() time.Time

	mu  sync.Mutex
	cur *tls.Certificate
}

const renewServer = 30 * 24 * time.Hour

func (s *serverCert) get(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur != nil && s.cur.Leaf.NotAfter.Sub(s.now()) > renewServer {
		return s.cur, nil
	}
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	c, err := s.ca.IssueServer(pub, s.names, s.now())
	if err != nil {
		return nil, err
	}
	s.cur = &tls.Certificate{Certificate: [][]byte{c.Raw, s.ca.Cert.Raw}, PrivateKey: key, Leaf: c}
	return s.cur, nil
}
