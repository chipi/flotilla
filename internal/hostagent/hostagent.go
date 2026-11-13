// Package hostagent is the host agent's side of its connection to flotillad (ADR-0006 §2 and §3): it
// enrols once, then dials out and keeps one TLS connection, pinned to the root CA and flotillad's name,
// with its own certificate. It sends a heartbeat (its clock and its audit log's head), runs each
// execution flotillad sends through its executor (which decides for itself: internal/hostexec), sends
// the result back, renews its certificate over the connection before it expires, and reconnects with
// backoff when the connection drops.
package hostagent

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"sync"
	"time"

	"github.com/chipi/flotilla/internal/hostca"
	"github.com/chipi/flotilla/internal/hostexec"
	"github.com/chipi/flotilla/internal/hostwire"
)

// errRenewed ends a session after a renewal: flotillad closes it, and the agent reconnects at once
// with its new certificate.
var errRenewed = errors.New("renewed")

// RenewBefore: the agent asks for its next certificate when its current one has less than this left.
const RenewBefore = 10 * 24 * time.Hour

// Executor runs a signed execution on this host (hostexec.Executor).
type Executor interface {
	Handle(ctx context.Context, raw []byte) hostexec.Result
	ChainHead() string
}

// Agent is one host agent's connection.
type Agent struct {
	Addr       string // flotillad's host endpoint, host:port
	ServerName string // the name in flotillad's server certificate
	Root       *x509.Certificate
	Host       string
	Version    string
	Key        ed25519.PrivateKey
	LoadChain  func() ([]byte, error) // this agent's certificate and the intermediate, PEM
	SaveChain  func([]byte) error     // a renewed one
	Exec       Executor
	Heartbeat  time.Duration
	Log        *slog.Logger
	Now        func() time.Time
}

func (a *Agent) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func tlsConfig(root *x509.Certificate, serverName string) *tls.Config {
	pool := x509.NewCertPool()
	pool.AddCert(root)
	return &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: pool, ServerName: serverName}
}

func parseChain(b []byte) ([]*x509.Certificate, error) {
	var out []*x509.Certificate
	for blk, rest := pem.Decode(b); blk != nil; blk, rest = pem.Decode(rest) {
		c, err := x509.ParseCertificate(blk.Bytes)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	if len(out) == 0 {
		return nil, errors.New("no certificate")
	}
	return out, nil
}

// checkChain is the agent's own check of a certificate flotillad gave it: for this host, up to the
// pinned root, for this key.
func checkChain(b []byte, root *x509.Certificate, host string, key ed25519.PrivateKey, now time.Time) ([]*x509.Certificate, error) {
	chain, err := parseChain(b)
	if err != nil {
		return nil, err
	}
	name, _, err := hostca.VerifyHost(chain, root, now)
	if err != nil {
		return nil, err
	}
	if name != host {
		return nil, fmt.Errorf("a certificate for %q, not %q", name, host)
	}
	if pub, ok := chain[0].PublicKey.(ed25519.PublicKey); !ok || !pub.Equal(key.Public()) {
		return nil, errors.New("a certificate for another key")
	}
	return chain, nil
}

func refusal(f hostwire.Frame) error {
	var rb hostwire.RefusalBody
	f.Decode(&rb)
	return fmt.Errorf("flotillad refused: %s", rb.Detail)
}

// Enrol makes this host's first certificate: a connection with no client certificate, the token and a
// request for key. It returns the certificate and the intermediate, PEM, checked.
func Enrol(ctx context.Context, addr, serverName string, root *x509.Certificate, host, token string, key ed25519.PrivateKey) ([]byte, error) {
	csr, err := hostca.HostRequest(key, host)
	if err != nil {
		return nil, err
	}
	d := tls.Dialer{NetDialer: &net.Dialer{Timeout: 10 * time.Second}, Config: tlsConfig(root, serverName)}
	nc, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	defer nc.Close()
	nc.SetDeadline(time.Now().Add(30 * time.Second))
	if err := hostwire.Write(nc, hostwire.Enrol, hostwire.EnrolBody{Host: host, Token: token, CSR: csr}); err != nil {
		return nil, err
	}
	f, err := hostwire.Read(nc)
	if err != nil {
		return nil, err
	}
	if f.Type != hostwire.Enrolled {
		return nil, refusal(f)
	}
	var cb hostwire.ChainBody
	if err := f.Decode(&cb); err != nil {
		return nil, err
	}
	if _, err := checkChain([]byte(cb.Chain), root, host, key, time.Now()); err != nil {
		return nil, fmt.Errorf("the enrolled certificate: %w", err)
	}
	return []byte(cb.Chain), nil
}

// Run keeps the connection until ctx ends, reconnecting with backoff (1 s doubling to 60 s, with
// jitter; back to 1 s after a connection that lasted a minute).
func (a *Agent) Run(ctx context.Context) error {
	if a.Heartbeat <= 0 {
		a.Heartbeat = 30 * time.Second
	}
	backoff := time.Second
	for {
		start := time.Now()
		err := a.session(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, errRenewed) {
			backoff = time.Second
			continue
		}
		if time.Since(start) > time.Minute {
			backoff = time.Second
		}
		wait := backoff/2 + rand.N(backoff)
		a.Log.Warn("connection to flotillad lost", "err", err, "retry_in", wait.Round(time.Millisecond))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
		backoff = min(2*backoff, time.Minute)
	}
}

type session struct {
	nc  *tls.Conn
	wmu sync.Mutex
}

func (s *session) write(t string, v any) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	s.nc.SetWriteDeadline(time.Now().Add(30 * time.Second))
	return hostwire.Write(s.nc, t, v)
}

func (a *Agent) session(ctx context.Context) error {
	pemChain, err := a.LoadChain()
	if err != nil {
		return err
	}
	chain, err := checkChain(pemChain, a.Root, a.Host, a.Key, a.now())
	if err != nil {
		return fmt.Errorf("this host's certificate: %w", err)
	}
	cfg := tlsConfig(a.Root, a.ServerName)
	raw := make([][]byte, len(chain))
	for i, c := range chain {
		raw[i] = c.Raw
	}
	cfg.Certificates = []tls.Certificate{{Certificate: raw, PrivateKey: a.Key, Leaf: chain[0]}}
	d := tls.Dialer{NetDialer: &net.Dialer{Timeout: 10 * time.Second}, Config: cfg}
	nc, err := d.DialContext(ctx, "tcp", a.Addr)
	if err != nil {
		return err
	}
	s := &session{nc: nc.(*tls.Conn)}
	defer nc.Close()
	stop := context.AfterFunc(ctx, func() { nc.Close() })
	defer stop()
	if err := s.write(hostwire.Hello, hostwire.HelloBody{Version: a.Version, Clock: a.now().UTC(), ChainHead: a.Exec.ChainHead()}); err != nil {
		return err
	}
	nc.SetReadDeadline(time.Now().Add(15 * time.Second))
	f, err := hostwire.Read(nc)
	if err != nil {
		return err
	}
	if f.Type != hostwire.Welcome {
		return refusal(f)
	}
	nc.SetReadDeadline(time.Time{})
	a.Log.Info("connected to flotillad", "addr", a.Addr, "certificate_until", chain[0].NotAfter)
	if chain[0].NotAfter.Sub(a.now()) < RenewBefore {
		csr, err := hostca.HostRequest(a.Key, a.Host)
		if err != nil {
			return err
		}
		if err := s.write(hostwire.Renew, hostwire.RenewBody{CSR: csr}); err != nil {
			return err
		}
	}
	beats := make(chan error, 1)
	go func() {
		t := time.NewTicker(a.Heartbeat)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				if err := s.write(hostwire.Heartbeat, hostwire.HeartbeatBody{Clock: a.now().UTC(), ChainHead: a.Exec.ChainHead()}); err != nil {
					beats <- err
					nc.Close()
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	for {
		f, err := hostwire.Read(nc)
		if err != nil {
			select {
			case berr := <-beats:
				return fmt.Errorf("heartbeat: %w", berr)
			default:
				return err
			}
		}
		switch f.Type {
		case hostwire.Execute:
			var eb hostwire.ExecuteBody
			if err := f.Decode(&eb); err != nil {
				return err
			}
			go func() {
				r := a.Exec.Handle(ctx, eb.Envelope)
				if err := s.write(hostwire.Result, r); err != nil {
					// the result stays in the host's store; flotillad marks the execution unknown, and a
					// resend of the same envelope returns it
					a.Log.Warn("a result could not be sent", "execution", r.ExecutionID, "status", r.Status, "err", err)
				}
			}()
		case hostwire.Renewed:
			var cb hostwire.ChainBody
			if err := f.Decode(&cb); err != nil {
				return err
			}
			next, err := checkChain([]byte(cb.Chain), a.Root, a.Host, a.Key, a.now())
			if err != nil {
				a.Log.Error("a renewed certificate refused", "err", err)
				continue
			}
			if err := a.SaveChain([]byte(cb.Chain)); err != nil {
				a.Log.Error("a renewed certificate could not be saved", "err", err)
				continue
			}
			a.Log.Info("certificate renewed", "until", next[0].NotAfter)
			return errRenewed
		case hostwire.Refusal:
			return refusal(f)
		default:
			return fmt.Errorf("a %q frame from flotillad", f.Type)
		}
	}
}
