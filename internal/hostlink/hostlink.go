// Package hostlink is flotillad's host endpoint (ADR-0006 §2): its own listener, mutual TLS terminated
// here (a proxy in front may only pass TCP through). Each host agent dials out and keeps one connection;
// flotillad sends executions down it. A connection without a client certificate may only enrol; one
// with a certificate must be the host's current one (Auth), checked again at every heartbeat, so a
// revoked host is cut off within one heartbeat. A lost connection means no executions: one in flight
// comes back ErrUnknown.
package hostlink

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sort"
	"sync"
	"time"

	"github.com/chipi/flotilla/internal/hostexec"
	"github.com/chipi/flotilla/internal/hostwire"
)

// DefaultHeartbeat is the agents' interval unless configured.
const DefaultHeartbeat = 30 * time.Second

var (
	ErrNotConnected = errors.New("the host is not connected: nothing was sent")
	ErrUnknown      = errors.New("the execution was sent, and its result never came: unknown")
)

// Auth is flotillad's side of identity (internal/api).
type Auth interface {
	AuthenticateHost(ctx context.Context, chain []*x509.Certificate) (string, error)
	EnrolHost(ctx context.Context, host, token string, csrDER []byte) ([]byte, error)
	RenewHost(ctx context.Context, host, oldSerial string, csrDER []byte) ([]byte, error)
}

// HostState is what flotillad knows of a connected (or once connected) host.
type HostState struct {
	Host        string        `json:"host"`
	Connected   bool          `json:"connected"`
	Version     string        `json:"version"`
	ConnectedAt time.Time     `json:"connected_at"`
	LastContact time.Time     `json:"last_contact"`
	ClockOffset time.Duration `json:"clock_offset_ns"` // the agent's clock minus flotillad's, at its last frame
	ChainHead   string        `json:"chain_head"`
}

// Server is the endpoint.
type Server struct {
	Auth Auth
	TLS  *tls.Config // the server certificate; ClientAuth is set here
	Log  *slog.Logger
	// Heartbeat is the agents' interval; a connection silent for three of them is closed.
	Heartbeat time.Duration
	Now       func() time.Time
	// OnContact, when set, gets a host's state after each change (connect, heartbeat, result, drop):
	// flotillad keeps it in its store for `host list` and the alerts.
	OnContact func(HostState)

	mu    sync.Mutex
	conns map[string]*conn
	state map[string]*HostState
}

type conn struct {
	host    string
	tc      *tls.Conn
	wmu     sync.Mutex
	pending map[string]chan hostexec.Result
	closed  chan struct{}
}

func (c *conn) write(t string, v any) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	c.tc.SetWriteDeadline(time.Now().Add(30 * time.Second))
	return hostwire.Write(c.tc, t, v)
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Serve accepts host agents on ln until it is closed.
func (s *Server) Serve(ln net.Listener) error {
	cfg := s.TLS.Clone()
	cfg.MinVersion = tls.VersionTLS13
	// verified in handle (Auth), after the handshake: a connection without a certificate may still enrol
	cfg.ClientAuth = tls.RequestClientCert
	s.mu.Lock()
	if s.conns == nil {
		s.conns, s.state = map[string]*conn{}, map[string]*HostState{}
	}
	if s.Heartbeat <= 0 {
		s.Heartbeat = DefaultHeartbeat
	}
	s.mu.Unlock()
	for {
		nc, err := ln.Accept()
		if err != nil {
			return err
		}
		go s.handle(tls.Server(nc, cfg))
	}
}

// report hands a copy of host's state to OnContact, outside the lock.
func (s *Server) report(host string) {
	if s.OnContact == nil {
		return
	}
	s.mu.Lock()
	st := *s.state[host]
	s.mu.Unlock()
	s.OnContact(st)
}

func refuse(tc *tls.Conn, detail string) {
	tc.SetWriteDeadline(time.Now().Add(5 * time.Second))
	hostwire.Write(tc, hostwire.Refusal, hostwire.RefusalBody{Detail: detail})
}

func (s *Server) handle(tc *tls.Conn) {
	defer tc.Close()
	ctx := context.Background()
	tc.SetDeadline(time.Now().Add(10 * time.Second))
	if err := tc.Handshake(); err != nil {
		s.Log.Warn("host endpoint: handshake", "remote", tc.RemoteAddr().String(), "err", err)
		return
	}
	chain := tc.ConnectionState().PeerCertificates
	if len(chain) == 0 {
		s.enrol(ctx, tc)
		return
	}
	host, err := s.Auth.AuthenticateHost(ctx, chain)
	if err != nil {
		s.Log.Warn("host endpoint: refused", "remote", tc.RemoteAddr().String(), "err", err)
		refuse(tc, "refused: "+err.Error())
		return
	}
	f, err := hostwire.Read(tc)
	var hello hostwire.HelloBody
	if err == nil && f.Type != hostwire.Hello {
		err = fmt.Errorf("the first frame is %q, not hello", f.Type)
	}
	if err == nil {
		err = f.Decode(&hello)
	}
	if err != nil {
		refuse(tc, err.Error())
		return
	}
	c := &conn{host: host, tc: tc, pending: map[string]chan hostexec.Result{}, closed: make(chan struct{})}
	if err := c.write(hostwire.Welcome, hostwire.WelcomeBody{Clock: s.now().UTC()}); err != nil {
		return
	}
	at := s.now()
	s.mu.Lock()
	if old := s.conns[host]; old != nil {
		old.tc.Close() // one connection per host: the newer one wins
	}
	s.conns[host] = c
	s.state[host] = &HostState{Host: host, Connected: true, Version: hello.Version, ConnectedAt: at, LastContact: at,
		ClockOffset: hello.Clock.Sub(at), ChainHead: hello.ChainHead}
	s.mu.Unlock()
	s.Log.Info("host connected", "host", host, "version", hello.Version, "clock_offset", hello.Clock.Sub(at))
	s.report(host)
	defer s.drop(c)
	serial := chain[0].SerialNumber.Text(16)
	for {
		tc.SetReadDeadline(time.Now().Add(3 * s.Heartbeat))
		f, err := hostwire.Read(tc)
		if err != nil {
			s.Log.Warn("host disconnected", "host", host, "err", err)
			return
		}
		switch f.Type {
		case hostwire.Heartbeat:
			var hb hostwire.HeartbeatBody
			if err := f.Decode(&hb); err != nil {
				refuse(tc, err.Error())
				return
			}
			// revoked, replaced or expired since it connected: cut off now
			if _, err := s.Auth.AuthenticateHost(ctx, chain); err != nil {
				s.Log.Warn("host cut off", "host", host, "err", err)
				c.write(hostwire.Refusal, hostwire.RefusalBody{Detail: "refused: " + err.Error()})
				return
			}
			now := s.now()
			s.mu.Lock()
			st := s.state[host]
			st.LastContact, st.ClockOffset, st.ChainHead = now, hb.Clock.Sub(now), hb.ChainHead
			s.mu.Unlock()
			s.report(host)
		case hostwire.Result:
			var r hostexec.Result
			if err := f.Decode(&r); err != nil {
				refuse(tc, err.Error())
				return
			}
			s.mu.Lock()
			ch := c.pending[r.ExecutionID]
			delete(c.pending, r.ExecutionID)
			if r.ChainHead != "" {
				s.state[host].ChainHead = r.ChainHead
			}
			s.state[host].LastContact = s.now()
			s.mu.Unlock()
			s.report(host)
			if ch == nil {
				s.Log.Warn("host sent a result nobody waits for", "host", host, "execution", r.ExecutionID, "status", r.Status)
				continue
			}
			ch <- r
		case hostwire.Renew:
			var rb hostwire.RenewBody
			if err := f.Decode(&rb); err != nil {
				refuse(tc, err.Error())
				return
			}
			chainPEM, err := s.Auth.RenewHost(ctx, host, serial, rb.CSR)
			if err != nil {
				s.Log.Warn("host renewal refused", "host", host, "err", err)
				c.write(hostwire.Refusal, hostwire.RefusalBody{Detail: "renewal refused: " + err.Error()})
				return
			}
			// this connection's certificate is no longer current: the agent reconnects with the new one
			c.write(hostwire.Renewed, hostwire.ChainBody{Chain: string(chainPEM)})
			return
		default:
			refuse(tc, fmt.Sprintf("a %q frame from an agent", f.Type))
			return
		}
	}
}

// enrol is the only thing a connection without a client certificate may do.
func (s *Server) enrol(ctx context.Context, tc *tls.Conn) {
	f, err := hostwire.Read(tc)
	var eb hostwire.EnrolBody
	if err == nil && f.Type != hostwire.Enrol {
		err = fmt.Errorf("without a certificate, only enrolment: got %q", f.Type)
	}
	if err == nil {
		err = f.Decode(&eb)
	}
	if err != nil {
		s.Log.Warn("host endpoint: refused", "remote", tc.RemoteAddr().String(), "err", err)
		refuse(tc, err.Error())
		return
	}
	chainPEM, err := s.Auth.EnrolHost(ctx, eb.Host, eb.Token, eb.CSR)
	if err != nil {
		refuse(tc, "enrolment refused: "+err.Error())
		return
	}
	tc.SetWriteDeadline(time.Now().Add(10 * time.Second))
	hostwire.Write(tc, hostwire.Enrolled, hostwire.ChainBody{Chain: string(chainPEM)})
}

func (s *Server) drop(c *conn) {
	s.mu.Lock()
	close(c.closed)
	current := s.conns[c.host] == c
	if current {
		delete(s.conns, c.host)
		s.state[c.host].Connected = false
	}
	s.mu.Unlock()
	if current {
		s.report(c.host)
	}
}

// Execute sends a signed execution to host and waits for its result, up to timeout. ErrNotConnected:
// nothing was sent. ErrUnknown: it was sent, and the connection was lost or the time ran out first (the
// host's own record is the truth; the target stays held).
func (s *Server) Execute(ctx context.Context, host, executionID string, envelope []byte, timeout time.Duration) (hostexec.Result, error) {
	s.mu.Lock()
	c := s.conns[host]
	if c == nil {
		s.mu.Unlock()
		return hostexec.Result{}, ErrNotConnected
	}
	ch := make(chan hostexec.Result, 1)
	c.pending[executionID] = ch
	s.mu.Unlock()
	forget := func() {
		s.mu.Lock()
		delete(c.pending, executionID)
		s.mu.Unlock()
	}
	if err := c.write(hostwire.Execute, hostwire.ExecuteBody{Envelope: envelope}); err != nil {
		forget()
		c.tc.Close()
		return hostexec.Result{}, fmt.Errorf("%w (%v)", ErrUnknown, err)
	}
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case r := <-ch:
		return r, nil
	case <-c.closed:
		forget()
		return hostexec.Result{}, fmt.Errorf("%w: the connection was lost", ErrUnknown)
	case <-t.C:
		forget()
		return hostexec.Result{}, fmt.Errorf("%w: no result within %s", ErrUnknown, timeout)
	case <-ctx.Done():
		forget()
		return hostexec.Result{}, fmt.Errorf("%w: %v", ErrUnknown, ctx.Err())
	}
}

// CloseAll drops every connection (flotillad stopping); the agents reconnect.
func (s *Server) CloseAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.conns {
		c.tc.Close()
	}
}

// Hosts is every host seen since flotillad started, connected or not.
func (s *Server) Hosts() []HostState {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]HostState, 0, len(s.state))
	for _, st := range s.state {
		out = append(out, *st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Host < out[j].Host })
	return out
}
