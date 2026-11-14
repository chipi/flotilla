package slackconn

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/chipi/flotilla/internal/hostwire"
)

// fakeSlack is Slack's Web API and a Socket Mode endpoint: it records each call and, once a socket is
// open, sends the click it is given and waits for the ack.
type fakeSlack struct {
	t     *testing.T
	srv   *httptest.Server
	mu    sync.Mutex
	calls []map[string]any
	auth  []string
	click string
	acked chan string
}

func newFake(t *testing.T, click string) *fakeSlack {
	f := &fakeSlack{t: t, click: click, acked: make(chan string, 4)}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/socket" {
			ws, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			defer ws.CloseNow()
			ctx := r.Context()
			_ = ws.Write(ctx, websocket.MessageText, []byte(`{"type":"hello"}`))
			_ = ws.Write(ctx, websocket.MessageText, []byte(f.click))
			_, ack, err := ws.Read(ctx)
			if err == nil {
				f.acked <- string(ack)
			}
			<-ctx.Done()
			return
		}
		method := strings.TrimPrefix(r.URL.Path, "/api/")
		var body map[string]any
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		f.mu.Lock()
		body["_method"] = method
		f.calls = append(f.calls, body)
		f.auth = append(f.auth, method+" "+r.Header.Get("Authorization"))
		f.mu.Unlock()
		switch method {
		case "apps.connections.open":
			_, _ = io.WriteString(w, `{"ok":true,"url":"ws`+strings.TrimPrefix(f.srv.URL, "http")+`/socket"}`)
		case "chat.postMessage":
			_, _ = io.WriteString(w, `{"ok":true,"channel":"C0FLOTILLA","ts":"1700000000.000100"}`)
		case "chat.update":
			_, _ = io.WriteString(w, `{"ok":true}`)
		default:
			_, _ = io.WriteString(w, `{"ok":false,"error":"unknown_method"}`)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeSlack) get(method string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c["_method"] == method {
			return c
		}
	}
	return nil
}

func connector(f *fakeSlack) *Connector {
	c := New("xoxb-bot", "xapp-app", "C0FLOTILLA", slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.API = f.srv.URL + "/api"
	return c
}

func TestPostClickAnswer(t *testing.T) {
	click := `{"envelope_id":"env-1","type":"interactive","payload":{"type":"block_actions","team":{"id":"T0001"},"user":{"id":"U0MARKO"},
		"actions":[{"action_id":"approve","block_id":"flotilla","value":"nonce-1"},{"action_id":"approve","block_id":"someone-else","value":"x"}]}}`
	f := newFake(t, click)
	c := connector(f)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	n := hostwire.NotifyBody{Nonce: "nonce-1", Case: "case-1", Title: "Backup <!channel> late", Plan: "backup.rerun_observability on host:mini",
		PlanHash: "abc123", URL: "https://flotilla.example/cases/case-1", Approvable: true}
	if err := c.Post(ctx, n); err != nil {
		t.Fatal(err)
	}
	post := f.get("chat.postMessage")
	b := plain(post["blocks"])
	if post["channel"] != "C0FLOTILLA" || !strings.Contains(b, `"action_id":"approve"`) || !strings.Contains(b, `"value":"nonce-1"`) ||
		strings.Contains(b, "<!channel>") || !strings.Contains(b, "&lt;!channel&gt;") {
		t.Fatalf("the message: %s", b)
	}

	go c.Run(ctx)
	select {
	case got := <-c.Clicks:
		if got != (hostwire.ApprovalBody{Nonce: "nonce-1", Team: "T0001", User: "U0MARKO", Action: "approve"}) {
			t.Fatalf("the click: %+v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no click came through")
	}
	select {
	case ack := <-f.acked:
		if ack != `{"envelope_id":"env-1"}` {
			t.Fatalf("the ack: %s", ack)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the envelope was not acknowledged")
	}
	select {
	case extra := <-c.Clicks:
		t.Fatalf("a button that is not flotilla's became a click: %+v", extra)
	case <-time.After(200 * time.Millisecond):
	}

	if err := c.Answer(ctx, hostwire.AnsweredBody{Nonce: "nonce-1", OK: true, Text: "approved by marko in Slack; it runs now"}); err != nil {
		t.Fatal(err)
	}
	upd := f.get("chat.update")
	ub := plain(upd["blocks"])
	if upd["ts"] != "1700000000.000100" || strings.Contains(ub, `"type":"actions"`) || !strings.Contains(ub, "approved by marko") {
		t.Fatalf("the update: %v %s", upd["ts"], ub)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, a := range f.auth {
		if strings.HasPrefix(a, "apps.connections.open ") && a != "apps.connections.open Bearer xapp-app" ||
			strings.HasPrefix(a, "chat.") && !strings.HasSuffix(a, "Bearer xoxb-bot") {
			t.Errorf("a call with the wrong token: %s", a)
		}
	}
}

// A plan Slack may not approve is posted without buttons, saying why.
func TestPostNotApprovable(t *testing.T) {
	f := newFake(t, `{}`)
	c := connector(f)
	if err := c.Post(context.Background(), hostwire.NotifyBody{Nonce: "n2", Case: "case-2", Title: "t", Plan: "p", Why: "high risk: answer in the UI or the CLI"}); err != nil {
		t.Fatal(err)
	}
	b := plain(f.get("chat.postMessage")["blocks"])
	if strings.Contains(b, `"type":"actions"`) || !strings.Contains(b, "high risk") {
		t.Fatalf("the message: %s", b)
	}
}

// Slack's ok:false is an error, with Slack's reason.
func TestSlackError(t *testing.T) {
	f := newFake(t, `{}`)
	c := connector(f)
	if err := c.call(context.Background(), "no.such", "xoxb", map[string]any{}, nil); err == nil || !strings.Contains(err.Error(), "unknown_method") {
		t.Fatalf("ok:false: %v", err)
	}
}

// plain is JSON without HTML escaping, so a test reads Slack's own escapes.
func plain(v any) string {
	var b strings.Builder
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	_ = e.Encode(v)
	return b.String()
}
