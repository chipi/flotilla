// Package slackconn is the host agent's Slack connector (ADR-0006 §1, decided 4; G9): it posts the plans
// flotillad sends it in one channel, with Approve and Reject buttons where Slack may answer, takes the
// clicks over Socket Mode (an outbound websocket: no host opens a port), acknowledges them at once and
// queues them for flotillad, which decides everything again; then it shows flotillad's answer on the
// message. It holds the Slack tokens, which flotillad never sees. Web API calls are plain HTTPS.
package slackconn

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/chipi/flotilla/internal/hostwire"
)

// DefaultAPI is Slack's Web API.
const DefaultAPI = "https://slack.com/api"

type Connector struct {
	BotToken, AppToken string
	Channel            string
	API                string // DefaultAPI unless a test serves one
	HTTP               *http.Client
	Log                *slog.Logger
	// Clicks are the answers to send to flotillad, in the order made; the agent's session drains it.
	Clicks chan hostwire.ApprovalBody

	mu     sync.Mutex
	posted map[string]posted // nonce -> the message
	byCase map[string]string // case -> the nonce of the message last shown for it
}

type posted struct {
	channel, ts string
	n           hostwire.NotifyBody
}

func New(bot, app, channel string, log *slog.Logger) *Connector {
	return &Connector{BotToken: bot, AppToken: app, Channel: channel, API: DefaultAPI, Log: log,
		HTTP: &http.Client{Timeout: 20 * time.Second}, Clicks: make(chan hostwire.ApprovalBody, 64), posted: map[string]posted{}, byCase: map[string]string{}}
}

// call is one Web API method with a token; Slack answers 200 with ok false on an error.
func (c *Connector) call(ctx context.Context, method, token string, body any, out any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.API, "/")+"/"+method, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("slack %s: %w", method, err)
	}
	defer resp.Body.Close()
	var r struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	raw := new(bytes.Buffer)
	if _, err := raw.ReadFrom(http.MaxBytesReader(nil, resp.Body, 1<<20)); err != nil {
		return fmt.Errorf("slack %s: %w", method, err)
	}
	if err := json.Unmarshal(raw.Bytes(), &r); err != nil {
		return fmt.Errorf("slack %s: HTTP %d, not JSON", method, resp.StatusCode)
	}
	if !r.OK {
		return fmt.Errorf("slack %s: %s", method, r.Error)
	}
	if out != nil {
		return json.Unmarshal(raw.Bytes(), out)
	}
	return nil
}

// answers is what a message offers: the body's own list, or for a body from before answers (a plan) approve and reject
// by whether Slack may approve it.
func answers(n hostwire.NotifyBody) []hostwire.NotifyAnswer {
	if len(n.Answers) > 0 {
		return n.Answers
	}
	return []hostwire.NotifyAnswer{
		{ID: "approve", Label: "Approve", Style: "primary", Allowed: n.Approvable, Why: n.Why},
		{ID: "reject", Label: "Reject", Style: "danger", Allowed: n.Approvable, Why: n.Why}}
}

func blocks(n hostwire.NotifyBody, answer string) []map[string]any {
	text := fmt.Sprintf("*%s*\n%s · plan `%s`\n<%s|Open the case %s>", esc(n.Title), esc(n.Plan), n.PlanHash, n.URL, n.Case)
	if n.Kind == hostwire.KindNotice {
		text = fmt.Sprintf("*%s*\n%s\n<%s|Open the case %s>", esc(n.Title), esc(n.Plan), n.URL, n.Case)
	}
	out := []map[string]any{{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": text}}}
	if answer != "" {
		return append(out, map[string]any{"type": "context", "elements": []map[string]any{{"type": "mrkdwn", "text": esc(answer)}}})
	}
	var buttons []map[string]any
	var refused []string
	for _, a := range answers(n) {
		if !a.Allowed {
			refused = append(refused, a.Why)
			continue
		}
		b := map[string]any{"type": "button", "action_id": a.ID, "value": n.Nonce, "text": map[string]any{"type": "plain_text", "text": a.Label}}
		if a.Style == "primary" || a.Style == "danger" {
			b["style"] = a.Style
		}
		buttons = append(buttons, b)
	}
	if len(buttons) > 0 {
		out = append(out, map[string]any{"type": "actions", "block_id": "flotilla", "elements": buttons})
	}
	if len(refused) > 0 {
		out = append(out, map[string]any{"type": "context", "elements": []map[string]any{{"type": "mrkdwn", "text": "Not from Slack: " + esc(strings.Join(dedupe(refused), "; "))}}})
	}
	return out
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// esc keeps Slack from reading a case's text as a mention or a link.
func esc(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// Post puts a question in the channel. A repeat of a case's notice updates the message the case already has; when this
// connector does not know it (it restarted), the notice is posted anew.
func (c *Connector) Post(ctx context.Context, n hostwire.NotifyBody) error {
	text := "flotilla: " + n.Title + " waits for you"
	if n.Kind == hostwire.KindNotice {
		text = "flotilla: " + n.Title + " is still watched"
	}
	if n.Repeat {
		c.mu.Lock()
		p, ok := c.posted[c.byCase[n.Case]]
		c.mu.Unlock()
		if ok {
			if err := c.call(ctx, "chat.update", c.BotToken, map[string]any{"channel": p.channel, "ts": p.ts, "text": text, "blocks": blocks(n, "")}, nil); err != nil {
				return err
			}
			c.remember(n, p.channel, p.ts)
			return nil
		}
	}
	var r struct {
		TS      string `json:"ts"`
		Channel string `json:"channel"`
	}
	if err := c.call(ctx, "chat.postMessage", c.BotToken, map[string]any{"channel": c.Channel,
		"text": text, "blocks": blocks(n, ""), "unfurl_links": false}, &r); err != nil {
		return err
	}
	c.remember(n, r.Channel, r.TS)
	return nil
}

func (c *Connector) remember(n hostwire.NotifyBody, channel, ts string) {
	c.mu.Lock()
	c.posted[n.Nonce] = posted{channel: channel, ts: ts, n: n}
	if n.Kind == hostwire.KindNotice { // only a notice's message may be updated by its case's next notice
		c.byCase[n.Case] = n.Nonce
	}
	c.mu.Unlock()
}

// Answer shows flotillad's answer on the message and takes its buttons away. A refusal is only logged: shown on a notice it
// would strip the buttons from the message a newer nonce shares. When the message is not known (the connector restarted
// since it posted), an answer that went through is posted as a message of its own.
func (c *Connector) Answer(ctx context.Context, a hostwire.AnsweredBody) error {
	c.mu.Lock()
	p, ok := c.posted[a.Nonce]
	if ok && a.OK && c.byCase[p.n.Case] == a.Nonce {
		delete(c.byCase, p.n.Case) // answered: the case's next notice is a message of its own
	}
	c.mu.Unlock()
	if ok && !a.OK && p.n.Kind == hostwire.KindNotice {
		c.Log.Warn("slack: a click on a notice was refused", "case", p.n.Case, "text", a.Text)
		return nil
	}
	if !ok && !a.OK {
		c.Log.Warn("slack: a click was refused for a message this connector did not post", "text", a.Text)
		return nil
	}
	if !ok {
		return c.call(ctx, "chat.postMessage", c.BotToken, map[string]any{"channel": c.Channel, "text": "flotilla: " + a.Text, "unfurl_links": false,
			"blocks": []map[string]any{{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": esc(a.Text)}}}}, nil)
	}
	return c.call(ctx, "chat.update", c.BotToken, map[string]any{"channel": p.channel, "ts": p.ts,
		"text": "flotilla: " + p.n.Title + ": " + a.Text, "blocks": blocks(p.n, a.Text)}, nil)
}

// envelope is a Socket Mode message.
type envelope struct {
	EnvelopeID string          `json:"envelope_id"`
	Type       string          `json:"type"`
	Payload    json.RawMessage `json:"payload"`
}

type blockActions struct {
	Type string `json:"type"`
	Team struct {
		ID string `json:"id"`
	} `json:"team"`
	User struct {
		ID string `json:"id"`
	} `json:"user"`
	Actions []struct {
		ActionID string `json:"action_id"`
		BlockID  string `json:"block_id"`
		Value    string `json:"value"`
	} `json:"actions"`
}

// Run keeps a Socket Mode connection until ctx ends, reconnecting with a backoff.
func (c *Connector) Run(ctx context.Context) {
	wait := time.Second
	for ctx.Err() == nil {
		err := c.socket(ctx)
		if ctx.Err() != nil {
			return
		}
		c.Log.Warn("slack socket closed", "err", err, "retry_in", wait)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		if wait < time.Minute {
			wait *= 2
		}
	}
}

func (c *Connector) socket(ctx context.Context) error {
	var r struct {
		URL string `json:"url"`
	}
	if err := c.call(ctx, "apps.connections.open", c.AppToken, map[string]any{}, &r); err != nil {
		return err
	}
	ws, _, err := websocket.Dial(ctx, r.URL, &websocket.DialOptions{HTTPClient: c.HTTP})
	if err != nil {
		return err
	}
	defer ws.CloseNow()
	ws.SetReadLimit(1 << 20)
	c.Log.Info("slack socket connected")
	for {
		_, raw, err := ws.Read(ctx)
		if err != nil {
			return err
		}
		var e envelope
		if err := json.Unmarshal(raw, &e); err != nil {
			continue
		}
		if e.EnvelopeID != "" {
			ack, _ := json.Marshal(map[string]string{"envelope_id": e.EnvelopeID})
			if err := ws.Write(ctx, websocket.MessageText, ack); err != nil {
				return err
			}
		}
		switch e.Type {
		case "disconnect":
			return errors.New("slack asked to reconnect")
		case "interactive":
			c.click(e.Payload)
		}
	}
}

func (c *Connector) click(raw json.RawMessage) {
	var p blockActions
	if json.Unmarshal(raw, &p) != nil || p.Type != "block_actions" {
		return
	}
	for _, a := range p.Actions {
		if a.BlockID != "flotilla" || a.ActionID == "" {
			continue
		}
		b := hostwire.ApprovalBody{Nonce: a.Value, Team: p.Team.ID, User: p.User.ID, Action: a.ActionID}
		select {
		case c.Clicks <- b:
			c.Log.Info("slack click", "action", a.ActionID, "slack_user", p.User.ID)
		default:
			c.Log.Warn("slack click dropped: the queue to flotillad is full", "slack_user", p.User.ID)
		}
	}
}
