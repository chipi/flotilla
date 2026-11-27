package hostagent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/coder/websocket"

	"github.com/chipi/flotilla/internal/hostlink"
	"github.com/chipi/flotilla/internal/hostwire"
	"github.com/chipi/flotilla/internal/slackconn"
)

// The connector role end to end over the real link: flotillad's notice reaches the agent, which posts it;
// a click in Slack is relayed to flotillad, whose answer the agent shows on the message.
func TestSlackConnectorOverTheLink(t *testing.T) {
	var mu sync.Mutex
	var methods []string
	var update string
	sendClick := make(chan struct{})
	slack := httptest.NewServer(nil)
	slack.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/socket" {
			ws, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			defer ws.CloseNow()
			<-sendClick
			_ = ws.Write(r.Context(), websocket.MessageText, []byte(`{"envelope_id":"e1","type":"interactive","payload":{"type":"block_actions",
				"team":{"id":"T0001"},"user":{"id":"U0OPER"},"actions":[{"action_id":"approve","block_id":"flotilla","value":"n1"}]}}`))
			_, _, _ = ws.Read(r.Context())
			<-r.Context().Done()
			return
		}
		m := strings.TrimPrefix(r.URL.Path, "/api/")
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		methods = append(methods, m)
		if m == "chat.update" {
			update = string(raw)
		}
		mu.Unlock()
		switch m {
		case "apps.connections.open":
			_, _ = io.WriteString(w, `{"ok":true,"url":"ws`+strings.TrimPrefix(slack.URL, "http")+`/socket"}`)
		case "chat.postMessage":
			_, _ = io.WriteString(w, `{"ok":true,"channel":"C0FLOTILLA","ts":"1.1"}`)
		default:
			_, _ = io.WriteString(w, `{"ok":true}`)
		}
	})
	t.Cleanup(slack.Close)

	relayed := make(chan hostwire.ApprovalBody, 1)
	w := newWorld(t, func(l *hostlink.Server) {
		l.OnApproval = func(_ context.Context, host string, b hostwire.ApprovalBody) hostwire.AnsweredBody {
			if host != "host-01" {
				t.Errorf("relayed by %s", host)
			}
			relayed <- b
			return hostwire.AnsweredBody{Nonce: b.Nonce, OK: true, Text: "approved by ada in Slack; it runs now"}
		}
	})
	a := w.agent(t, "host-01", &fakeExec{})
	a.Slack = slackconn.New("xoxb-bot", "xapp-app", "C0FLOTILLA", quiet)
	a.Slack.API = slack.URL + "/api"
	run(t, a)
	waitFor(t, "the agent to connect", connected(w, "host-01"))
	if err := w.link.Notify("host-01", hostwire.NotifyBody{Nonce: "n1", Case: "case-1", Title: "Backup late", Plan: "rerun on host:host-01",
		PlanHash: "abc", URL: "https://f/cases/case-1", Approvable: true, Kind: hostwire.KindPlan,
		Answers: []hostwire.NotifyAnswer{{ID: "approve", Label: "Approve", Style: "primary", Allowed: true}, {ID: "reject", Label: "Reject", Style: "danger", Allowed: true}}}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the plan posted in Slack", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return strings.Contains(strings.Join(methods, " "), "chat.postMessage")
	})
	close(sendClick)
	b := <-relayed
	if b != (hostwire.ApprovalBody{Nonce: "n1", Team: "T0001", User: "U0OPER", Action: "approve"}) {
		t.Fatalf("relayed: %+v", b)
	}
	waitFor(t, "the answer on the message", func() bool {
		mu.Lock()
		defer mu.Unlock()
		var u map[string]any
		_ = json.Unmarshal([]byte(update), &u)
		return u["ts"] == "1.1" && strings.Contains(update, "approved by ada")
	})
	// a notice frame: posted as its own message, then a repeat of it updates that message
	notice := hostwire.NotifyBody{Nonce: "n3", Case: "case-3", Title: "Backup watched", Plan: "watched: failed_backup on host:host-01", URL: "https://f/cases/case-3",
		Kind: hostwire.KindNotice, Answers: []hostwire.NotifyAnswer{{ID: "acknowledge", Label: "Acknowledge", Allowed: true}}}
	count := func(m string) int {
		mu.Lock()
		defer mu.Unlock()
		return strings.Count(" "+strings.Join(methods, " "), " "+m)
	}
	if err := w.link.Notify("host-01", notice); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the notice posted in Slack", func() bool { return count("chat.postMessage") == 2 })
	notice.Nonce, notice.Repeat = "n4", true
	updates := count("chat.update")
	if err := w.link.Notify("host-01", notice); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the repeat updating the notice", func() bool { return count("chat.update") == updates+1 && count("chat.postMessage") == 2 })
	if err := w.link.Notify("model-host", hostwire.NotifyBody{Nonce: "n2"}); err == nil {
		t.Error("a notice to a host that is not connected was accepted")
	}
}
