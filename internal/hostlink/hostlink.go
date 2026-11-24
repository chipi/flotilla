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
	"strings"
	"sync"
	"time"

	"github.com/chipi/flotilla/internal/hostca"
	"github.com/chipi/flotilla/internal/hostexec"
	"github.com/chipi/flotilla/internal/hostwire"
	"github.com/chipi/flotilla/internal/spec"
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
	RenewHost(ctx context.Context, host, oldCert string, csrDER []byte) ([]byte, error)
}

// HostState is what flotillad knows of a connected (or once connected) host.
type HostState struct {
	Host        string                   `json:"host"`
	Connected   bool                     `json:"connected"`
	Version     string                   `json:"version"`
	ConnectedAt time.Time                `json:"connected_at"`
	LastContact time.Time                `json:"last_contact"`
	ClockOffset time.Duration            `json:"clock_offset_ns"` // the agent's clock minus flotillad's, at its last frame
	ChainHead   string                   `json:"chain_head"`
	Ceiling     string                   `json:"ceiling,omitempty"`
	Allowlist   []hostwire.AllowedAction `json:"allowlist,omitempty"`
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
	// OnApproval, when set, answers a click a connector relayed (api.RelayedApproval); without it a relayed
	// approval is refused.
	OnApproval func(ctx context.Context, host string, b hostwire.ApprovalBody) hostwire.AnsweredBody
	// Workers maps a remote worker's identity (its certificate name) to the worker it serves (RFC-0011);
	// a connection under such an identity speaks only the worker frames.
	Workers map[string]string
	// Version is flotillad's: a remote worker at another version is refused, loudly.
	Version string
	// RequireRunToken refuses a remote worker whose hello does not list the run_token capability (run_tokens: required): an agent that cannot carry a
	// run's token would run its routines with the worker's, which flotillad no longer accepts. Fail closed, with the way out in the message.
	RequireRunToken bool

	mu     sync.Mutex
	conns  map[string]*conn
	state  map[string]*HostState
	wconns map[string]*conn
	wstate map[string]*WorkerState
	runs   map[string]*remoteRun
	// orphans: runs of a worker that the previous flotillad left in flight, to be cancelled when its link connects
	orphans map[string][]string
	// refusedPair is, per remote worker identity, the (its version / flotillad's) pair last refused and logged: a refusal logs at ERROR
	// only when the pair is new, and the next acceptance logs once that it is accepted again
	refusedPair map[string]string
	// closing and active: Close stops accepting, closes every live connection and waits for their handlers (wg), which write the
	// store through OnContact; the store is closed after it
	closing bool
	active  map[*tls.Conn]struct{}
	wg      sync.WaitGroup
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
		if !s.spawn(tls.Server(nc, cfg), s.handle) {
			nc.Close()
			return net.ErrClosed
		}
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
	if name, ok := s.Workers[host]; ok {
		s.handleWorker(ctx, tc, hostca.Fingerprint(chain[0]), host, name)
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
		ClockOffset: hello.Clock.Sub(at), ChainHead: hello.ChainHead, Ceiling: reportedCeiling(hello.Ceiling), Allowlist: hello.Allowlist}
	s.mu.Unlock()
	s.Log.Info("host connected", "host", host, "version", hello.Version, "clock_offset", hello.Clock.Sub(at))
	s.report(host)
	defer s.drop(c)
	cert := hostca.Fingerprint(chain[0])
	logBudget := budget{}
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
		case hostwire.Approval:
			var ab hostwire.ApprovalBody
			if err := f.Decode(&ab); err != nil {
				refuse(tc, err.Error())
				return
			}
			ans := hostwire.AnsweredBody{Nonce: ab.Nonce, Text: "refused: this flotillad takes no approvals from Slack"}
			if s.OnApproval != nil {
				ans = s.OnApproval(ctx, host, ab)
			}
			if err := c.write(hostwire.Answered, ans); err != nil {
				s.Log.Warn("host: the answer to a relayed approval was not sent", "host", host, "err", err)
			}
		case hostwire.Log:
			var lb hostwire.LogBody
			if err := f.Decode(&lb); err != nil {
				refuse(tc, err.Error())
				return
			}
			s.hostLog(ctx, host, &logBudget, lb)
		case hostwire.Renew:
			var rb hostwire.RenewBody
			if err := f.Decode(&rb); err != nil {
				refuse(tc, err.Error())
				return
			}
			chainPEM, err := s.Auth.RenewHost(ctx, host, cert, rb.CSR)
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

// spawn runs h on tc in its own goroutine, tracked so Close can wait for it; false (nothing started) once Close has begun.
func (s *Server) spawn(tc *tls.Conn, h func(*tls.Conn)) bool {
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		return false
	}
	if s.active == nil {
		s.active = map[*tls.Conn]struct{}{}
	}
	s.active[tc] = struct{}{}
	s.wg.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.wg.Done()
		defer func() { s.mu.Lock(); delete(s.active, tc); s.mu.Unlock() }()
		h(tc)
	}()
	return true
}

// closeWait is how long Close waits for the handlers: they end on their closed connections, bar a store write in flight.
const closeWait = 10 * time.Second

// CloseAll drops every connection, workers' links included; the agents reconnect. The endpoint keeps accepting.
func (s *Server) CloseAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for tc := range s.active {
		tc.Close()
	}
}

// Close is flotillad stopping: it stops accepting, drops every connection and returns once their handlers are done, so none
// writes the store (OnContact) after the caller closes it. The agents and the links reconnect to the next flotillad.
func (s *Server) Close() {
	s.mu.Lock()
	s.closing = true
	s.mu.Unlock()
	s.CloseAll()
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(closeWait):
		s.Log.Warn("host endpoint: handlers still running after close", "wait", closeWait)
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

// A host's log lines (hostwire.Log): logged by flotillad as "host log" with the host's name, so they are
// collected with flotillad's own. An agent may send at most hostLogPerMinute a minute; the rest are
// counted and the count logged once the minute is over. Text is cut to hostLogText.
const (
	hostLogPerMinute = 120
	hostLogText      = 2000
	hostLogAttrs     = 20
)

type budget struct {
	start         time.Time
	used, dropped int
}

func (s *Server) hostLog(ctx context.Context, host string, b *budget, l hostwire.LogBody) {
	now := s.now()
	if now.Sub(b.start) >= time.Minute {
		if b.dropped > 0 {
			s.Log.Warn("host log lines dropped", "host", host, "dropped", b.dropped, "per_minute", hostLogPerMinute)
		}
		*b = budget{start: now}
	}
	if b.used >= hostLogPerMinute {
		b.dropped++
		return
	}
	b.used++
	level := slog.LevelInfo
	switch strings.ToUpper(l.Level) {
	case "WARN":
		level = slog.LevelWarn
	case "ERROR":
		level = slog.LevelError
	}
	args := []any{"host", host, "line", cut(l.Msg, hostLogText), "host_time", l.At}
	n := 0
	for _, k := range sortedAttrKeys(l.Attrs) {
		if n == hostLogAttrs {
			break
		}
		if k == "host" || k == "line" || k == "host_time" || k == "msg" || k == "level" {
			continue
		}
		args = append(args, "a_"+cut(k, 64), cut(l.Attrs[k], hostLogText))
		n++
	}
	s.Log.Log(ctx, level, "host log", args...)
}

func cut(v string, n int) string {
	if len(v) <= n {
		return v
	}
	return v[:n]
}

func sortedAttrKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Notify sends a plan that waits for a person to a connected host's connector.
func (s *Server) Notify(host string, n hostwire.NotifyBody) error {
	s.mu.Lock()
	c := s.conns[host]
	s.mu.Unlock()
	if c == nil {
		return ErrNotConnected
	}
	return c.write(hostwire.Notify, n)
}

// reportedCeiling reads the ceiling a host agent reports. An agent not yet upgraded past the rename still says
// "shadow" for its lowest level; stored verbatim it would undo what schema step 28 moved to "record".
func reportedCeiling(c string) string {
	c, _ = spec.CanonicalLevel(c)
	return c
}
