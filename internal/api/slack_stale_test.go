package api

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/chipi/flotilla/internal/cases"
	"github.com/chipi/flotilla/internal/casetimer"
	"github.com/chipi/flotilla/internal/hostwire"
	"github.com/chipi/flotilla/internal/store"
)

func click(s *Server, nonce string) hostwire.AnsweredBody {
	return s.RelayedAnswer(context.Background(), "backup-01", hostwire.ApprovalBody{Nonce: nonce, Team: "T0001", User: "U0OPER", Action: "acknowledge"})
}

func dueIn(d time.Duration) string { return time.Now().Add(d).UTC().Format(time.RFC3339Nano) }

func firstNotice(t *testing.T, s *Server, id string) hostwire.NotifyBody {
	t.Helper()
	ctx := context.Background()
	if err := casetimer.Run(ctx, s.Store, time.Now(), casetimer.Config{}, s.Log, nil); err != nil {
		t.Fatal(err)
	}
	ns, _ := s.SlackNotices(ctx)
	for _, n := range ns {
		if n.Case == id {
			_ = s.Store.MarkAskSent(ctx, n.Nonce)
			return n
		}
	}
	t.Fatal("no notice for " + id)
	return hostwire.NotifyBody{}
}

// A message of an earlier notice must not acknowledge a later one its reader has not seen: the second notice is due
// and its own message is not out yet, so the first message's nonce is still open.
func TestStaleNoticeNonceCannotAcknowledgeANewerNotice(t *testing.T) {
	ctx := context.Background()
	s := slackServer(t)
	id := watchingCase(t, s, "s1")
	first := firstNotice(t, s, id)
	if err := s.Store.DueNotice(ctx, id, store.NoticeWatching, 2, dueIn(time.Second)); err != nil {
		t.Fatal(err)
	}
	if a := click(s, first.Nonce); a.OK || !strings.Contains(a.Text, "superseded") {
		t.Fatalf("the first notice's message after a second fell due: %+v", a)
	}
	if hasEvent(t, s, id, "notice.acked") {
		t.Fatal("the stale click acknowledged a notice")
	}
}

// The second notice's ask closes the first's: a repeat's old nonce is refused.
func TestRepeatClosesTheEarlierNoticeAsk(t *testing.T) {
	ctx := context.Background()
	s := slackServer(t)
	id := watchingCase(t, s, "s2")
	first := firstNotice(t, s, id)
	_ = s.Store.DueNotice(ctx, id, store.NoticeWatching, 2, dueIn(time.Second))
	if second, _ := s.SlackNotices(ctx); len(second) != 1 || !second[0].Repeat {
		t.Fatalf("the second notice: %+v", second)
	}
	if a := click(s, first.Nonce); a.OK || !strings.Contains(a.Text, "already answered") {
		t.Fatalf("the earlier notice's nonce after its repeat: %+v", a)
	}
}

// An acknowledgement in the UI and a handing back each spend the case's open notice asks.
func TestLeavingWatchingOrAckingElsewhereClosesNoticeAsks(t *testing.T) {
	ctx := context.Background()
	s := slackServer(t)
	ui := watchingCase(t, s, "s3")
	back := watchingCase(t, s, "s4")
	if err := casetimer.Run(ctx, s.Store, time.Now(), casetimer.Config{}, s.Log, nil); err != nil {
		t.Fatal(err)
	}
	nonces := map[string]string{}
	ns, _ := s.SlackNotices(ctx)
	for _, n := range ns {
		nonces[n.Case] = n.Nonce
		_ = s.Store.MarkAskSent(ctx, n.Nonce)
	}
	if _, err := s.Answer(ctx, opCaller, "ui", ui, "acknowledge", AnswerIn{}); err != nil {
		t.Fatal(err)
	}
	if a := click(s, nonces[ui]); a.OK || !strings.Contains(a.Text, "already answered") {
		t.Fatalf("a Slack click after the UI acknowledged: %+v", a)
	}
	if err := s.Store.MoveCase(ctx, back, cases.HandedOver, cases.ByOperator, "", "operator", "again"); err != nil {
		t.Fatal(err)
	}
	_ = s.Store.MoveCase(ctx, back, cases.Planned, cases.ByAct, "", "a", "")
	if _, err := s.Store.ApplyPlan(ctx, store.PlanRecord{CaseID: back, RunID: "r2", Action: rerun, Params: map[string]string{"host": "s4"},
		PlanHash: "hash-s4-second-0123", Outcome: "record", Autonomy: "record"}, cases.Watching, "", "host:s4", false); err != nil {
		t.Fatal(err)
	}
	if a := click(s, nonces[back]); a.OK || !strings.Contains(a.Text, "already answered") {
		t.Fatalf("the old period's nonce on a re-watched case: %+v", a)
	}
}

// A refusal that cannot change (the case left watching behind the click) leaves the ask spent.
func TestRefusedAcknowledgeLeavesTheAskSpent(t *testing.T) {
	ctx := context.Background()
	s := slackServer(t)
	id := watchingCase(t, s, "s5")
	first := firstNotice(t, s, id)
	if _, err := s.Store.AckNotice(ctx, id, "user:ada"); err != nil {
		t.Fatal(err)
	}
	if err := s.Store.ReopenAsk(ctx, first.Nonce); err != nil { // as if the ask had been open when the notice was acknowledged
		t.Fatal(err)
	}
	if a := click(s, first.Nonce); a.OK {
		t.Fatalf("a click on an acknowledged notice: %+v", a)
	}
	if ask, _ := s.Store.SlackAskByNonce(ctx, first.Nonce); ask.DecidedAt == "" {
		t.Fatal("the ask was reopened after a refusal that cannot change")
	}
}
