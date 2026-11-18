package hostagent

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chipi/flotilla/internal/api"
	"github.com/chipi/flotilla/internal/hostca"
	"github.com/chipi/flotilla/internal/hostexec"
	"github.com/chipi/flotilla/internal/hostlink"
	"github.com/chipi/flotilla/internal/hostwire"
	"github.com/chipi/flotilla/internal/store"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

var operator = api.Caller{Name: "operator", Role: api.RoleOperator}

// fakeExec answers each execution with its envelope's bytes as the id; block holds it until released.
type fakeExec struct {
	mu    sync.Mutex
	ran   []string
	block chan struct{}
}

func (f *fakeExec) Handle(ctx context.Context, raw []byte) hostexec.Result {
	f.mu.Lock()
	f.ran = append(f.ran, string(raw))
	b := f.block
	f.mu.Unlock()
	if b != nil {
		<-b
	}
	return hostexec.Result{ExecutionID: string(raw), Status: hostexec.Done, ChainHead: "head-" + string(raw)}
}

func (f *fakeExec) ChainHead() string { return "head-0" }

type world struct {
	api  *api.Server
	link *hostlink.Server
	addr string
	root *hostca.CA
}

func newWorld(t *testing.T, opts ...func(*hostlink.Server)) world {
	t.Helper()
	ll := quiet
	st, err := store.Open(filepath.Join(t.TempDir(), "f.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	root, _ := hostca.NewRoot(time.Now())
	inter, _ := hostca.NewIntermediate(root, time.Now())
	a := &api.Server{Store: st, Log: quiet, HostCA: inter, HostRoot: root.Cert}
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	srvCert, err := inter.IssueServer(pub, []string{"flotilla"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	link := &hostlink.Server{Auth: a, Log: ll, Heartbeat: 200 * time.Millisecond,
		TLS: &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{srvCert.Raw, inter.Cert.Raw}, PrivateKey: key}}},
		// as flotillad wires it (cmd/flotillad recordContact)
		OnContact: func(h hostlink.HostState) {
			allow := ""
			if len(h.Allowlist) > 0 {
				b, _ := json.Marshal(h.Allowlist)
				allow = string(b)
			}
			st.RecordHostContact(context.Background(), store.HostContact{Host: h.Host, Connected: h.Connected, Version: h.Version,
				ConnectedAt: h.ConnectedAt, LastContact: h.LastContact, ClockOffset: h.ClockOffset, ChainHead: h.ChainHead,
				Ceiling: h.Ceiling, Allowlist: allow})
		}}
	for _, o := range opts {
		o(link)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go link.Serve(ln)
	return world{a, link, ln.Addr().String(), root}
}

// agent enrols a host with a fresh key and returns an Agent for it, not yet running.
func (w world) agent(t *testing.T, host string, x Executor) *Agent {
	t.Helper()
	tok, _, err := w.api.MintEnrolment(context.Background(), operator, "test", host)
	if err != nil {
		t.Fatal(err)
	}
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	chain, err := Enrol(context.Background(), w.addr, "flotilla", w.root.Cert, host, tok, key)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	return &Agent{Addr: w.addr, ServerName: "flotilla", Root: w.root.Cert, Host: host, Version: "test", Key: key,
		LoadChain: func() ([]byte, error) { mu.Lock(); defer mu.Unlock(); return chain, nil },
		SaveChain: func(b []byte) error { mu.Lock(); defer mu.Unlock(); chain = b; return nil },
		Exec:      x, Heartbeat: 200 * time.Millisecond, Log: quiet}
}

func run(t *testing.T, a *Agent) context.CancelFunc {
	ctx, cancel := context.WithCancel(context.Background())
	go a.Run(ctx)
	t.Cleanup(cancel)
	return cancel
}

// waitFor polls cond for up to 5 s.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for i := 0; i < 250; i++ {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("waited 5 s for %s", what)
}

func connected(w world, host string) func() bool {
	return func() bool {
		for _, h := range w.link.Hosts() {
			if h.Host == host {
				return h.Connected
			}
		}
		return false
	}
}

func TestConnectExecuteHeartbeat(t *testing.T) {
	w := newWorld(t)
	x := &fakeExec{}
	a := w.agent(t, "host-01", x)
	// The agent's clock runs 60 s ahead. The offset flotillad measures includes the frame's way there:
	// under a loaded machine a heartbeat took ~2 s (ci-fast, 2026-11-13), so a small skew vanished.
	a.Now = func() time.Time { return time.Now().Add(60 * time.Second) }
	run(t, a)
	waitFor(t, "the agent to connect", connected(w, "host-01"))
	r, err := w.link.Execute(context.Background(), "host-01", "x1", []byte("x1"), 2*time.Second)
	if err != nil || r.ExecutionID != "x1" || r.Status != hostexec.Done {
		t.Fatalf("execute: %+v %v", r, err)
	}
	st := w.link.Hosts()[0]
	if st.ChainHead != "head-x1" || st.Version != "test" {
		t.Fatalf("state after a result: %+v", st)
	}
	if st.ClockOffset < 50*time.Second || st.ClockOffset > 61*time.Second {
		t.Fatalf("clock offset %s, want about 60 s", st.ClockOffset)
	}
	// heartbeats keep it connected over many intervals, and update its last contact, in the store too
	before := st.LastContact
	hs0, _ := w.api.Store.Hosts(context.Background())
	time.Sleep(700 * time.Millisecond) // three heartbeats at 200 ms
	if hs, _ := w.api.Store.Hosts(context.Background()); hs[0].LastContact <= hs0[0].LastContact {
		t.Fatalf("the stored last contact did not move with heartbeats: %s then %s", hs0[0].LastContact, hs[0].LastContact)
	}
	if st := w.link.Hosts()[0]; !st.Connected || !st.LastContact.After(before) {
		t.Fatalf("after three heartbeats: %+v", st)
	}
	if _, err := w.link.Execute(context.Background(), "model-host", "x2", []byte("x2"), time.Second); !errors.Is(err, hostlink.ErrNotConnected) {
		t.Fatalf("a host never connected: %v", err)
	}
	// the store has what the endpoint heard: `host list` and the alerts read it
	hs, _ := w.api.Store.Hosts(context.Background())
	if h := hs[0]; !h.Connected || h.Version != "test" || h.LastContact == "" || h.ClockOffsetMS < 50000 || h.ClockOffsetMS > 61000 || h.ChainHead == "" {
		t.Fatalf("the stored host: %+v", h)
	}
}

// The agent reports its own config in hello (G10): flotillad keeps the ceiling and the allowlist for
// people to read; an agent that reports none (older) leaves what was stored.
func TestHelloReportsTheConfig(t *testing.T) {
	w := newWorld(t)
	a := w.agent(t, "host-01", &fakeExec{})
	a.Ceiling = "propose"
	a.Allowlist = []hostwire.AllowedAction{{Name: "backup.rerun", Argv: []string{"/usr/bin/sudo", "-n", "/bin/launchctl", "kickstart", "x"},
		PerHour: 1, Timeout: "30m0s"}}
	cancel := run(t, a)
	waitFor(t, "the agent to connect", connected(w, "host-01"))
	if st := w.link.Hosts()[0]; st.Ceiling != "propose" || len(st.Allowlist) != 1 || st.Allowlist[0].Argv[3] != "kickstart" {
		t.Fatalf("the link's state: %+v", st)
	}
	waitFor(t, "the store to have it", func() bool {
		hs, _ := w.api.Store.Hosts(context.Background())
		return len(hs) == 1 && hs[0].Ceiling == "propose" && strings.Contains(hs[0].Allowlist, "kickstart")
	})
	cancel()
	b := w.agent(t, "model-host", &fakeExec{})
	run(t, b)
	waitFor(t, "an agent without a report to connect", connected(w, "model-host"))
	if err := w.api.Store.RecordHostContact(context.Background(), store.HostContact{Host: "host-01", Connected: true}); err != nil {
		t.Fatal(err)
	}
	hs, _ := w.api.Store.Hosts(context.Background())
	for _, h := range hs {
		if h.Name == "host-01" && (h.Ceiling != "propose" || !strings.Contains(h.Allowlist, "kickstart")) {
			t.Fatalf("a contact without a report erased the stored one: %+v", h)
		}
	}
}

// Revoking cuts a connected host off within a heartbeat, an execution in flight comes back unknown,
// and it cannot reconnect.
func TestRevokeCutsOffAndInFlightIsUnknown(t *testing.T) {
	w := newWorld(t)
	x := &fakeExec{block: make(chan struct{})}
	defer close(x.block)
	run(t, w.agent(t, "host-01", x))
	waitFor(t, "the agent to connect", connected(w, "host-01"))
	done := make(chan error, 1)
	go func() {
		_, err := w.link.Execute(context.Background(), "host-01", "slow", []byte("slow"), 10*time.Second)
		done <- err
	}()
	waitFor(t, "the execution to reach the agent", func() bool { x.mu.Lock(); defer x.mu.Unlock(); return len(x.ran) == 1 })
	if err := w.api.RevokeHost(context.Background(), operator, "test", "host-01"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, hostlink.ErrUnknown) {
			t.Fatalf("in flight at the cut: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the execution in flight never ended")
	}
	waitFor(t, "the host to show disconnected", func() bool { return !connected(w, "host-01")() })
	waitFor(t, "the store to show it disconnected", func() bool {
		hs, _ := w.api.Store.Hosts(context.Background())
		return len(hs) == 1 && !hs[0].Connected
	})
	time.Sleep(1500 * time.Millisecond) // the agent retries (backoff 1 s): each is refused
	if connected(w, "host-01")() {
		t.Fatal("a revoked host reconnected")
	}
}

// The connection drops (flotillad restarts its side): the agent reconnects by itself.
func TestReconnects(t *testing.T) {
	w := newWorld(t)
	run(t, w.agent(t, "host-01", &fakeExec{}))
	waitFor(t, "the agent to connect", connected(w, "host-01"))
	first := w.link.Hosts()[0].ConnectedAt
	w.link.CloseAll()
	waitFor(t, "the agent to reconnect", func() bool { st := w.link.Hosts()[0]; return st.Connected && st.ConnectedAt.After(first) })
}

// An agent near its certificate's expiry renews over the connection and comes back on the new one; the
// old certificate stops working.
func TestRenewal(t *testing.T) {
	w := newWorld(t)
	a := w.agent(t, "host-01", &fakeExec{})
	old, _ := a.LoadChain()
	var shift atomic.Int64
	shift.Store(int64(25 * 24 * time.Hour)) // 5 days left
	a.Now = func() time.Time { return time.Now().Add(time.Duration(shift.Load())) }
	run(t, a)
	waitFor(t, "a renewal", func() bool { b, _ := a.LoadChain(); return string(b) != string(old) })
	shift.Store(0) // back to the real clock: the new certificate has 30 days, nothing more to renew
	renewed := time.Now()
	waitFor(t, "the agent on its new certificate, and the store agreeing", func() bool {
		hs, _ := w.api.Store.Hosts(context.Background())
		next, _ := a.LoadChain()
		chain, _ := parseChain(next)
		return connected(w, "host-01")() && hs[0].CertSHA256 == hostca.Fingerprint(chain[0])
	})
	// it reconnects at once after a renewal, not after a backoff (at least 0.5 s)
	if d := time.Since(renewed); d > 400*time.Millisecond {
		t.Fatalf("back on the new certificate after %s", d)
	}
	oldChain, _ := parseChain(old)
	if _, err := w.api.AuthenticateHost(context.Background(), oldChain); err == nil {
		t.Fatal("the replaced certificate still authenticates")
	}
}

func TestEnrolRefusals(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	tok, _, _ := w.api.MintEnrolment(ctx, operator, "test", "host-01")
	if _, err := Enrol(ctx, w.addr, "flotilla", w.root.Cert, "model-host", tok, key); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("another host's token: %v", err)
	}
	if _, err := Enrol(ctx, w.addr, "flotilla", w.root.Cert, "host-01", tok, key); err != nil {
		t.Fatal(err)
	}
	if _, err := Enrol(ctx, w.addr, "flotilla", w.root.Cert, "host-01", tok, key); err == nil {
		t.Fatal("a token used twice")
	}
	// the agent refuses a flotillad under another name, or under another root
	tok2, _, _ := w.api.MintEnrolment(ctx, operator, "test", "host-01")
	if _, err := Enrol(ctx, w.addr, "evil", w.root.Cert, "host-01", tok2, key); err == nil {
		t.Fatal("enrolled with a server under another name")
	}
	other := newWorld(t)
	if _, err := Enrol(ctx, w.addr, "flotilla", other.root.Cert, "host-01", tok2, key); err == nil {
		t.Fatal("enrolled with a server under another root")
	}
	// another CA's agent cannot connect: a raw client, past the agent's own checks
	stranger := other.agent(t, "host-01", &fakeExec{})
	strangerChain, _ := stranger.LoadChain()
	c := rawDial(t, w, strangerChain, stranger.Key)
	hostwire.Write(c, hostwire.Hello, hostwire.HelloBody{Version: "x", Clock: time.Now()})
	if f, err := hostwire.Read(c); err == nil && f.Type != hostwire.Refusal {
		t.Fatalf("another CA's agent got %q", f.Type)
	}
	if connected(w, "host-01")() {
		t.Fatal("another CA's agent connected")
	}
	// without a certificate, only an enrol frame: a good token under another type is refused, unspent
	tok3, _, _ := w.api.MintEnrolment(ctx, operator, "test", "host-01")
	csr, _ := hostca.HostRequest(key, "host-01")
	c = rawDial(t, w, nil, nil)
	hostwire.Write(c, hostwire.Hello, hostwire.EnrolBody{Host: "host-01", Token: tok3, CSR: csr})
	if f, err := hostwire.Read(c); err != nil || f.Type != hostwire.Refusal {
		t.Fatalf("an enrolment under another frame type: %q %v", f.Type, err)
	}
	if _, err := Enrol(ctx, w.addr, "flotilla", w.root.Cert, "host-01", tok3, key); err != nil {
		t.Fatalf("the token after the refused frame: %v", err)
	}
}

// rawDial is a TLS client to w's endpoint with an optional certificate, and no agent logic.
func rawDial(t *testing.T, w world, chainPEM []byte, key ed25519.PrivateKey) *tls.Conn {
	t.Helper()
	cfg := tlsConfig(w.root.Cert, "flotilla")
	if chainPEM != nil {
		chain, err := parseChain(chainPEM)
		if err != nil {
			t.Fatal(err)
		}
		var raw [][]byte
		for _, c := range chain {
			raw = append(raw, c.Raw)
		}
		cfg.Certificates = []tls.Certificate{{Certificate: raw, PrivateKey: key}}
	}
	c, err := tls.Dial("tcp", w.addr, cfg)
	if err != nil {
		t.Fatal(err)
	}
	c.SetDeadline(time.Now().Add(5 * time.Second))
	t.Cleanup(func() { c.Close() })
	return c
}

// After a renewal flotillad closes the connection, whatever the agent does: the certificate it
// connected with is no longer current.
func TestRenewalClosesTheConnection(t *testing.T) {
	w := newWorld(t)
	a := w.agent(t, "host-01", &fakeExec{})
	chainPEM, _ := a.LoadChain()
	c := rawDial(t, w, chainPEM, a.Key)
	hostwire.Write(c, hostwire.Hello, hostwire.HelloBody{Version: "raw", Clock: time.Now()})
	if f, err := hostwire.Read(c); err != nil || f.Type != hostwire.Welcome {
		t.Fatalf("hello: %q %v", f.Type, err)
	}
	csr, _ := hostca.HostRequest(a.Key, "host-01")
	hostwire.Write(c, hostwire.Renew, hostwire.RenewBody{CSR: csr})
	if f, err := hostwire.Read(c); err != nil || f.Type != hostwire.Renewed {
		t.Fatalf("renew: %q %v", f.Type, err)
	}
	// a client that keeps going finds the connection closed
	hostwire.Write(c, hostwire.Heartbeat, hostwire.HeartbeatBody{Clock: time.Now()})
	if f, err := hostwire.Read(c); err == nil {
		t.Fatalf("the connection after a renewal answered %q", f.Type)
	}
}

// The agent's own check of a certificate flotillad hands it: this host, this key, the pinned root.
func TestCheckChain(t *testing.T) {
	w := newWorld(t)
	a := w.agent(t, "host-01", &fakeExec{})
	good, _ := a.LoadChain()
	if _, err := checkChain(good, w.root.Cert, "host-01", a.Key, time.Now()); err != nil {
		t.Fatal(err)
	}
	_, otherKey, _ := ed25519.GenerateKey(rand.Reader)
	other := newWorld(t)
	for name, err := range map[string]error{
		"another host":   must(checkChain(good, w.root.Cert, "model-host", a.Key, time.Now())),
		"another key":    must(checkChain(good, w.root.Cert, "host-01", otherKey, time.Now())),
		"another root":   must(checkChain(good, other.root.Cert, "host-01", a.Key, time.Now())),
		"expired":        must(checkChain(good, w.root.Cert, "host-01", a.Key, time.Now().Add(31*24*time.Hour))),
		"no certificate": must(checkChain([]byte("x"), w.root.Cert, "host-01", a.Key, time.Now())),
	} {
		if err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func must(_ any, err error) error { return err }

// An agent that stops sending heartbeats is dropped after three of flotillad's intervals.
func TestSilentAgentDropped(t *testing.T) {
	w := newWorld(t)
	a := w.agent(t, "host-01", &fakeExec{})
	a.Heartbeat = time.Hour
	run(t, a)
	waitFor(t, "the agent to connect", connected(w, "host-01"))
	waitFor(t, "the silent agent to be dropped", func() bool { return !connected(w, "host-01")() })
}

// lockedBuf is a log sink two goroutines can share.
type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *lockedBuf) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

// The agent's own log lines reach flotillad's log while it is connected (Info and above, its
// attributes kept), and lines logged while it was disconnected follow once it connects.
func TestAgentLogLinesReachFlotillad(t *testing.T) {
	var sink lockedBuf
	w := newWorld(t, func(l *hostlink.Server) { l.Log = slog.New(slog.NewJSONHandler(&sink, nil)) })
	a := w.agent(t, "host-01", &fakeExec{})
	a.Forward = NewForwarder(slog.NewJSONHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug}))
	a.Log = slog.New(a.Forward)
	a.Log.Info("logged before it connected", "job", "backup")
	a.Log.Debug("a debug line stays on the host")
	run(t, a)
	waitFor(t, "the agent to connect", connected(w, "host-01"))
	a.Log.Warn("the backup job is slow", "minutes", 42)
	waitFor(t, "the lines in flotillad's log", func() bool {
		s := sink.String()
		return strings.Contains(s, `"msg":"host log","host":"host-01","line":"logged before it connected"`) &&
			strings.Contains(s, `"a_job":"backup"`) && strings.Contains(s, `"level":"WARN","msg":"host log","host":"host-01","line":"the backup job is slow"`) &&
			strings.Contains(s, `"a_minutes":"42"`)
	})
	if strings.Contains(sink.String(), "a debug line") {
		t.Error("a debug line was forwarded")
	}
}

// An agent not yet upgraded past the rename reports its ceiling as "shadow" in hello: the link's state and the
// store keep "record", so the host page and the migration's work do not drift back.
func TestHelloCeilingShadowIsStoredAsRecord(t *testing.T) {
	w := newWorld(t)
	a := w.agent(t, "host-01", &fakeExec{})
	a.Ceiling = "shadow"
	run(t, a)
	waitFor(t, "the agent to connect", connected(w, "host-01"))
	if st := w.link.Hosts()[0]; st.Ceiling != "record" {
		t.Fatalf("the link's state: %+v", st)
	}
	waitFor(t, "the store to have it", func() bool {
		hs, _ := w.api.Store.Hosts(context.Background())
		return len(hs) == 1 && hs[0].Ceiling == "record"
	})
}
