package hostagent

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chipi/flotilla/internal/api"
	"github.com/chipi/flotilla/internal/cases"
	"github.com/chipi/flotilla/internal/hostaudit"
	"github.com/chipi/flotilla/internal/hostcfg"
	"github.com/chipi/flotilla/internal/hostexec"
	"github.com/chipi/flotilla/internal/policy"
	"github.com/chipi/flotilla/internal/store"
)

// realHost is the host side as flotilla-host runs it: its own config (accepting key k1, one harmless
// action, ceiling as given), the executions store, the audit log, the real runner.
func realHost(t *testing.T, pub ed25519.PublicKey, ceiling string) (*hostexec.Executor, string) {
	t.Helper()
	dir := t.TempDir()
	cfg, err := hostcfg.Parse([]byte(fmt.Sprintf(`{"host": "mini", "user": "_flotilla", "ceiling": %q,
	  "signing_keys": {"k1": %q},
	  "actions": {"host.true": {"argv": ["/bin/true"], "params": {"host": {"kind": "this_host"}}, "per_hour": 5, "timeout": "10s"}}}`,
		ceiling, base64.StdEncoding.EncodeToString(pub))))
	if err != nil {
		t.Fatal(err)
	}
	db, _, err := hostexec.Open(filepath.Join(dir, "executions.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	audit, err := hostaudit.Open(filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { audit.Close() })
	return hostexec.New(db, audit, cfg, hostexec.ExecRunner{}, hostexec.LocalChecker{Run: hostexec.ExecRunner{}}), filepath.Join(dir, "audit.jsonl")
}

// decidedCase puts a case on host:mini through planned to executing, as policy (auto) or an operator
// (approver) would.
func decidedCase(t *testing.T, st *store.Store, approver string) string {
	t.Helper()
	ctx := context.Background()
	k := store.Key{Kind: "target", Value: "x/host:mini/" + approver}
	id, _, err := st.OpenCase(ctx, store.NewCase{Kind: "test_kind", Target: "host:mini", Keys: []store.Key{k}, Canonical: k}, "test", "")
	if err != nil {
		t.Fatal(err)
	}
	st.MoveCase(ctx, id, cases.HandedOver, cases.ByDetector, "", "test", "")
	st.MoveCase(ctx, id, cases.Planned, cases.ByAct, "", "test", "")
	outcome, to := policy.Auto, cases.Executing
	if approver != "" {
		outcome, to = policy.Propose, cases.AwaitingApproval
	}
	if _, err := st.ApplyPlan(ctx, store.PlanRecord{CaseID: id, Action: "host.true", Params: map[string]string{"host": "mini"},
		PlanHash: "h", Outcome: outcome, Autonomy: outcome}, to, "", "host:mini", true); err != nil {
		t.Fatal(err)
	}
	if approver != "" {
		if err := st.MoveCase(ctx, id, cases.Executing, cases.ByOperator, "", approver, "approved"); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

func status(st *store.Store, id string) string {
	c, _ := st.Case(context.Background(), id)
	return c.Status
}

// flotillad signs a decided case's execution and sends it over the real connection; the real executor
// on the host verifies it against its own config, runs it, logs it, and the result moves the case.
func TestRealExecution(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	w.api.Exec = &api.Executions{Link: w.link, Key: key, KeyID: "k1"}
	x, auditPath := realHost(t, pub, "propose")
	run(t, w.agent(t, "mini", x))
	waitFor(t, "the agent to connect", connected(w, "mini"))

	// approved by an operator, within the host's ceiling (propose): runs; the case goes to checking
	id := decidedCase(t, w.api.Store, "user:ada")
	if n, err := w.api.SendExecutions(ctx); n != 1 || err != nil {
		t.Fatalf("send: %d %v", n, err)
	}
	w.api.Exec.Wait()
	if s := status(w.api.Store, id); s != cases.Checking {
		r, _ := w.api.Store.HostExecution(ctx, id+"-p1")
		t.Fatalf("approved: the case is %s; the execution: %+v", s, r)
	}
	rec, _ := w.api.Store.HostExecution(ctx, id+"-p1")
	b, _ := os.ReadFile(auditPath)
	if rec == nil || rec.Status != hostexec.Done || rec.Approved != "user:ada" || rec.ChainHead == "" {
		t.Fatalf("flotillad's record: %+v", rec)
	}
	if !strings.Contains(string(b), `"event":"started"`) || !strings.Contains(string(b), `"approved":"user:ada"`) {
		t.Fatalf("the host's audit log:\n%s", b)
	}
	if rec.ChainHead != x.ChainHead() {
		t.Fatalf("flotillad's chain head %s, the host's %s", rec.ChainHead, x.ChainHead())
	}
	// the audit (`flotilla host audit`): the host's real log against flotillad's real record
	xs, _ := w.api.Store.HostExecutions(ctx, "mini")
	var recs []hostaudit.Record
	for _, e := range xs {
		recs = append(recs, hostaudit.Record{ID: e.ID, Status: e.Status})
	}
	hs, _ := w.api.Store.Hosts(ctx)
	if fs := hostaudit.Compare(b, recs, hs[0].ChainHead); hostaudit.Failed(fs) || len(recs) != 1 {
		t.Fatalf("the audit after a real execution: %d records, %v", len(recs), fs)
	}
}

// auto, above this host's ceiling (propose): the host refuses whatever flotillad decided; the case
// escalates.
func TestHostCeilingWins(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	w.api.Exec = &api.Executions{Link: w.link, Key: key, KeyID: "k1"}
	x, _ := realHost(t, pub, "propose")
	run(t, w.agent(t, "mini", x))
	waitFor(t, "the agent to connect", connected(w, "mini"))
	id := decidedCase(t, w.api.Store, "")
	w.api.SendExecutions(ctx)
	w.api.Exec.Wait()
	if s := status(w.api.Store, id); s != cases.Escalated {
		t.Fatalf("auto above the ceiling: the case is %s", s)
	}
	rec, _ := w.api.Store.HostExecution(ctx, id+"-p1")
	if rec == nil || rec.Status != hostexec.Refused || !strings.Contains(rec.Detail, "ceiling") {
		t.Fatalf("the refusal: %+v", rec)
	}
}

// A key the host does not accept: refused there, nothing runs.
func TestUnacceptedKey(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	otherPub, _, _ := ed25519.GenerateKey(rand.Reader)
	w.api.Exec = &api.Executions{Link: w.link, Key: key, KeyID: "k1"}
	x, _ := realHost(t, otherPub, "propose")
	run(t, w.agent(t, "mini", x))
	waitFor(t, "the agent to connect", connected(w, "mini"))
	id := decidedCase(t, w.api.Store, "user:ada")
	w.api.SendExecutions(ctx)
	w.api.Exec.Wait()
	rec, _ := w.api.Store.HostExecution(ctx, id+"-p1")
	if s := status(w.api.Store, id); s != cases.Escalated || rec == nil || rec.Status != hostexec.Refused {
		t.Fatalf("a key the host does not accept: case %s, record %+v", s, rec)
	}

}
