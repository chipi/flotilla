// Package worker is flotillad's side of the worker protocol: start a run on a fleet's worker and
// wait for its result. The worker (worker/flotilla_worker.py) holds the fleet's secrets and runs the
// command; flotillad never sees the secrets, and the worker never decides whether a run may happen.
package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"
)

// ErrUnreachable: the request never reached the worker (its name did not resolve, or nothing answered
// the dial), so the run did not start.
var ErrUnreachable = errors.New("worker unreachable")

// ErrNoSuchRun: the worker is not running that run id (never started there, or already answered).
var ErrNoSuchRun = errors.New("the worker is not running that run")

// ErrRefused: the worker answered 409, so it did not do what was asked: the routine is already running there, or it is the
// wrong mode for the run (a shadow run on a live worker, a live run on a shadow worker), or a shadow run's environment holds a
// write credential, or its snapshot is not there. The message says which.
var ErrRefused = errors.New("the worker refused")

// ErrTooBig: the state to snapshot is over the worker's caps (413); nothing was kept.
var ErrTooBig = errors.New("the state is too big to snapshot")

// MaxResultBytes bounds a worker's answer: the output tail and up to the worker's CALLS_MAX (5000) calls, a few hundred
// bytes each and more with tool arguments. A longer answer would be cut mid-JSON and fail the whole run.
const MaxResultBytes = 16 << 20

// Request is one run, as flotillad hands it to a worker.
type Request struct {
	RunID     string            `json:"run_id"`
	Routine   string            `json:"routine"`
	Command   string            `json:"command"`
	Workdir   string            `json:"workdir"`
	TimeoutS  float64           `json:"timeout_s"`
	SpendFile string            `json:"spend_file,omitempty"`
	PlanFile  string            `json:"plan_file,omitempty"` // a routine on a case writes its decision here
	Env       map[string]string `json:"env"`
	// APIToken is this run's own token for flotillad's workers' listener (internal/runtoken): the worker writes it to a file in the
	// run's temp directory, names that file in FLOTILLA_API_TOKEN_FILE and removes it when the run ends. It is never in Env, so no
	// log of the environment holds it.
	APIToken string `json:"api_token,omitempty"`
	// Shadow marks a shadow run (v2 step 2): a worker that is not a shadow worker refuses it, and a shadow worker refuses
	// anything else.
	Shadow bool `json:"shadow,omitempty"`
	// InputKey is the live run whose input a shadow run was given; SnapshotOf is the live run whose state snapshot it starts from
	// (the same run, today) and State the routine's state paths under HOME: the worker removes them from its own HOME and copies
	// the snapshot's in before the command starts. Only a shadow run has them.
	InputKey   string   `json:"input_key,omitempty"`
	SnapshotOf string   `json:"snapshot_of,omitempty"`
	State      []string `json:"state,omitempty"`
}

// Result is what the worker reports. Status is ok, error, timeout or cancelled (a cancel stopped it); anything that keeps the
// worker from answering is an error of the call, not a Result.
type Result struct {
	Status    string  `json:"status"`
	ExitCode  *int    `json:"exit_code"`
	DurationS float64 `json:"duration_s"`
	SpendUSD  float64 `json:"spend_usd"`
	// SpendReported is false when the routine names a spend file and left no number in it: SpendUSD is then 0
	// by default, not by report. nil from a worker older than the field.
	SpendReported *bool  `json:"spend_reported,omitempty"`
	OutputTail    string `json:"output_tail"`
	// What a routine on a case decided (pipeline v1 step 5): one JSON object, from an ok run only, or why
	// it could not be read.
	Plan      json.RawMessage `json:"plan,omitempty"`
	PlanError string          `json:"plan_error,omitempty"`
	// Each model call the run made through runtime/model_client.py (its FLOTILLA_CALLS_FILE), and the harness's
	// steps (bugfix/fix.py): at most the worker's CALLS_MAX, in order.
	Calls []Call `json:"calls,omitempty"`
	// How many more lines the run's calls file had than the worker returned (the first CALLS_MAX are kept); 0 from a
	// worker older than the field or a run within the cap.
	CallsDropped int `json:"calls_dropped,omitempty"`
	// What the run read into a model's context (G7, runtime/loaded.py: its FLOTILLA_LOADED_FILE).
	Loaded []Loaded `json:"loaded,omitempty"`
}

// Loaded is one thing a run read: a playbook, a skill, a memory or an input, and how far it is trusted
// (source, memory, untrusted).
type Loaded struct {
	At     string `json:"at"`
	Kind   string `json:"kind"`
	Trust  string `json:"trust"`
	Title  string `json:"title"`
	Source string `json:"source"`
	SHA256 string `json:"sha256"`
}

// Call is one model call as the model client recorded it. Tokens and cost are nil when the endpoint
// did not report them (never counted as zero).
type Call struct {
	At         string   `json:"at"`
	Alias      string   `json:"alias"`
	Endpoint   string   `json:"endpoint"`
	Model      string   `json:"model"`
	Deployment string   `json:"deployment"`
	TokensIn   *int64   `json:"tokens_in"`
	TokensOut  *int64   `json:"tokens_out"`
	CostUSD    *float64 `json:"cost_usd"`
	CostSource string   `json:"cost_source"`
	LatencyS   *float64 `json:"latency_s"`
	Status     *int     `json:"status"`
	Error      string   `json:"error"`
	// set on a contract check's line (G15, runtime/contract_routine.py): which check, and its result
	Check   string `json:"check,omitempty"`
	CheckOK *bool  `json:"check_ok,omitempty"`
	Detail  string `json:"detail,omitempty"`
	// the model client's finish reason, the turn within a step, and what the call served (model_client.py)
	FinishReason string `json:"finish_reason"`
	Step         *int64 `json:"step"`
	DecisionKey  string `json:"decision_key"`
	// who made the call and in which session, and the tools it called (model_client.py, bugfix/callrec_pi.py)
	Role       string `json:"role"`
	Episode    string `json:"episode"`
	EpisodeSeq *int64 `json:"episode_seq"`
	// Tools is a JSON array of the turn's tool calls, [{"name","arg","ok"}]; a worker older than that sends names
	// ["bash"]. Kept raw: store.NormalizeTools reads either form, and a re-marshal (the host link) loses nothing.
	Tools json.RawMessage `json:"tools,omitempty"`
	// "call" (a model call, and what a line without it is) or "step" (a harness step: no model, tokens or cost)
	Kind string `json:"kind,omitempty"`
}

type Health struct {
	OK      bool     `json:"ok"`
	Worker  string   `json:"worker"`
	Version string   `json:"version"`
	Running []string `json:"running"`
	// Mode is "live" or "shadow"; "" from a worker older than the field, which must never be sent a shadow run (it would drop the
	// flag and run it as a live one).
	Mode string `json:"mode,omitempty"`
}

// ShadowCapable says the worker answered /healthz as a shadow worker: only that is evidence it will refuse what is not a shadow
// run and run a shadow with the read environment.
func (h *Health) ShadowCapable() bool { return h != nil && h.Mode == "shadow" }

// Snapshot is what a live worker made of a routine's state paths for a run: how much it copied, the links it skipped and the
// paths that were not there.
type Snapshot struct {
	RunID   string   `json:"run_id"`
	Bytes   int64    `json:"bytes"`
	Files   int      `json:"files"`
	Skipped int      `json:"skipped"`
	Missing []string `json:"missing"`
}

// TakeSnapshot asks a live worker to copy a routine's state paths (relative to its HOME) into its snapshot volume under the live
// run's id, whole or not at all. ErrRefused: one exists, or it is a shadow worker. ErrTooBig: over the caps.
func (c *Client) TakeSnapshot(ctx context.Context, runID string, paths []string) (*Snapshot, error) {
	body, err := json.Marshal(map[string]any{"run_id": runID, "paths": paths})
	if err != nil {
		return nil, err
	}
	raw, status, err := c.do(ctx, http.MethodPost, "/v1/snapshots", body, 2*time.Minute)
	if err != nil {
		return nil, err
	}
	switch status {
	case http.StatusOK:
		var sn Snapshot
		if err := json.Unmarshal(raw, &sn); err != nil {
			return nil, fmt.Errorf("worker %s: bad snapshot answer: %w", c.URL, err)
		}
		return &sn, nil
	case http.StatusConflict:
		return nil, fmt.Errorf("worker %s: %w: %s", c.URL, ErrRefused, bytes.TrimSpace(raw))
	case http.StatusRequestEntityTooLarge:
		return nil, fmt.Errorf("worker %s: %w: %s", c.URL, ErrTooBig, bytes.TrimSpace(raw))
	}
	return nil, fmt.Errorf("worker %s: HTTP %d: %s", c.URL, status, bytes.TrimSpace(raw))
}

// ErrNoSuchSnapshot: the worker has no snapshot of that run (never taken, deleted, or on another host).
var ErrNoSuchSnapshot = errors.New("the worker has no such snapshot")

// FetchSnapshot streams a run's snapshot, a tar.gz, into w, at most max bytes (ErrTooBig past it: w then holds a cut archive and
// the caller discards it).
func (c *Client) FetchSnapshot(ctx context.Context, runID string, w io.Writer, max int64) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.URL+"/v1/snapshots/"+url.PathEscape(runID), nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		var op *net.OpError
		if errors.As(err, &op) && op.Op == "dial" {
			return 0, fmt.Errorf("worker %s: %w: %w", c.URL, ErrUnreachable, err)
		}
		return 0, fmt.Errorf("worker %s: %w", c.URL, err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return 0, fmt.Errorf("worker %s: %w (%s)", c.URL, ErrNoSuchSnapshot, runID)
	default:
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		return 0, fmt.Errorf("worker %s: HTTP %d: %s", c.URL, resp.StatusCode, bytes.TrimSpace(raw))
	}
	n, err := io.Copy(w, io.LimitReader(resp.Body, max+1))
	if err != nil {
		return n, fmt.Errorf("worker %s: %w", c.URL, err)
	}
	if n > max {
		return n, fmt.Errorf("worker %s: %w: more than %d bytes", c.URL, ErrTooBig, max)
	}
	return n, nil
}

// DeleteSnapshot removes a run's snapshot; existed says whether there was one (a second delete is fine).
func (c *Client) DeleteSnapshot(ctx context.Context, runID string) (existed bool, err error) {
	raw, status, err := c.do(ctx, http.MethodPost, "/v1/snapshots/"+url.PathEscape(runID)+"/delete", []byte("{}"), 30*time.Second)
	if err != nil {
		return false, err
	}
	if status != http.StatusOK {
		return false, fmt.Errorf("worker %s: HTTP %d: %s", c.URL, status, bytes.TrimSpace(raw))
	}
	var out struct {
		Existed bool `json:"existed"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return false, fmt.Errorf("worker %s: bad delete answer: %w", c.URL, err)
	}
	return out.Existed, nil
}

func (c *Client) do(ctx context.Context, method, path string, body []byte, timeout time.Duration) ([]byte, int, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, c.URL+path, bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.Token)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		var op *net.OpError
		if errors.As(err, &op) && op.Op == "dial" {
			return nil, 0, fmt.Errorf("worker %s: %w: %w", c.URL, ErrUnreachable, err)
		}
		return nil, 0, fmt.Errorf("worker %s: %w", c.URL, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return raw, resp.StatusCode, nil
}

type Client struct {
	URL   string
	Token string
	HTTP  *http.Client
}

func New(url, token string) *Client {
	return &Client{URL: url, Token: token, HTTP: &http.Client{}}
}

// Run blocks until the worker reports, or ctx ends. The HTTP deadline is the run's timeout plus a
// margin for the worker to stop the process group and answer.
func (c *Client) Run(ctx context.Context, r Request) (*Result, error) {
	body, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(r.TimeoutS*float64(time.Second))+45*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL+"/v1/runs", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if tp := r.Env["TRACEPARENT"]; tp != "" {
		req.Header.Set("traceparent", tp)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		var op *net.OpError
		if errors.As(err, &op) && op.Op == "dial" {
			return nil, fmt.Errorf("worker %s: %w: %w", c.URL, ErrUnreachable, err)
		}
		return nil, fmt.Errorf("worker %s: %w", c.URL, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, MaxResultBytes))
	if resp.StatusCode == http.StatusConflict {
		return nil, fmt.Errorf("worker %s: %w: HTTP %d: %s", c.URL, ErrRefused, resp.StatusCode, bytes.TrimSpace(raw))
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("worker %s: HTTP %d: %s", c.URL, resp.StatusCode, bytes.TrimSpace(raw))
	}
	var res Result
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("worker %s: bad result: %w", c.URL, err)
	}
	switch res.Status {
	case "ok", "error", "timeout", "cancelled":
	default:
		return nil, fmt.Errorf("worker %s: unknown status %q", c.URL, res.Status)
	}
	return &res, nil
}

// Cancel asks the worker to stop a run in flight, as its timeout would: SIGTERM, then SIGKILL after the
// worker's grace. The run's own Run call then returns a Result with status "cancelled". A run the worker
// is not running is ErrNoSuchRun; cancelling twice while it ends is fine.
func (c *Client) Cancel(ctx context.Context, runID string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL+"/v1/runs/"+url.PathEscape(runID)+"/cancel", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		var op *net.OpError
		if errors.As(err, &op) && op.Op == "dial" {
			return fmt.Errorf("worker %s: %w: %w", c.URL, ErrUnreachable, err)
		}
		return fmt.Errorf("worker %s: %w", c.URL, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	switch resp.StatusCode {
	case http.StatusOK:
		return nil
	case http.StatusNotFound:
		return fmt.Errorf("worker %s: %w (%s)", c.URL, ErrNoSuchRun, bytes.TrimSpace(raw))
	}
	return fmt.Errorf("worker %s: HTTP %d: %s", c.URL, resp.StatusCode, bytes.TrimSpace(raw))
}

// Ready is whether the worker answers its health check now.
func (c *Client) Ready(ctx context.Context) bool {
	_, err := c.Health(ctx)
	return err == nil
}

func (c *Client) Health(ctx context.Context) (*Health, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.URL+"/healthz", nil)
	req.Header.Set("Authorization", "Bearer "+c.Token)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("worker %s: HTTP %d", c.URL, resp.StatusCode)
	}
	var h Health
	return &h, json.NewDecoder(resp.Body).Decode(&h)
}

// ResolveCommit asks a live worker for the commit a repository's branch points at, read as a routine would read it.
func (c *Client) ResolveCommit(ctx context.Context, repo, branch string) (string, error) {
	body, err := json.Marshal(map[string]string{"repo": repo, "branch": branch})
	if err != nil {
		return "", err
	}
	raw, status, err := c.do(ctx, http.MethodPost, "/v1/commit", body, 30*time.Second)
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("worker %s: HTTP %d: %s", c.URL, status, bytes.TrimSpace(raw))
	}
	var out struct {
		SHA string `json:"sha"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.SHA == "" {
		return "", fmt.Errorf("worker %s: bad commit answer", c.URL)
	}
	return out.SHA, nil
}
