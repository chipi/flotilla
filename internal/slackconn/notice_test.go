package slackconn

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/chipi/flotilla/internal/hostwire"
)

func (f *fakeSlack) all(method string) []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []map[string]any
	for _, c := range f.calls {
		if c["_method"] == method {
			out = append(out, c)
		}
	}
	return out
}

// A notice offers one button per allowed answer and a line for the ones not allowed; a click on any action id under
// flotilla's block is relayed as it came (flotillad, not the connector, says which answers a message offers); the
// answer shows on the message; a repeat of the notice updates it, and a message this connector never posted is posted anew.
func TestNoticeAnswersRepeatAndRestart(t *testing.T) {
	click := `{"envelope_id":"env-2","type":"interactive","payload":{"type":"block_actions","team":{"id":"T0001"},"user":{"id":"U0OPER"},
		"actions":[{"action_id":"acknowledge","block_id":"flotilla","value":"notice-1"},{"action_id":"delete-everything","block_id":"flotilla","value":"notice-1"},
		{"action_id":"acknowledge","block_id":"someone-else","value":"x"},{"action_id":"","block_id":"flotilla","value":"x"}]}}`
	f := newFake(t, click)
	c := connector(f)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	n := hostwire.NotifyBody{Nonce: "notice-1", Case: "case-9", Title: "Backup late", Plan: "watched: failed_backup on host:h, repeated 3 times unchanged",
		URL: "https://flotilla.example/cases/case-9", Kind: hostwire.KindNotice,
		Answers: []hostwire.NotifyAnswer{{ID: "acknowledge", Label: "Acknowledge", Allowed: true}, {ID: "dismiss", Label: "Dismiss", Why: "answer in the UI"}}}
	if err := c.Post(ctx, n); err != nil {
		t.Fatal(err)
	}
	b := plain(f.get("chat.postMessage")["blocks"])
	if !strings.Contains(b, `"action_id":"acknowledge"`) || !strings.Contains(b, `"value":"notice-1"`) || strings.Contains(b, `"action_id":"dismiss"`) ||
		strings.Contains(b, "plan `") || !strings.Contains(b, "Not from Slack: answer in the UI") || strings.Contains(b, `"style"`) {
		t.Fatalf("the notice: %s", b)
	}

	go c.Run(ctx)
	for _, want := range []string{"acknowledge", "delete-everything"} {
		select {
		case got := <-c.Clicks:
			if got != (hostwire.ApprovalBody{Nonce: "notice-1", Team: "T0001", User: "U0OPER", Action: want}) {
				t.Fatalf("the click for %s: %+v", want, got)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("no click for %s", want)
		}
	}
	select {
	case extra := <-c.Clicks:
		t.Fatalf("a button that is not flotilla's became a click: %+v", extra)
	case <-time.After(200 * time.Millisecond):
	}

	if err := c.Answer(ctx, hostwire.AnsweredBody{Nonce: "notice-1", OK: true, Text: "acknowledged by ada in Slack; the case stays watching"}); err != nil {
		t.Fatal(err)
	}
	if ub := plain(f.all("chat.update")[0]["blocks"]); strings.Contains(ub, `"type":"actions"`) || !strings.Contains(ub, "acknowledged by ada") {
		t.Fatalf("the answer on the notice: %s", ub)
	}

	rep := n
	rep.Nonce, rep.Repeat, rep.Plan = "notice-2", true, "watched: failed_backup on host:h, repeated 40 times unchanged"
	if err := c.Post(ctx, rep); err != nil {
		t.Fatal(err)
	}
	ups := f.all("chat.update")
	if len(ups) != 2 || ups[1]["ts"] != "1700000000.000100" || len(f.all("chat.postMessage")) != 1 || !strings.Contains(plain(ups[1]["blocks"]), `"value":"notice-2"`) {
		t.Fatalf("a repeat posted anew instead of updating: %d updates, %d posts", len(ups), len(f.all("chat.postMessage")))
	}
	if err := c.Answer(ctx, hostwire.AnsweredBody{Nonce: "notice-2", OK: true, Text: "acknowledged"}); err != nil || len(f.all("chat.update")) != 3 {
		t.Fatalf("the repeat's own nonce answers its message: %v", err)
	}

	// after a restart the connector knows no message: a repeat is posted anew, and an answer is posted as a message
	c2 := connector(f)
	rep2 := rep
	rep2.Nonce = "notice-3"
	if err := c2.Post(ctx, rep2); err != nil || len(f.all("chat.postMessage")) != 2 {
		t.Fatalf("a repeat after a restart: %v, %d posts", err, len(f.all("chat.postMessage")))
	}
	if err := c2.Answer(ctx, hostwire.AnsweredBody{Nonce: "unknown", OK: false, Text: "refused: already answered"}); err != nil {
		t.Fatal(err)
	}
	posts := f.all("chat.postMessage")
	if len(posts) != 3 || !strings.Contains(plain(posts[2]["blocks"]), "already answered") {
		t.Fatalf("an answer for a message this connector never posted: %d posts", len(posts))
	}
}

// A plan with Slack's answers marked: approve is refused with its reason, reject stays a button; a body with no
// answers (an older flotillad) is drawn from Approvable as before.
func TestPlanAnswerRendering(t *testing.T) {
	b := plain(blocks(hostwire.NotifyBody{Nonce: "n", Case: "c", Title: "t", Plan: "p", Kind: hostwire.KindPlan, Answers: []hostwire.NotifyAnswer{
		{ID: "approve", Label: "Approve", Style: "primary", Why: "high risk: answer in the UI or the CLI"}, {ID: "reject", Label: "Reject", Style: "danger", Allowed: true}}}, ""))
	if strings.Contains(b, `"action_id":"approve"`) || !strings.Contains(b, `"action_id":"reject"`) || !strings.Contains(b, `"style":"danger"`) || !strings.Contains(b, "high risk") {
		t.Fatalf("a plan Slack may not approve: %s", b)
	}
	legacy := plain(blocks(hostwire.NotifyBody{Nonce: "n", Case: "c", Title: "t", Plan: "p", Approvable: true}, ""))
	if !strings.Contains(legacy, `"action_id":"approve"`) || !strings.Contains(legacy, `"action_id":"reject"`) || !strings.Contains(legacy, `"style":"primary"`) {
		t.Fatalf("an older body: %s", legacy)
	}
}
