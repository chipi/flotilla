package api

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chipi/flotilla/internal/cases"
	"github.com/chipi/flotilla/internal/envelope"
	"github.com/chipi/flotilla/internal/hostexec"
	"github.com/chipi/flotilla/internal/hostlink"
	"github.com/chipi/flotilla/internal/policy"
	"github.com/chipi/flotilla/internal/spec"
	"github.com/chipi/flotilla/internal/store"
)

// fakeLink answers each execution with result/err, after release when block is set; it keeps what it
// was sent.
type fakeLink struct {
	mu     sync.Mutex
	sent   [][]byte
	result hostexec.Result
	err    error
	block  chan struct{}
}

func (f *fakeLink) Execute(ctx context.Context, host, id string, env []byte, timeout time.Duration) (hostexec.Result, error) {
	f.mu.Lock()
	f.sent = append(f.sent, env)
	r, err, b := f.result, f.err, f.block
	f.mu.Unlock()
	if b != nil {
		<-b
	}
	r.ExecutionID = id
	return r, err
}

func (f *fakeLink) count() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.sent) }

// executingCase is a case on host:mini whose auto plan put it in executing (or, with approver, a
// proposed plan an operator approved).
func executingCase(t *testing.T, s *Server, key, approver string) string {
	t.Helper()
	ctx := context.Background()
	id := plannedCase(t, s, key, "")
	outcome, to := policy.Auto, cases.Executing
	if approver != "" {
		outcome, to = policy.Propose, cases.AwaitingApproval
	}
	if _, err := s.Store.ApplyPlan(ctx, store.PlanRecord{CaseID: id, Action: rerun, Params: map[string]string{"host": "mini"},
		PlanHash: "hash-" + key, Outcome: outcome, Autonomy: outcome}, to, "", "host:mini", true); err != nil {
		t.Fatal(err)
	}
	if approver != "" {
		if err := s.Store.MoveCase(ctx, id, cases.Executing, cases.ByOperator, "", approver, "approved"); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

func execServer(t *testing.T, link *fakeLink) (*Server, ed25519.PublicKey) {
	t.Helper()
	s := newServer(t, func(*spec.Routine) {})
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	s.Exec = &Executions{Link: link, Key: key, KeyID: "k1"}
	return s, pub
}

func waitStatus(t *testing.T, s *Server, id, status string) store.Case {
	t.Helper()
	for i := 0; i < 200; i++ {
		if c := caseStatus(t, s, id); c.Status == status {
			return c
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("case %s never reached %s: %s", id, status, caseStatus(t, s, id).Status)
	return store.Case{}
}

// A done execution: the envelope is signed with the key, names the case, the host, the plan and how it
// was approved, expires in 10 minutes; the case goes to checking; the record has the result.
func TestExecutionDone(t *testing.T) {
	ctx := context.Background()
	link := &fakeLink{result: hostexec.Result{Status: hostexec.Done, OutputSHA: "abc", ChainHead: "head"}}
	s, pub := execServer(t, link)
	id := executingCase(t, s, "a", "")
	if n, err := s.SendExecutions(ctx); err != nil || n != 1 {
		t.Fatalf("send: %d %v", n, err)
	}
	waitStatus(t, s, id, cases.Checking)
	e, _, err := envelope.Verify(link.sent[0], map[string]ed25519.PublicKey{"k1": pub}, "mini", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if e.CaseID != id || e.Action != rerun || e.Params["host"] != "mini" || e.PlanHash != "hash-a" || e.Approved != "auto" ||
		e.ExpiresAt.Sub(e.SignedAt) != ExecutionValidity {
		t.Fatalf("the envelope: %+v", e)
	}
	x, _ := s.Store.HostExecution(ctx, e.ID)
	if x == nil || x.Status != hostexec.Done || x.OutputSHA256 != "abc" || x.ChainHead != "head" || x.EndedAt == nil {
		t.Fatalf("the record: %+v", x)
	}
	// nothing more to send for it
	if n, _ := s.SendExecutions(ctx); n != 0 || link.count() != 1 {
		t.Fatalf("sent again: %d, %d sends", n, link.count())
	}
}

func TestExecutionOutcomes(t *testing.T) {
	ctx := context.Background()
	for name, c := range map[string]struct {
		r      hostexec.Result
		err    error
		status string
	}{
		"precondition no longer holds": {hostexec.Result{Status: hostexec.PreconditionFalse}, nil, cases.Closed},
		"failed":                       {hostexec.Result{Status: hostexec.Failed, ExitCode: 1}, nil, cases.Escalated},
		"refused by the host":          {hostexec.Result{Status: hostexec.Refused, Detail: "above this host's ceiling"}, nil, cases.Escalated},
		"unknown on the host":          {hostexec.Result{Status: hostexec.Unknown}, nil, cases.Escalated},
		"the connection lost":          {hostexec.Result{}, hostlink.ErrUnknown, cases.Escalated},
	} {
		link := &fakeLink{result: c.r, err: c.err}
		s, _ := execServer(t, link)
		id := executingCase(t, s, "b", "")
		s.SendExecutions(ctx)
		got := waitStatus(t, s, id, c.status)
		if c.status == cases.Closed && got.Resolution != cases.ResolvedItself {
			t.Errorf("%s: closed as %q", name, got.Resolution)
		}
		if c.status == cases.Escalated {
			if h, _ := s.Store.LockHolder(ctx, "host:mini"); h != id {
				t.Errorf("%s: the target is not held after a failed or unknown execution (holder %q)", name, h)
			}
		}
	}
}

// Not connected: nothing reached the host; the case waits and the next pass sends again, under the same
// id with a new envelope; past the limit, it escalates unsent.
func TestNotConnected(t *testing.T) {
	ctx := context.Background()
	link := &fakeLink{err: hostlink.ErrNotConnected}
	s, _ := execServer(t, link)
	id := executingCase(t, s, "c", "")
	s.SendExecutions(ctx)
	s.Exec.Wait()
	if c := caseStatus(t, s, id); c.Status != cases.Executing {
		t.Fatalf("a short wait escalated: %s", c.Status)
	}
	s.SendExecutions(ctx)
	s.Exec.Wait()
	if link.count() != 2 {
		t.Fatalf("%d sends, want 2", link.count())
	}
	s.Exec.NotConnected = time.Nanosecond
	s.SendExecutions(ctx)
	c := waitStatus(t, s, id, cases.Escalated)
	if c.Status != cases.Escalated {
		t.Fatal(c.Status)
	}
	ev, _ := s.Store.CaseEvents(ctx, id)
	if last := ev[len(ev)-1]; !strings.Contains(last.Detail, "never sent") {
		t.Fatalf("the escalation says: %q", last.Detail)
	}
}

// An approved plan carries its approver; a case already on its way is not sent twice.
func TestApprovedByAndInFlight(t *testing.T) {
	ctx := context.Background()
	link := &fakeLink{result: hostexec.Result{Status: hostexec.Done}, block: make(chan struct{})}
	s, pub := execServer(t, link)
	id := executingCase(t, s, "d", "user:ada")
	if n, _ := s.SendExecutions(ctx); n != 1 {
		t.Fatalf("first pass sent %d", n)
	}
	if n, _ := s.SendExecutions(ctx); n != 0 {
		t.Fatalf("a case in flight sent again: %d", n)
	}
	close(link.block)
	waitStatus(t, s, id, cases.Checking)
	e, _, err := envelope.Verify(link.sent[0], map[string]ed25519.PublicKey{"k1": pub}, "mini", time.Now())
	if err != nil || e.Approved != "user:ada" {
		t.Fatalf("approved by: %q %v", e.Approved, err)
	}
}

// After a restart, an execution sent and never answered is unknown; its case escalates.
func TestRecoverExecutions(t *testing.T) {
	ctx := context.Background()
	s, _ := execServer(t, &fakeLink{})
	id := executingCase(t, s, "e", "")
	if err := s.Store.StartHostExecution(ctx, store.HostExecution{ID: id + "-p1", CaseID: id, PlanID: 1, Host: "mini", Action: rerun,
		Approved: "auto", Envelope: []byte("x")}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecoverExecutions(ctx); err != nil {
		t.Fatal(err)
	}
	if c := caseStatus(t, s, id); c.Status != cases.Escalated {
		t.Fatalf("after recovery: %s", c.Status)
	}
	x, _ := s.Store.HostExecution(ctx, id+"-p1")
	if x.Status != hostexec.Unknown {
		t.Fatalf("the record: %s", x.Status)
	}
	if err := s.Store.StartHostExecution(ctx, store.HostExecution{ID: id + "-p1", CaseID: id, Envelope: []byte("y")}); !errors.Is(err, store.ErrExecutionExists) {
		t.Fatalf("an unknown execution sent again: %v", err)
	}
}

// Without the signing key, or for a case not on a host, nothing is sent.
func TestNothingToSend(t *testing.T) {
	ctx := context.Background()
	s := newServer(t, func(*spec.Routine) {})
	executingCase(t, s, "f", "")
	if n, err := s.SendExecutions(ctx); n != 0 || err != nil {
		t.Fatalf("without Exec: %d %v", n, err)
	}
	link := &fakeLink{result: hostexec.Result{Status: hostexec.Done}}
	s2, _ := execServer(t, link)
	k := store.Key{Kind: "target", Value: "failed_backup/service:grafana"}
	id, _, err := s2.Store.OpenCase(ctx, store.NewCase{Kind: "failed_backup", Target: "service:grafana", Keys: []store.Key{k}, Canonical: k}, "x", "")
	if err != nil {
		t.Fatal(err)
	}
	s2.Store.MoveCase(ctx, id, cases.HandedOver, cases.ByDetector, "", "x", "")
	s2.Store.MoveCase(ctx, id, cases.Planned, cases.ByAct, "", "x", "")
	if _, err := s2.Store.ApplyPlan(ctx, store.PlanRecord{CaseID: id, Action: rerun, Params: map[string]string{}, PlanHash: "h",
		Outcome: policy.Auto, Autonomy: policy.Auto}, cases.Executing, "", "service:grafana", true); err != nil {
		t.Fatal(err)
	}
	var logged bytes.Buffer
	s2.Log = slog.New(slog.NewTextHandler(&logged, nil))
	if n, _ := s2.SendExecutions(ctx); n != 0 || link.count() != 0 {
		t.Fatalf("a case not on a host was sent: %d", link.count())
	}
	// not a host's to run: passed over quietly, not an error every pass
	if logged.Len() != 0 {
		t.Fatalf("a case not on a host was logged: %s", logged.String())
	}
}
