package api

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/chipi/flotilla/internal/casetimer"
	"github.com/chipi/flotilla/internal/hostwire"
	"github.com/chipi/flotilla/internal/store"
)

func hasEvent(t *testing.T, s *Server, id, kind string) bool {
	t.Helper()
	evs, err := s.Store.CaseEvents(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range evs {
		if e.Kind == kind {
			return true
		}
	}
	return false
}

// A watched case's due notice is offered once, with its detail and one answer; marking it sent writes notice.sent and
// the notice row's sent_at; a later notice of the case is a repeat.
func TestSlackNoticesOfAWatchedCase(t *testing.T) {
	ctx := context.Background()
	s := slackServer(t)
	id := watchingCase(t, s, "w1")
	if ns, err := s.SlackNotices(ctx); err != nil || len(ns) != 0 {
		t.Fatalf("a watched case with no notice due was offered: %v %v", ns, err)
	}
	if err := casetimer.Run(ctx, s.Store, time.Now(), casetimer.Config{}, s.Log, nil); err != nil {
		t.Fatal(err)
	}
	ns, err := s.SlackNotices(ctx)
	if err != nil || len(ns) != 1 {
		t.Fatalf("the due notice: %v %v", ns, err)
	}
	n := ns[0]
	if n.Kind != hostwire.KindNotice || n.Case != id || n.Repeat || n.Approvable || n.PlanHash != "" || n.URL != "https://flotilla.example/cases/"+id ||
		len(n.Answers) != 1 || n.Answers[0].ID != "acknowledge" || !n.Answers[0].Allowed ||
		!strings.Contains(n.Plan, "watched: failed_backup on host:w1") || !strings.Contains(n.Plan, "would have run backup.rerun on host:w1") {
		t.Fatalf("the notice: %+v", n)
	}
	if again, _ := s.SlackNotices(ctx); len(again) != 1 || again[0].Nonce != n.Nonce {
		t.Fatal("a second pass made a second ask for the same notice")
	}
	if err := s.Store.MarkAskSent(ctx, n.Nonce); err != nil {
		t.Fatal(err)
	}
	if again, _ := s.SlackNotices(ctx); len(again) != 0 {
		t.Fatalf("a sent notice was offered again: %v", again)
	}
	ns2, _ := s.Store.Notices(ctx, id)
	if len(ns2) != 1 || ns2[0].SentAt == "" || !hasEvent(t, s, id, "notice.sent") {
		t.Fatalf("the notice row after sending: %+v", ns2)
	}

	// the next notice on the schedule is a repeat of the message already shown
	if err := s.Store.DueNotice(ctx, id, store.NoticeWatching, 2, time.Now().Add(time.Second).UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	ns, _ = s.SlackNotices(ctx)
	if len(ns) != 1 || !ns[0].Repeat || ns[0].Nonce == n.Nonce {
		t.Fatalf("the second notice: %+v", ns)
	}
}

// Every rule a relayed acknowledgement must pass, then the acknowledgement itself.
func TestRelayedAcknowledge(t *testing.T) {
	ctx := context.Background()
	s := slackServer(t)
	id := watchingCase(t, s, "w2")
	if err := casetimer.Run(ctx, s.Store, time.Now(), casetimer.Config{}, s.Log, nil); err != nil {
		t.Fatal(err)
	}
	ns, _ := s.SlackNotices(ctx)
	nonce := ns[0].Nonce
	plan := slackCase(t, s, "w2p")
	pns, _ := s.SlackNotices(ctx)
	var planNonce string
	for _, p := range pns {
		if p.Case == plan {
			planNonce = p.Nonce
		}
	}
	ack := hostwire.ApprovalBody{Nonce: nonce, Team: "T0001", User: "U0OPER", Action: "acknowledge"}
	for _, c := range []struct {
		what string
		host string
		b    hostwire.ApprovalBody
		want string
	}{
		{"another host relays", "model-host", ack, "not this installation's Slack connector"},
		{"another workspace", "backup-01", hostwire.ApprovalBody{Nonce: nonce, Team: "T9999", User: "U0OPER", Action: "acknowledge"}, "not the pinned Slack workspace"},
		{"an unpinned user", "backup-01", hostwire.ApprovalBody{Nonce: nonce, Team: "T0001", User: "U0OTHER", Action: "acknowledge"}, "not pinned"},
		{"a viewer", "backup-01", hostwire.ApprovalBody{Nonce: nonce, Team: "T0001", User: "U0VIEW", Action: "acknowledge"}, "not an active operator"},
		{"a nonce flotilla never made", "backup-01", hostwire.ApprovalBody{Nonce: "beef", Team: "T0001", User: "U0OPER", Action: "acknowledge"}, "not made by flotilla"},
		{"an answer the notice did not offer", "backup-01", hostwire.ApprovalBody{Nonce: nonce, Team: "T0001", User: "U0OPER", Action: "approve"}, "unknown action"},
		{"an answer no one offers", "backup-01", hostwire.ApprovalBody{Nonce: nonce, Team: "T0001", User: "U0OPER", Action: "dismiss"}, "unknown action"},
		{"acknowledging with a plan's message", "backup-01", hostwire.ApprovalBody{Nonce: planNonce, Team: "T0001", User: "U0OPER", Action: "acknowledge"}, "unknown action"},
	} {
		if a := s.RelayedAnswer(ctx, c.host, c.b); a.OK || !strings.Contains(a.Text, c.want) {
			t.Errorf("%s: %+v", c.what, a)
		}
	}
	if cs := caseStatus(t, s, id); cs.Status != "watching" || hasEvent(t, s, id, "notice.acked") {
		t.Fatalf("a refused click changed the case: %+v", cs)
	}

	a := s.RelayedAnswer(ctx, "backup-01", ack)
	if !a.OK || !strings.Contains(a.Text, "acknowledged by ada") {
		t.Fatalf("the acknowledgement: %+v", a)
	}
	if cs := caseStatus(t, s, id); cs.Status != "watching" || !hasEvent(t, s, id, "notice.acked") {
		t.Fatalf("after the acknowledgement: %+v", cs)
	}
	if a := s.RelayedAnswer(ctx, "backup-01", ack); a.OK || !strings.Contains(a.Text, "already answered: acknowledged by ada") {
		t.Fatalf("a second click: %+v", a)
	}
}

// A notice acknowledged elsewhere first (the UI) is refused with the core's reason, and the earlier messages of the
// case's notices are all closed by an acknowledgement.
func TestRelayedAcknowledgeSiblingsAndRace(t *testing.T) {
	ctx := context.Background()
	s := slackServer(t)
	id := watchingCase(t, s, "w3")
	if err := casetimer.Run(ctx, s.Store, time.Now(), casetimer.Config{}, s.Log, nil); err != nil {
		t.Fatal(err)
	}
	first, _ := s.SlackNotices(ctx)
	_ = s.Store.MarkAskSent(ctx, first[0].Nonce)
	if err := s.Store.DueNotice(ctx, id, store.NoticeWatching, 2, time.Now().Add(time.Second).UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	second, _ := s.SlackNotices(ctx)
	if a := s.RelayedAnswer(ctx, "backup-01", hostwire.ApprovalBody{Nonce: second[0].Nonce, Team: "T0001", User: "U0OPER", Action: "acknowledge"}); !a.OK {
		t.Fatalf("acknowledging the second notice: %+v", a)
	}
	if a := s.RelayedAnswer(ctx, "backup-01", hostwire.ApprovalBody{Nonce: first[0].Nonce, Team: "T0001", User: "U0OPER", Action: "acknowledge"}); a.OK || !strings.Contains(a.Text, "already answered") {
		t.Fatalf("the first notice's message after the second was acknowledged: %+v", a)
	}

	id2 := watchingCase(t, s, "w4")
	if err := casetimer.Run(ctx, s.Store, time.Now(), casetimer.Config{}, s.Log, nil); err != nil {
		t.Fatal(err)
	}
	var nonce string
	ns, _ := s.SlackNotices(ctx)
	for _, n := range ns {
		if n.Case == id2 {
			nonce = n.Nonce
		}
	}
	if _, err := s.Answer(ctx, opCaller, "ui", id2, "acknowledge", AnswerIn{}); err != nil {
		t.Fatal(err)
	}
	if a := s.RelayedAnswer(ctx, "backup-01", hostwire.ApprovalBody{Nonce: nonce, Team: "T0001", User: "U0OPER", Action: "acknowledge"}); a.OK || !strings.Contains(a.Text, "already answered") {
		t.Fatalf("acknowledging what the UI already acknowledged: %+v", a)
	}
}
