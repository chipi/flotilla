// Package hostwire is the frame format between a host agent and flotillad (ADR-0006 §2): over the
// agent's one outbound TLS connection, each frame is a 4-byte big-endian length and that many bytes of
// JSON, {"type": ..., "body": ...}. No dependency (ADR-0006 named a websocket library only if one is
// used).
package hostwire

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

// MaxFrame bounds a frame: an execution's envelope fits many times over; so does a run's result with its 16 KiB of
// output and up to 5000 recorded model calls (worker.MaxResultBytes). A peer announcing more is cut off before
// anything is read.
const MaxFrame = 16 << 20

// Frame types.
const (
	// agent -> flotillad
	Enrol     = "enrol"     // {token, host, csr}: on a connection without a client certificate, the only frame
	Hello     = "hello"     // {version, clock, chain_head}: first on an authenticated connection
	Heartbeat = "heartbeat" // {clock, chain_head}
	Result    = "result"    // hostexec.Result
	Renew     = "renew"     // {csr}: a request for the agent's next certificate
	Log       = "log"       // {at, level, msg, attrs}: one of the agent's own log lines (flotillad logs it)
	Approval  = "approval"  // {nonce, team, user, action}: a person's answer in Slack, relayed by the connector
	// a remote worker's link (RFC-0011) -> flotillad
	WorkerHello = "worker_hello" // {version, worker}: first on a worker identity's connection
	RunResult   = "run_result"   // {run_id, result | error}: a run's result, on any connection of its worker
	// flotillad -> agent
	Welcome  = "welcome"  // {clock}
	Enrolled = "enrolled" // {chain}: the certificate and the intermediate, PEM
	Renewed  = "renewed"  // {chain}
	Execute  = "execute"  // {envelope}: the signed execution, its exact bytes
	Refusal  = "refusal"  // {detail}: and the connection closes
	Notify   = "notify"   // {nonce, case, ...}: a plan or a notice waiting for a person, for the connector to post
	Answered = "answered" // {nonce, ok, text}: what became of a relayed approval
	// flotillad -> a remote worker's link (RFC-0011)
	Run    = "run"    // {run_id, request}: the worker's run request, as its HTTP API takes it
	Cancel = "cancel" // {run_id, stop}: the run's time is up (the link ends its local request), or with stop an operator's cancel (the worker stops the run)
)

type Frame struct {
	Type string          `json:"type"`
	Body json.RawMessage `json:"body,omitempty"`
}

// Write sends one frame of type t with body v.
func Write(w io.Writer, t string, v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b, err := json.Marshal(Frame{Type: t, Body: body})
	if err != nil {
		return err
	}
	if len(b) > MaxFrame {
		return fmt.Errorf("a %s frame of %d bytes, over %d", t, len(b), MaxFrame)
	}
	buf := make([]byte, 4+len(b))
	binary.BigEndian.PutUint32(buf, uint32(len(b)))
	copy(buf[4:], b)
	_, err = w.Write(buf)
	return err
}

// Read reads one frame.
func Read(r io.Reader) (Frame, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return Frame{}, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n == 0 || n > MaxFrame {
		return Frame{}, fmt.Errorf("a frame of %d bytes", n)
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return Frame{}, err
	}
	var f Frame
	if err := json.Unmarshal(b, &f); err != nil {
		return Frame{}, fmt.Errorf("a frame: %v", err)
	}
	if f.Type == "" {
		return Frame{}, errors.New("a frame with no type")
	}
	return f, nil
}

// Decode reads f's body into v, strictly.
func (f Frame) Decode(v any) error {
	dec := json.NewDecoder(bytes.NewReader(f.Body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("a %s frame: %v", f.Type, err)
	}
	return nil
}

// DecodeLenient reads f's body into v, ignoring fields v does not know: for a body flotillad may grow (a notify), so an
// agent older than flotillad still reads it.
func (f Frame) DecodeLenient(v any) error {
	if err := json.Unmarshal(f.Body, v); err != nil {
		return fmt.Errorf("a %s frame: %v", f.Type, err)
	}
	return nil
}

// Bodies. A result is hostexec.Result.

type EnrolBody struct {
	Host  string `json:"host"`
	Token string `json:"token"`
	CSR   []byte `json:"csr"` // DER
}

type HelloBody struct {
	Version   string    `json:"version"`
	Clock     time.Time `json:"clock"`
	ChainHead string    `json:"chain_head"`
	// What this host's own config allows (G10), for people to read on the UI: flotillad never trusts it
	// for a decision (the host enforces its config whatever flotillad believes). Older agents send none.
	Ceiling   string          `json:"ceiling,omitempty"`
	Allowlist []AllowedAction `json:"allowlist,omitempty"`
	// Caps are what the agent can do beyond the first protocol, sent only by an agent that has one (an added field: an
	// older agent sends none, and a flotillad older than the field refuses a hello that has it, so flotillad upgrades first).
	Caps []string `json:"caps,omitempty"`
}

// CapNotifyAnswers is the connector's capability of reading a NotifyBody with kind, answers and repeat, and so of showing
// notices. flotillad sends an agent without it plans in the old shape and no notices.
const CapNotifyAnswers = "notify_answers"

// HasCap reports whether the hello lists the capability.
func (h HelloBody) HasCap(c string) bool {
	for _, x := range h.Caps {
		if x == c {
			return true
		}
	}
	return false
}

// Legacy is the body as an agent from before answers reads it: a plan, with its approvable flag and no kind, answers or repeat.
func (n NotifyBody) Legacy() NotifyBody {
	n.Kind, n.Answers, n.Repeat = "", nil, false
	return n
}

// LogBody is one of the agent's own log lines. flotillad logs it as "host log" with the host's name; it
// is the agent's word about itself, never trusted for a decision.
type LogBody struct {
	At    time.Time         `json:"at"`
	Level string            `json:"level"`
	Msg   string            `json:"msg"`
	Attrs map[string]string `json:"attrs,omitempty"`
}

// Ask kinds a NotifyBody carries (ADR-0023): a plan that waits for approval, or a notice about a watched case.
const (
	KindPlan   = "plan"
	KindNotice = "notice"
)

// NotifyAnswer is one answer a message offers: a button when it is allowed, a line saying why not when it is not.
// flotillad checks a click against the answers it stored for the ask, never against what the connector rendered.
type NotifyAnswer struct {
	ID      string `json:"id"`
	Label   string `json:"label"`
	Style   string `json:"style,omitempty"` // primary | danger | empty
	Allowed bool   `json:"allowed"`
	Why     string `json:"why,omitempty"`
}

// NotifyBody is a question for a person, for the connector to post in Slack (ADR-0006 §1, decided 4; ADR-0023): a
// plan that waits, or a notice about a watched case. Plan is the line under the title: the action and its target for a
// plan, the case's detail for a notice. Approvable is whether policy lets Slack approve a plan at all (low or medium
// risk, reversible or regenerable); without it the message only says where to answer. Answers, when present, say what
// the message offers; a body without them (an older flotillad) is a plan offering approve and reject by Approvable.
// An agent reads it leniently; one from before answers does not (its decoding is strict), so it says what it reads in its hello
// (CapNotifyAnswers) and flotillad sends it plans in the old shape and no notices.
type NotifyBody struct {
	Nonce      string         `json:"nonce"`
	Case       string         `json:"case"`
	Title      string         `json:"title"`
	Plan       string         `json:"plan"`
	PlanHash   string         `json:"plan_hash"`
	URL        string         `json:"url"`
	Approvable bool           `json:"approvable"`
	Why        string         `json:"why,omitempty"` // why it is not approvable from Slack
	Kind       string         `json:"kind,omitempty"`
	Answers    []NotifyAnswer `json:"answers,omitempty"`
	Repeat     bool           `json:"repeat,omitempty"` // a later notice of a case already shown: update that message
}

// ApprovalBody is a click in Slack as the connector saw it. flotillad decides everything from it again:
// the workspace and user must be pinned, the nonce unused and the plan unchanged.
type ApprovalBody struct {
	Nonce  string `json:"nonce"`
	Team   string `json:"team"`
	User   string `json:"user"`
	Action string `json:"action"` // any answer id the message offered
}

// AnsweredBody is flotillad's answer to a relayed approval, for the connector to show on the message.
type AnsweredBody struct {
	Nonce string `json:"nonce"`
	OK    bool   `json:"ok"`
	Text  string `json:"text"`
}

type HeartbeatBody struct {
	Clock     time.Time `json:"clock"`
	ChainHead string    `json:"chain_head"`
}

type WelcomeBody struct {
	Clock time.Time `json:"clock"`
}

type RenewBody struct {
	CSR []byte `json:"csr"` // DER
}

type ChainBody struct {
	Chain string `json:"chain"` // PEM, the certificate then the intermediate
}

type ExecuteBody struct {
	Envelope []byte `json:"envelope"` // the signed execution's exact bytes
}

type RefusalBody struct {
	Detail string `json:"detail"`
}

// AllowedAction is one action a host's config allows: its exact command and limits.
type AllowedAction struct {
	Name    string   `json:"name"`
	Argv    []string `json:"argv"`
	PerHour int      `json:"per_hour"`
	Timeout string   `json:"timeout"`
}

// WorkerHelloBody opens a remote worker link's connection (RFC-0011 §2): its version must be flotillad's.
type WorkerHelloBody struct {
	Version string `json:"version"`
	Worker  string `json:"worker"`
	// Caps are what the agent can do beyond the first protocol: an added field, so an older agent sends none and a newer flotillad still reads it.
	Caps []string `json:"caps,omitempty"`
}

// CapRunToken is the capability of carrying a run's own token to the routine it runs (v2 step 2): an agent without it runs a routine with the worker's token,
// which flotillad refuses to accept when run_tokens is required.
const CapRunToken = "run_token"

// HasCap reports whether the hello lists the capability.
func (h WorkerHelloBody) HasCap(c string) bool {
	for _, x := range h.Caps {
		if x == c {
			return true
		}
	}
	return false
}

// RunBody is one run for a remote worker: the request exactly as the worker's /v1/runs takes it.
type RunBody struct {
	RunID   string          `json:"run_id"`
	Request json.RawMessage `json:"request"`
}

// RunResultBody is a run's result as the worker's /v1/runs answered it, or why the link has none.
type RunResultBody struct {
	RunID  string          `json:"run_id"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
}

type CancelBody struct {
	RunID string `json:"run_id"`
	// Stop: the run is cancelled, not timed out. The link asks its worker to stop the run (SIGTERM, then
	// SIGKILL) and leaves the request open, so the "cancelled" result and its spend still come back.
	Stop bool `json:"stop,omitempty"`
}
