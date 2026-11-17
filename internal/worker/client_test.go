package worker

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRun(t *testing.T) {
	var got Request
	var auth, tp string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth, tp = r.Header.Get("Authorization"), r.Header.Get("traceparent")
		json.NewDecoder(r.Body).Decode(&got)
		w.Write([]byte(`{"status":"ok","exit_code":0,"duration_s":2,"spend_usd":0.1,"output_tail":"done"}`))
	}))
	defer ts.Close()
	c := New(ts.URL, "secret")
	res, err := c.Run(context.Background(), Request{RunID: "r", Routine: "j/a", Command: "true", TimeoutS: 5,
		Env: map[string]string{"TRACEPARENT": "00-abc-def-01"}})
	if err != nil || res.Status != "ok" || *res.ExitCode != 0 || res.SpendUSD != 0.1 {
		t.Fatalf("%+v %v", res, err)
	}
	if auth != "Bearer secret" || tp != "00-abc-def-01" || got.RunID != "r" {
		t.Fatalf("auth=%q traceparent=%q req=%+v", auth, tp, got)
	}
}

func TestRunRejects(t *testing.T) {
	for name, c := range map[string]struct {
		code int
		body string
		want string
	}{
		"http error":     {409, `{"error":"already running"}`, "HTTP 409"},
		"unknown status": {200, `{"status":"maybe"}`, "unknown status"},
		"not json":       {200, `<html>`, "bad result"},
	} {
		t.Run(name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(c.code)
				w.Write([]byte(c.body))
			}))
			defer ts.Close()
			_, err := New(ts.URL, "s").Run(context.Background(), Request{RunID: "r", Routine: "j/a", Command: "true", TimeoutS: 1})
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want %q, got %v", c.want, err)
			}
		})
	}
}

// A worker nothing answers for is ErrUnreachable (the run did not start); an answered error is not.
func TestUnreachable(t *testing.T) {
	c := &Client{URL: "http://127.0.0.1:1", Token: "t", HTTP: http.DefaultClient}
	if _, err := c.Run(context.Background(), Request{RunID: "r", TimeoutS: 1}); !errors.Is(err, ErrUnreachable) {
		t.Fatalf("a refused dial: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "no", http.StatusInternalServerError) }))
	defer srv.Close()
	c.URL = srv.URL
	if _, err := c.Run(context.Background(), Request{RunID: "r", TimeoutS: 1}); err == nil || errors.Is(err, ErrUnreachable) {
		t.Fatalf("an answered error is not unreachable: %v", err)
	}
}

func TestCancel(t *testing.T) {
	var method, path, auth string
	code := 200
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path, auth = r.Method, r.URL.Path, r.Header.Get("Authorization")
		w.WriteHeader(code)
		w.Write([]byte(`{"error":"no such run here"}`))
	}))
	defer ts.Close()
	c := New(ts.URL, "secret")
	if err := c.Cancel(context.Background(), "triage-cycle-abc123"); err != nil {
		t.Fatal(err)
	}
	if method != "POST" || path != "/v1/runs/triage-cycle-abc123/cancel" || auth != "Bearer secret" {
		t.Fatalf("sent %s %s auth=%q", method, path, auth)
	}
	code = 404
	if err := c.Cancel(context.Background(), "gone"); !errors.Is(err, ErrNoSuchRun) {
		t.Fatalf("an unknown run: %v", err)
	}
	code = 401
	if err := c.Cancel(context.Background(), "x"); err == nil || errors.Is(err, ErrNoSuchRun) || !strings.Contains(err.Error(), "HTTP 401") {
		t.Fatalf("a refused token: %v", err)
	}
	dead := &Client{URL: "http://127.0.0.1:1", Token: "t", HTTP: http.DefaultClient}
	if err := dead.Cancel(context.Background(), "x"); !errors.Is(err, ErrUnreachable) {
		t.Fatalf("nothing answering: %v", err)
	}
}

// A run a cancel stopped is answered as cancelled, which is a result and not an error of the call.
func TestRunCancelledIsAResult(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"cancelled","exit_code":-15,"duration_s":3,"spend_usd":0.07,"output_tail":"stopped"}`))
	}))
	defer ts.Close()
	res, err := New(ts.URL, "s").Run(context.Background(), Request{RunID: "r", Routine: "j/a", Command: "true", TimeoutS: 5})
	if err != nil || res.Status != "cancelled" || res.SpendUSD != 0.07 {
		t.Fatalf("%+v %v", res, err)
	}
}

// A result's calls cross the link (hostagent marshals the worker's Result, hostlink unmarshals it) with their role,
// episode, its order and tool names: a Call without those fields would drop them from every remote run.
func TestResultCallsKeepRoleEpisodeAndTools(t *testing.T) {
	in := `{"status":"ok","calls":[{"alias":"a","role":"coder","episode":"ep-1","episode_seq":2,"tools":["bash","read"],"step":1}]}`
	var res Result
	if err := json.Unmarshal([]byte(in), &res); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	var back Result
	if err := json.Unmarshal(b, &back); err != nil || len(back.Calls) != 1 {
		t.Fatalf("round trip: %v %+v", err, back)
	}
	c := back.Calls[0]
	if c.Role != "coder" || c.Episode != "ep-1" || c.EpisodeSeq == nil || *c.EpisodeSeq != 2 || len(c.Tools) != 2 || c.Tools[1] != "read" {
		t.Fatalf("the call after the round trip: %+v", c)
	}
}
