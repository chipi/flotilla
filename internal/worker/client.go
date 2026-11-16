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
}

// Result is what the worker reports. Status is ok, error, timeout or cancelled (a cancel stopped it); anything that keeps the
// worker from answering is an error of the call, not a Result.
type Result struct {
	Status     string  `json:"status"`
	ExitCode   *int    `json:"exit_code"`
	DurationS  float64 `json:"duration_s"`
	SpendUSD   float64 `json:"spend_usd"`
	OutputTail string  `json:"output_tail"`
	// What a routine on a case decided (pipeline v1 step 5): one JSON object, from an ok run only, or why
	// it could not be read.
	Plan      json.RawMessage `json:"plan,omitempty"`
	PlanError string          `json:"plan_error,omitempty"`
	// Each model call the run made through runtime/model_client.py (its FLOTILLA_CALLS_FILE).
	Calls []Call `json:"calls,omitempty"`
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
}

type Health struct {
	OK      bool     `json:"ok"`
	Worker  string   `json:"worker"`
	Version string   `json:"version"`
	Running []string `json:"running"`
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
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
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
