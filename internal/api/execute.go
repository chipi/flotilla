package api

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/chipi/flotilla/internal/cases"
	"github.com/chipi/flotilla/internal/envelope"
	"github.com/chipi/flotilla/internal/hostexec"
	"github.com/chipi/flotilla/internal/hostlink"
	"github.com/chipi/flotilla/internal/policy"
	"github.com/chipi/flotilla/internal/store"
)

// ADR-0006 §4 and §7: a case policy let run (auto) or the operator approved goes to its host as a signed
// execution, and the host's result moves the case. flotillad signs after the decision, so the envelope
// expires soon; the host decides for itself whether to run it.

// ExecutionValidity is how long a signed execution may wait to be run (ADR-0006 §4: 10 minutes).
const ExecutionValidity = 10 * time.Minute

// NotConnectedLimit is how long a case waits for its host to connect before it escalates unsent,
// unless Executions.NotConnected says otherwise.
const NotConnectedLimit = 10 * time.Minute

// HostSender sends a signed execution down a host's connection (hostlink.Server).
type HostSender interface {
	Execute(ctx context.Context, host, executionID string, envelope []byte, timeout time.Duration) (hostexec.Result, error)
}

// Executions is the sending side: the signing key only flotillad holds, and the connections.
type Executions struct {
	Link  HostSender
	Key   ed25519.PrivateKey
	KeyID string
	// NotConnected overrides NotConnectedLimit (0: the limit)
	NotConnected time.Duration

	wg sync.WaitGroup
}

// Wait waits for the executions under way to come back.
func (x *Executions) Wait() { x.wg.Wait() }

// RecoverExecutions, at start: an execution the last flotillad sent and never heard back from is
// unknown; its case escalates and its target stays held.
func (s *Server) RecoverExecutions(ctx context.Context) error {
	xs, err := s.Store.RecoverHostExecutions(ctx)
	if err != nil {
		return err
	}
	for _, x := range xs {
		s.Log.Error("host execution unknown: flotillad stopped before its result", "case", x.CaseID, "host", x.Host, "execution", x.ID)
		if err := s.Store.MoveCase(ctx, x.CaseID, cases.Escalated, cases.ByAct, "", "flotillad",
			"execution "+x.ID+" on "+x.Host+": unknown (flotillad stopped before its result); the target stays held"); err != nil {
			s.Log.Error("escalating after an unknown execution", "case", x.CaseID, "err", err)
		}
	}
	return nil
}

// SendExecutions sends each executing case on a host, each in its own goroutine (a host action may take
// its whole timeout). A case already sent is not sent again: the store refuses a second record of one
// execution (store.StartHostExecution). It returns how many it started.
func (s *Server) SendExecutions(ctx context.Context) (int, error) {
	if s.Exec == nil || s.Exec.Link == nil || s.Exec.Key == nil {
		return 0, nil
	}
	cs, err := s.Store.Cases(ctx, store.CaseFilter{Status: cases.Executing})
	if err != nil {
		return 0, err
	}
	n := 0
	for _, c := range cs {
		if !strings.HasPrefix(c.Target, "host:") {
			continue
		}
		started, err := s.sendOne(ctx, c)
		if err != nil {
			s.Log.Error("host execution", "case", c.ID, "err", err)
		}
		if started {
			n++
		}
	}
	return n, nil
}

// ExecutionID is a case's execution of one plan: sent once, under this id (the host refuses a second).
func ExecutionID(caseID string, planID int64) string { return fmt.Sprintf("%s-p%d", caseID, planID) }

// sendOne signs and records the case's execution and starts it; false when there is nothing to send.
func (s *Server) sendOne(ctx context.Context, c store.Case) (bool, error) {
	plans, err := s.Store.Plans(ctx, c.ID)
	if err != nil || len(plans) == 0 {
		return false, err
	}
	p := plans[len(plans)-1]
	id := ExecutionID(c.ID, p.ID)
	approved, err := s.approvedBy(ctx, c, p)
	if err != nil {
		return false, err
	}
	host := policy.HostOf(c.Target)
	timeout := 30 * time.Minute
	if s.Registry != nil {
		if a, ok := s.Registry.Action(p.Action); ok && a.Timeout > 0 {
			timeout = time.Duration(a.Timeout)
		}
	}
	now := time.Now().UTC()
	raw, err := envelope.Sign(s.Exec.Key, s.Exec.KeyID, envelope.Execution{ID: id, CaseID: c.ID, Host: host, Action: p.Action,
		Params: p.Params, PlanHash: p.PlanHash, Approved: approved, SignedAt: now, ExpiresAt: now.Add(ExecutionValidity)})
	if err != nil {
		return false, err
	}
	err = s.Store.StartHostExecution(ctx, store.HostExecution{ID: id, CaseID: c.ID, PlanID: p.ID, Host: host, Action: p.Action,
		Approved: approved, Envelope: raw})
	if errors.Is(err, store.ErrExecutionExists) {
		return false, nil // sent already: its result moves the case
	}
	if err != nil {
		return false, err
	}
	s.Exec.wg.Add(1)
	go s.runExecution(c, id, host, raw, timeout)
	return true, nil
}

// approvedBy is the envelope's "how approved": auto when policy let it run, else the operator who moved
// it from awaiting_approval to executing.
func (s *Server) approvedBy(ctx context.Context, c store.Case, p store.PlanRecord) (string, error) {
	if p.Outcome == policy.Auto {
		return "auto", nil
	}
	ev, err := s.Store.CaseEvents(ctx, c.ID)
	if err != nil {
		return "", err
	}
	for i := len(ev) - 1; i >= 0; i-- {
		if ev[i].Kind == "case."+cases.Executing && ev[i].By == cases.ByOperator {
			return ev[i].Actor, nil
		}
	}
	return "", fmt.Errorf("case %s is executing, its plan was %s, and no operator approved it", c.ID, p.Outcome)
}

func (s *Server) runExecution(c store.Case, id, host string, raw []byte, timeout time.Duration) {
	defer s.Exec.wg.Done()
	ctx := context.Background()
	// the host's own timeout runs first; this one only bounds the wait for its answer
	r, err := s.Exec.Link.Execute(ctx, host, id, raw, timeout+time.Minute)
	move := func(to, resolution, detail string) {
		if err := s.Store.MoveCase(ctx, c.ID, to, cases.ByAct, resolution, "flotillad", detail); err != nil {
			s.Log.Error("host execution: moving the case", "case", c.ID, "to", to, "err", err)
		}
	}
	switch {
	case errors.Is(err, hostlink.ErrNotConnected):
		s.Store.EndHostExecution(ctx, id, store.ExecNotSent, 0, "", err.Error(), "")
		limit := NotConnectedLimit
		if s.Exec.NotConnected != 0 {
			limit = s.Exec.NotConnected
		}
		since, _ := time.Parse(time.RFC3339Nano, c.StatusAt)
		if time.Since(since) >= limit {
			s.Log.Error("host execution never sent: the host is not connected", "case", c.ID, "host", host)
			move(cases.Escalated, "", fmt.Sprintf("execution %s never sent: %s not connected for %s", id, host, limit))
		}
		return
	case err != nil:
		s.Store.EndHostExecution(ctx, id, hostexec.Unknown, 0, "", err.Error(), "")
		s.Log.Error("host execution unknown", "case", c.ID, "host", host, "execution", id, "err", err)
		move(cases.Escalated, "", fmt.Sprintf("execution %s on %s: unknown (%v); the target stays held", id, host, err))
		return
	}
	s.Store.EndHostExecution(ctx, id, r.Status, r.ExitCode, r.OutputSHA, r.Detail, r.ChainHead)
	detail := fmt.Sprintf("execution %s on %s: %s", id, host, r.Status)
	if r.Detail != "" {
		detail += ": " + r.Detail
	}
	s.Log.Info("host execution", "case", c.ID, "host", host, "execution", id, "status", r.Status, "exit", r.ExitCode)
	switch r.Status {
	case hostexec.Done:
		move(cases.Checking, "", detail)
	case hostexec.PreconditionFalse:
		move(cases.Closed, cases.ResolvedItself, detail)
	default: // failed, refused, unknown
		move(cases.Escalated, "", detail)
	}
}
