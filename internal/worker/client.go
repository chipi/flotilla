// Package worker is flotillad's side of the worker protocol: start a run on a fleet's worker and
// wait for its result. The worker (worker/flotilla_worker.py) holds the fleet's secrets and runs the
// command; flotillad never sees the secrets, and the worker never decides whether a run may happen.
package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Request is one run, as flotillad hands it to a worker.
type Request struct {
	RunID     string            `json:"run_id"`
	Routine   string            `json:"routine"`
	Command   string            `json:"command"`
	Workdir   string            `json:"workdir"`
	TimeoutS  float64           `json:"timeout_s"`
	SpendFile string            `json:"spend_file,omitempty"`
	Env       map[string]string `json:"env"`
}

// Result is what the worker reports. Status is ok, error or timeout; anything that keeps the
// worker from answering is an error of the call, not a Result.
type Result struct {
	Status     string  `json:"status"`
	ExitCode   *int    `json:"exit_code"`
	DurationS  float64 `json:"duration_s"`
	SpendUSD   float64 `json:"spend_usd"`
	OutputTail string  `json:"output_tail"`
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
	case "ok", "error", "timeout":
	default:
		return nil, fmt.Errorf("worker %s: unknown status %q", c.URL, res.Status)
	}
	return &res, nil
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
