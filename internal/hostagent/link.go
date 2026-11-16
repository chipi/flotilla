package hostagent

// The worker link (RFC-0011 §2): beside a worker on another machine, it dials flotillad's host endpoint
// under the worker's own identity, takes runs and hands each to the worker's local /v1/runs, unchanged,
// and sends the result back. A result that could not be sent (the connection dropped while the run ran)
// is kept and sent on the next connection, which flotillad accepts within the run's deadline. A cancel
// whose time is up ends the local request; the worker's own timeout (the run's timeout_s, which
// flotillad's deadline exceeds) ends the run itself. An operator's cancel (stop) goes to the worker's
// /v1/runs/<id>/cancel, and the run's request stays open for its "cancelled" result.

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"sync"
	"time"

	"github.com/chipi/flotilla/internal/hostca"
	"github.com/chipi/flotilla/internal/hostwire"
	"github.com/chipi/flotilla/internal/worker"
)

// LocalWorker is the worker beside the link (worker.Client).
type LocalWorker interface {
	Run(ctx context.Context, r worker.Request) (*worker.Result, error)
	Cancel(ctx context.Context, runID string) error
}

// Link is one remote worker's connection to flotillad.
type Link struct {
	Addr       string
	ServerName string
	Root       *x509.Certificate
	Identity   string // the certificate name, w-<worker>-<node>
	Worker     string // the worker it serves, as flotillad.yaml names it
	Version    string
	Key        ed25519.PrivateKey
	LoadChain  func() ([]byte, error)
	SaveChain  func([]byte) error
	Local      LocalWorker
	Heartbeat  time.Duration
	Log        *slog.Logger

	mu      sync.Mutex
	cancels map[string]context.CancelFunc
	unsent  []hostwire.RunResultBody
	cur     *session
}

// Run keeps the link until ctx ends, reconnecting with the agent's backoff.
func (l *Link) Run(ctx context.Context) error {
	if l.Heartbeat <= 0 {
		l.Heartbeat = 30 * time.Second
	}
	l.mu.Lock()
	if l.cancels == nil {
		l.cancels = map[string]context.CancelFunc{}
	}
	l.mu.Unlock()
	backoff := time.Second
	for {
		start := time.Now()
		err := l.session(ctx)
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
		l.Log.Warn("link to flotillad lost", "err", err, "retry_in", wait.Round(time.Millisecond))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
		backoff = min(2*backoff, time.Minute)
	}
}

// deliver sends a run's result on the current connection, or keeps it for the next one.
func (l *Link) deliver(rb hostwire.RunResultBody) {
	l.mu.Lock()
	s := l.cur
	l.mu.Unlock()
	if s != nil && s.write(hostwire.RunResult, rb) == nil {
		return
	}
	l.mu.Lock()
	l.unsent = append(l.unsent, rb)
	l.mu.Unlock()
	l.Log.Warn("a run's result is kept for the next connection", "run_id", rb.RunID)
}

func (l *Link) run(ctx context.Context, rb hostwire.RunBody) {
	var req worker.Request
	if err := json.Unmarshal(rb.Request, &req); err != nil {
		l.deliver(hostwire.RunResultBody{RunID: rb.RunID, Error: "the run request: " + err.Error()})
		return
	}
	rctx, cancel := context.WithCancel(ctx)
	l.mu.Lock()
	l.cancels[rb.RunID] = cancel
	l.mu.Unlock()
	defer func() {
		cancel()
		l.mu.Lock()
		delete(l.cancels, rb.RunID)
		l.mu.Unlock()
	}()
	l.Log.Info("run started", "run_id", rb.RunID, "routine", req.Routine)
	res, err := l.Local.Run(rctx, req)
	out := hostwire.RunResultBody{RunID: rb.RunID}
	if err != nil {
		out.Error = err.Error()
	} else if out.Result, err = json.Marshal(res); err != nil {
		out.Error = err.Error()
	}
	l.Log.Info("run ended", "run_id", rb.RunID, "error", out.Error)
	l.deliver(out)
}

func (l *Link) session(ctx context.Context) error {
	pemChain, err := l.LoadChain()
	if err != nil {
		return err
	}
	chain, err := checkChain(pemChain, l.Root, l.Identity, l.Key, time.Now())
	if err != nil {
		return fmt.Errorf("this link's certificate: %w", err)
	}
	cfg := tlsConfig(l.Root, l.ServerName)
	raw := make([][]byte, len(chain))
	for i, c := range chain {
		raw[i] = c.Raw
	}
	cfg.Certificates = []tls.Certificate{{Certificate: raw, PrivateKey: l.Key, Leaf: chain[0]}}
	d := tls.Dialer{NetDialer: &net.Dialer{Timeout: 10 * time.Second}, Config: cfg}
	nc, err := d.DialContext(ctx, "tcp", l.Addr)
	if err != nil {
		return err
	}
	s := &session{nc: nc.(*tls.Conn)}
	defer nc.Close()
	stop := context.AfterFunc(ctx, func() { nc.Close() })
	defer stop()
	if err := s.write(hostwire.WorkerHello, hostwire.WorkerHelloBody{Version: l.Version, Worker: l.Worker}); err != nil {
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
	l.Log.Info("linked to flotillad", "addr", l.Addr, "worker", l.Worker, "certificate_until", chain[0].NotAfter)
	l.mu.Lock()
	l.cur = s
	pending := l.unsent
	l.unsent = nil
	l.mu.Unlock()
	defer func() {
		l.mu.Lock()
		if l.cur == s {
			l.cur = nil
		}
		l.mu.Unlock()
	}()
	for _, rb := range pending {
		l.deliver(rb)
	}
	if chain[0].NotAfter.Sub(time.Now()) < RenewBefore {
		csr, err := hostca.HostRequest(l.Key, l.Identity)
		if err != nil {
			return err
		}
		if err := s.write(hostwire.Renew, hostwire.RenewBody{CSR: csr}); err != nil {
			return err
		}
	}
	beats := make(chan error, 1)
	go func() {
		t := time.NewTicker(l.Heartbeat)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				if err := s.write(hostwire.Heartbeat, hostwire.HeartbeatBody{Clock: time.Now().UTC()}); err != nil {
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
		case hostwire.Run:
			var rb hostwire.RunBody
			if err := f.Decode(&rb); err != nil {
				return err
			}
			go l.run(context.WithoutCancel(ctx), rb) // a run outlives the connection: its result is kept
		case hostwire.Cancel:
			var cb hostwire.CancelBody
			if err := f.Decode(&cb); err != nil {
				return err
			}
			if cb.Stop {
				go func() {
					if err := l.Local.Cancel(context.WithoutCancel(ctx), cb.RunID); err != nil {
						l.Log.Warn("the worker did not take flotillad's cancel", "run_id", cb.RunID, "err", err)
						return
					}
					l.Log.Warn("run cancelled by flotillad's request", "run_id", cb.RunID)
				}()
				continue
			}
			l.mu.Lock()
			cancel := l.cancels[cb.RunID]
			l.mu.Unlock()
			if cancel != nil {
				l.Log.Warn("run cancelled by flotillad (its time is up)", "run_id", cb.RunID)
				cancel()
			}
		case hostwire.Renewed:
			var cb hostwire.ChainBody
			if err := f.Decode(&cb); err != nil {
				return err
			}
			next, err := checkChain([]byte(cb.Chain), l.Root, l.Identity, l.Key, time.Now())
			if err != nil {
				l.Log.Error("a renewed certificate refused", "err", err)
				continue
			}
			if err := l.SaveChain([]byte(cb.Chain)); err != nil {
				l.Log.Error("a renewed certificate could not be saved", "err", err)
				continue
			}
			l.Log.Info("certificate renewed", "until", next[0].NotAfter)
			return errRenewed
		case hostwire.Refusal:
			return refusal(f)
		default:
			return fmt.Errorf("a %q frame from flotillad", f.Type)
		}
	}
}
