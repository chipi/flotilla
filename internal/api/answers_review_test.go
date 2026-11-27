package api

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/chipi/flotilla/internal/cases"
	"github.com/chipi/flotilla/internal/casetimer"
	"github.com/chipi/flotilla/internal/spec"
	"github.com/chipi/flotilla/internal/store"
)

// Two clicks on different asks at cap-1 must not both approve: the cap is a condition of spending the ask.
func TestSlackCapIsAtomicWithTheApproval(t *testing.T) {
	ctx := context.Background()
	s := slackServer(t) // cap 2
	var nonces []string
	for _, k := range []string{"r1", "r2", "r3"} {
		id := slackCase(t, s, k)
		ns, _ := s.SlackNotices(ctx)
		for _, n := range ns {
			if n.Case == id {
				nonces = append(nonces, n.Nonce)
			}
		}
	}
	since := "2000-01-01T00:00:00.000Z"
	if err := s.Store.UseAskCapped(ctx, nonces[0], "user:ada", "approved", since, 2); err != nil {
		t.Fatal(err)
	}
	errs := make(chan error, 2)
	for _, n := range nonces[1:] {
		go func(n string) { errs <- s.Store.UseAskCapped(ctx, n, "user:ada", "approved", since, 2) }(n)
	}
	var ok, capped int
	for i := 0; i < 2; i++ {
		switch err := <-errs; {
		case err == nil:
			ok++
		case errors.Is(err, store.ErrCapReached):
			capped++
		default:
			t.Fatal(err)
		}
	}
	if ok != 1 || capped != 1 {
		t.Fatalf("at cap-1 two clicks: %d approved, %d refused (want 1 and 1)", ok, capped)
	}
	if n, _ := s.Store.SlackApprovalsSince(ctx, since); n != 2 {
		t.Fatalf("approvals counted: %d", n)
	}
}

// A re-watched case whose new period has no notice row yet is a 409, never an ack of an earlier period's notice.
func TestAckIsScopedToTheWatchingPeriod(t *testing.T) {
	ctx := context.Background()
	s := newServer(t, func(*spec.Routine) {})
	id := watchingCase(t, s, "p1")
	if err := casetimer.Run(ctx, s.Store, time.Now(), casetimer.Config{}, s.Log, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Answer(ctx, opCaller, "api", id, "acknowledge", AnswerIn{}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	if err := s.Store.MoveCase(ctx, id, cases.HandedOver, cases.ByOperator, "", "operator", "again"); err != nil {
		t.Fatal(err)
	}
	_ = s.Store.MoveCase(ctx, id, cases.Planned, cases.ByAct, "", "a", "")
	if _, err := s.Store.ApplyPlan(ctx, store.PlanRecord{CaseID: id, RunID: "r2", Action: rerun, Params: map[string]string{"host": "p1"},
		PlanHash: "hash-p1-second-0123", Outcome: "record", Autonomy: "record"}, cases.Watching, "", "host:p1", false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Answer(ctx, opCaller, "api", id, "acknowledge", AnswerIn{}); err == nil || AsProblem(err).Code != 409 {
		t.Fatalf("acking the new period against the old period's notice: %v", err)
	}
	// the store refuses on its own too: a case that is not watching has nothing to ack
	esc := escalatedCase(t, s, "p2")
	if _, err := s.Store.AckNotice(ctx, esc, "user:x"); !errors.Is(err, store.ErrNotWatching) {
		t.Fatalf("the store acked a case that is not watching: %v", err)
	}
}

// The directory's flotilla id is a bare user name, as the pins and the users table have them: a "user:" prefix is the
// ledger's actor form and not another person, but another spelling (case) is.
func TestSlackActorNamespace(t *testing.T) {
	ctx := context.Background()
	s := slackServer(t)
	for _, c := range []struct {
		in      string
		refused bool
	}{{"ada", false}, {"user:ada", false}, {"Ada", true}, {"USER:ADA", true}, {"vic", true}} {
		if _, err := s.Store.SyncPeople(ctx, []store.Person{{Email: "x@example.org", Name: "X", IDs: map[string]string{"slack": "U0OPER", "flotilla": c.in}}}, "directory:file"); err != nil {
			t.Fatal(err)
		}
		got, err := s.resolveSlackActor(ctx, "U0OPER", "ada")
		if (err != nil) != c.refused || (err == nil && got != "ada") {
			t.Errorf("directory id %q: %q %v (refused want %v)", c.in, got, err, c.refused)
		}
	}
}

// A malformed body is a 400, and the HTTP note cannot imitate the Slack connector's.
func TestAnswerBodyIsChecked(t *testing.T) {
	ctx := context.Background()
	s := newServer(t, func(*spec.Routine) {})
	hts := httptest.NewServer(s.Handler())
	t.Cleanup(hts.Close)
	id := slackCase(t, s, "b1")
	if c, _ := call(t, hts, "POST", "/v1/cases/"+id+"/answers", opTok, `{"answer":`); c != 400 {
		t.Fatalf("a malformed body: %d", c)
	}
	if c, _ := call(t, hts, "POST", "/v1/cases/"+id+"/answers", opTok, `{"answer":"approve","note":"from Slack"}`); c != 200 {
		t.Fatalf("approve with a note: %d", c)
	}
	ev, _ := s.Store.CaseEvents(ctx, id)
	if d := ev[len(ev)-1].Detail; !strings.Contains(d, ": via api: from Slack") {
		t.Fatalf("the note in the ledger: %q", d)
	}
}
