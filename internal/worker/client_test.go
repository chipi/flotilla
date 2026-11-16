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
