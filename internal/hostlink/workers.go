package hostlink

// Remote workers (RFC-0011): a worker on another machine is reached through a link beside it that dials
// this endpoint, as a host agent does, under its own identity (a certificate name the installation
// declares for that worker, `w-<worker>-<node>`). Such a connection speaks only the worker frames: a
// host identity cannot take runs, and a worker identity cannot send host results or approvals. A run is
// one frame out and one frame back; its result is taken on any connection of its worker within the run's
// deadline, so a tailnet blip during a run loses nothing (RFC-0011 §8).

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/chipi/flotilla/internal/hostwire"
	"github.com/chipi/flotilla/internal/worker"
)

// ErrWorkerNotConnected: the remote worker has no connection; nothing was sent.
var ErrWorkerNotConnected = errors.New("the remote worker is not connected: nothing was sent")

// RunGrace is how long past a run's own timeout a remote result is still awaited: the worker's 45 s,
// and room for the link to reconnect and deliver it.
const RunGrace = 3 * time.Minute

// WorkerState is what flotillad knows of a remote worker's link.
type WorkerState struct {
	Worker      string    `json:"worker"`
	Identity    string    `json:"identity"`
	Connected   bool      `json:"connected"`
	Version     string    `json:"version"`
	ConnectedAt time.Time `json:"connected_at"`
	LastContact time.Time `json:"last_contact"`
	Refused     string    `json:"refused,omitempty"` // why its last connection was refused (a version mismatch)
}

type remoteRun struct {
	worker string
	ch     chan hostwire.RunResultBody
}

func (s *Server) initWorkers() {
	if s.wconns == nil {
		s.wconns, s.wstate, s.runs = map[string]*conn{}, map[string]*WorkerState{}, map[string]*remoteRun{}
	}
}

func (s *Server) handleWorker(ctx context.Context, tc *tls.Conn, cert string, identity, name string) {
	f, err := hostwire.Read(tc)
	var hello hostwire.WorkerHelloBody
	if err == nil && f.Type != hostwire.WorkerHello {
		err = fmt.Errorf("the first frame is %q, not worker_hello", f.Type)
	}
	if err == nil {
		err = f.Decode(&hello)
	}
	if err == nil && hello.Worker != name {
		err = fmt.Errorf("identity %s is worker %s's, not %s's", identity, name, hello.Worker)
	}
	if err != nil {
		s.Log.Warn("remote worker refused", "identity", identity, "err", err)
		refuse(tc, err.Error())
		return
	}
	s.mu.Lock()
	s.initWorkers()
	st := s.wstate[name]
	if st == nil {
		st = &WorkerState{Worker: name, Identity: identity}
		s.wstate[name] = st
	}
	if s.Version != "" && hello.Version != s.Version {
		st.Refused = fmt.Sprintf("version %s, not flotillad's %s", hello.Version, s.Version)
		pair := hello.Version + " / " + s.Version
		first := s.refusedPair[identity] != pair
		if s.refusedPair == nil {
			s.refusedPair = map[string]string{}
		}
		s.refusedPair[identity] = pair
		s.mu.Unlock()
		// loud, once: a worker that never runs must not look like one that is only quiet (RFC-0011 §8), but its link retries every few
		// seconds until it is updated (3,592 ERRORs in one deploy window, 2026-11-24); the repeats stay on the worker's status.
		if first {
			s.Log.Error("remote worker refused: its version is not flotillad's", "worker", name, "identity", identity,
				"version", hello.Version, "flotillad", s.Version)
		} else {
			s.Log.Debug("remote worker refused again: its version is not flotillad's", "worker", name, "identity", identity,
				"version", hello.Version, "flotillad", s.Version)
		}
		refuse(tc, "refused: version "+hello.Version+", flotillad runs "+s.Version)
		return
	}
	if s.RequireRunToken && !hello.HasCap(hostwire.CapRunToken) {
		st.Refused = "its agent does not carry run tokens (run_tokens: required): update the agent"
		s.mu.Unlock()
		s.Log.Error("remote worker refused: its agent does not carry run tokens", "worker", name, "identity", identity, "version", hello.Version)
		refuse(tc, "refused: the agent does not carry run tokens (flotillad has run_tokens: required): update the agent to this build")
		return
	}
	s.mu.Unlock()
	c := &conn{host: identity, tc: tc, closed: make(chan struct{})}
	if err := c.write(hostwire.Welcome, hostwire.WelcomeBody{Clock: s.now().UTC()}); err != nil {
		return
	}
	at := s.now()
	s.mu.Lock()
	if old := s.wconns[name]; old != nil {
		old.tc.Close() // one connection per worker: the newer one wins
	}
	s.wconns[name] = c
	*st = WorkerState{Worker: name, Identity: identity, Connected: true, Version: hello.Version, ConnectedAt: at, LastContact: at}
	_, wasRefused := s.refusedPair[identity]
	delete(s.refusedPair, identity)
	orphans := s.orphans[name]
	delete(s.orphans, name)
	s.mu.Unlock()
	s.Log.Info("remote worker connected", "worker", name, "identity", identity, "version", hello.Version)
	if wasRefused {
		s.Log.Info("remote worker accepted again: its version is flotillad's", "worker", name, "identity", identity, "version", hello.Version)
	}
	s.reportWorker(name)
	defer s.dropWorker(name, c)
	s.cancelOrphans(name, c, orphans)
	logBudget := budget{}
	for {
		tc.SetReadDeadline(time.Now().Add(3 * s.Heartbeat))
		f, err := hostwire.Read(tc)
		if err != nil {
			s.Log.Warn("remote worker disconnected", "worker", name, "err", err)
			return
		}
		s.mu.Lock()
		st.LastContact = s.now()
		s.mu.Unlock()
		s.reportWorker(name)
		switch f.Type {
		case hostwire.Heartbeat:
			var hb hostwire.HeartbeatBody
			if err := f.Decode(&hb); err != nil {
				refuse(tc, err.Error())
				return
			}
			if _, err := s.Auth.AuthenticateHost(ctx, tc.ConnectionState().PeerCertificates); err != nil {
				s.Log.Warn("remote worker cut off", "worker", name, "err", err)
				c.write(hostwire.Refusal, hostwire.RefusalBody{Detail: "refused: " + err.Error()})
				return
			}
		case hostwire.RunResult:
			var rb hostwire.RunResultBody
			if err := f.Decode(&rb); err != nil {
				refuse(tc, err.Error())
				return
			}
			s.mu.Lock()
			r := s.runs[rb.RunID]
			if r != nil && r.worker == name {
				delete(s.runs, rb.RunID)
			} else {
				r = nil
			}
			s.mu.Unlock()
			if r == nil {
				s.Log.Warn("remote worker sent a result nobody waits for", "worker", name, "run_id", rb.RunID)
				continue
			}
			r.ch <- rb
		case hostwire.Log:
			var lb hostwire.LogBody
			if err := f.Decode(&lb); err != nil {
				refuse(tc, err.Error())
				return
			}
			s.hostLog(ctx, identity, &logBudget, lb)
		case hostwire.Renew:
			var rb hostwire.RenewBody
			if err := f.Decode(&rb); err != nil {
				refuse(tc, err.Error())
				return
			}
			chainPEM, err := s.Auth.RenewHost(ctx, identity, cert, rb.CSR)
			if err != nil {
				s.Log.Warn("remote worker renewal refused", "worker", name, "err", err)
				c.write(hostwire.Refusal, hostwire.RefusalBody{Detail: "renewal refused: " + err.Error()})
				return
			}
			c.write(hostwire.Renewed, hostwire.ChainBody{Chain: string(chainPEM)})
			return
		default:
			refuse(tc, fmt.Sprintf("a %q frame from a remote worker", f.Type))
			return
		}
	}
}

func (s *Server) dropWorker(name string, c *conn) {
	s.mu.Lock()
	close(c.closed)
	if s.wconns[name] == c {
		delete(s.wconns, name)
		s.wstate[name].Connected = false
	}
	s.mu.Unlock()
	s.reportWorker(name)
}

// reportWorker hands the link's state to OnContact under its identity: the link is an enrolled host in
// the store, listed by `host list` and watched by hostalert, so its contact is recorded like an agent's.
func (s *Server) reportWorker(name string) {
	if s.OnContact == nil {
		return
	}
	s.mu.Lock()
	w := *s.wstate[name]
	s.mu.Unlock()
	s.OnContact(HostState{Host: w.Identity, Connected: w.Connected, Version: w.Version, ConnectedAt: w.ConnectedAt, LastContact: w.LastContact})
}

// WorkerConnected is whether the remote worker has a live connection.
func (s *Server) WorkerConnected(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.wconns[name] != nil
}

// WorkerStates is every remote worker seen since flotillad started.
func (s *Server) WorkerStates() []WorkerState {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]WorkerState, 0, len(s.wstate))
	for _, st := range s.wstate {
		out = append(out, *st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Worker < out[j].Worker })
	return out
}

// RunRemote hands a run to a remote worker and waits for its result, up to the run's timeout and
// RunGrace, across reconnects. ErrWorkerNotConnected: nothing was sent. When the time is up, the link is
// told to cancel the run and the call ends in ErrUnknown: the run may have done its work.
func (s *Server) RunRemote(ctx context.Context, name string, req worker.Request) (*worker.Result, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.initWorkers()
	c := s.wconns[name]
	if c == nil {
		s.mu.Unlock()
		return nil, ErrWorkerNotConnected
	}
	r := &remoteRun{worker: name, ch: make(chan hostwire.RunResultBody, 1)}
	s.runs[req.RunID] = r
	s.mu.Unlock()
	forget := func() {
		s.mu.Lock()
		delete(s.runs, req.RunID)
		s.mu.Unlock()
	}
	if err := c.write(hostwire.Run, hostwire.RunBody{RunID: req.RunID, Request: body}); err != nil {
		forget()
		c.tc.Close()
		return nil, fmt.Errorf("%w (sending the run: %v)", ErrUnknown, err)
	}
	t := time.NewTimer(time.Duration(req.TimeoutS*float64(time.Second)) + RunGrace)
	defer t.Stop()
	select {
	case rb := <-r.ch:
		if rb.Error != "" {
			return nil, fmt.Errorf("the remote worker: %s", rb.Error)
		}
		var res worker.Result
		if err := json.Unmarshal(rb.Result, &res); err != nil {
			return nil, fmt.Errorf("the remote worker's result: %v", err)
		}
		return &res, nil
	case <-t.C:
	case <-ctx.Done():
	}
	forget()
	s.mu.Lock()
	cur := s.wconns[name]
	s.mu.Unlock()
	if cur != nil {
		_ = cur.write(hostwire.Cancel, hostwire.CancelBody{RunID: req.RunID})
	}
	return nil, fmt.Errorf("%w: no result from remote worker %s in time", ErrUnknown, name)
}

// CancelRemote tells the link of worker name to stop run runID. It sends a frame and returns: whether the
// worker took it shows in the run's result.
func (s *Server) CancelRemote(name, runID string) error {
	s.mu.Lock()
	s.initWorkers()
	c := s.wconns[name]
	r := s.runs[runID]
	s.mu.Unlock()
	if r == nil || r.worker != name {
		return fmt.Errorf("remote worker %s: %w", name, worker.ErrNoSuchRun)
	}
	if c == nil {
		return ErrWorkerNotConnected
	}
	if err := c.write(hostwire.Cancel, hostwire.CancelBody{RunID: runID, Stop: true}); err != nil {
		return fmt.Errorf("remote worker %s: sending the cancel: %v", name, err)
	}
	return nil
}

// CancelOrphan stops a run of worker name that flotillad does not wait for: the previous flotillad's, still
// running at the worker (its link keeps it going across the restart). The cancel frame is the one
// CancelRemote sends, without needing the run in this server's table. A worker with no connection yet is
// remembered, and its link is told when it connects (queued). Only in memory: another restart before the
// link connects loses it, and the run then ends by its own timeout.
func (s *Server) CancelOrphan(name, runID string) (queued bool, err error) {
	s.mu.Lock()
	s.initWorkers()
	c := s.wconns[name]
	if c == nil {
		if s.orphans == nil {
			s.orphans = map[string][]string{}
		}
		s.orphans[name] = append(s.orphans[name], runID)
		s.mu.Unlock()
		return true, nil
	}
	s.mu.Unlock()
	if err := c.write(hostwire.Cancel, hostwire.CancelBody{RunID: runID, Stop: true}); err != nil {
		return false, fmt.Errorf("remote worker %s: sending the cancel: %v", name, err)
	}
	return false, nil
}

// RemoteRunner is a remote worker as the scheduler runs it (sched.Runner, and Available for its quiet
// skip when the worker is not connected).
type RemoteRunner struct {
	S      *Server
	Worker string
}

func (r RemoteRunner) Run(ctx context.Context, req worker.Request) (*worker.Result, error) {
	return r.S.RunRemote(ctx, r.Worker, req)
}

// Cancel has the link stop a run in flight on its worker (an operator's cancel, not a timeout): the run's
// own Run call then returns its "cancelled" result over the link as usual. A run this server is not
// waiting for is worker.ErrNoSuchRun; a worker with no link is ErrWorkerNotConnected.
func (r RemoteRunner) Cancel(ctx context.Context, runID string) error {
	return r.S.CancelRemote(r.Worker, runID)
}

func (r RemoteRunner) Available() bool { return r.S.WorkerConnected(r.Worker) }

// CancelOrphan is Server.CancelOrphan for this worker (sched.OrphanCanceller).
func (r RemoteRunner) CancelOrphan(ctx context.Context, runID string) (bool, error) {
	return r.S.CancelOrphan(r.Worker, runID)
}

// cancelOrphans sends the cancels CancelOrphan kept for a worker whose link has just connected.
func (s *Server) cancelOrphans(name string, c *conn, runIDs []string) {
	for _, id := range runIDs {
		if err := c.write(hostwire.Cancel, hostwire.CancelBody{RunID: id, Stop: true}); err != nil {
			s.Log.Error("cancel of an orphaned run: sending", "worker", name, "run_id", id, "err", err)
			continue
		}
		s.Log.Warn("orphaned run cancelled on connect", "worker", name, "run_id", id)
	}
}
