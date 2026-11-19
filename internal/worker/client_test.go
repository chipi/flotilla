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
	in := `{"status":"ok","calls":[{"alias":"a","role":"coder","episode":"ep-1","episode_seq":2,"tools":["bash","read"],"step":1},{"alias":"b","tools":[{"name":"bash","arg":"ls","ok":false}]},{"kind":"step","role":"gates","detail":"round 1: gates clean"}],"calls_dropped":7}`
	var res Result
	if err := json.Unmarshal([]byte(in), &res); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	var back Result
	if err := json.Unmarshal(b, &back); err != nil || len(back.Calls) != 3 || back.CallsDropped != 7 {
		t.Fatalf("round trip: %v %+v", err, back)
	}
	c := back.Calls[0]
	if c.Role != "coder" || c.Episode != "ep-1" || c.EpisodeSeq == nil || *c.EpisodeSeq != 2 || string(c.Tools) != `["bash","read"]` {
		t.Fatalf("the call after the round trip: %+v", c)
	}
	if string(back.Calls[1].Tools) != `[{"name":"bash","arg":"ls","ok":false}]` || back.Calls[2].Kind != "step" || back.Calls[2].Role != "gates" || back.Calls[2].Tools != nil {
		t.Fatalf("object tools and a step line after the round trip: %+v / %+v", back.Calls[1], back.Calls[2])
	}
}

// A run that recorded the worker's whole cap of calls answers with several MiB: the client reads all of it.
func TestRunReadsAResultOfAFullCallsCap(t *testing.T) {
	var b strings.Builder
	b.WriteString(`{"status":"ok","calls":[`)
	for i := 0; i < 5000; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"alias":"a","role":"coder","tools":[{"name":"bash","arg":"` + strings.Repeat("x", 160) + `","ok":true}],"detail":"` + strings.Repeat("d", 100) + `"}`)
	}
	b.WriteString(`],"calls_dropped":12}`)
	if b.Len() < 1<<20 {
		t.Fatalf("the fixture must be over the old 1 MiB bound, is %d", b.Len())
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(b.String())) }))
	defer ts.Close()
	res, err := New(ts.URL, "s").Run(context.Background(), Request{RunID: "r", Routine: "j/a", Command: "true", TimeoutS: 5})
	if err != nil || len(res.Calls) != 5000 || res.CallsDropped != 12 {
		t.Fatalf("%d calls, dropped %d, %v", len(res.Calls), res.CallsDropped, err)
	}
}

// A 409 is a refusal the caller can tell from a failed run: the wrong mode, a write credential, a missing snapshot, or a routine
// already running. The message says which.
func TestRunConflictIsARefusal(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(409)
		w.Write([]byte(`{"error":"this worker is not a shadow worker: it does not run a shadow run"}`))
	}))
	defer ts.Close()
	_, err := New(ts.URL, "s").Run(context.Background(), Request{RunID: "r", Routine: "j/a", Command: "true", TimeoutS: 1, Shadow: true})
	if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "not a shadow worker") || !strings.Contains(err.Error(), "HTTP 409") {
		t.Fatalf("%v", err)
	}
}

func TestShadowRequestFieldsTravelAndALiveRequestHasNone(t *testing.T) {
	var raw map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&raw)
		w.Write([]byte(`{"status":"ok","exit_code":0}`))
	}))
	defer ts.Close()
	c := New(ts.URL, "s")
	if _, err := c.Run(context.Background(), Request{RunID: "r", Routine: "j/a", Command: "true", TimeoutS: 1}); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"shadow", "input_key", "snapshot_of", "state", "api_token"} {
		if _, ok := raw[k]; ok {
			t.Errorf("a live request carries %q: %v", k, raw)
		}
	}
	if _, err := c.Run(context.Background(), Request{RunID: "s", Routine: "j/a", Command: "true", TimeoutS: 1, Shadow: true,
		InputKey: "live-1", SnapshotOf: "live-1", State: []string{"signal-fleet"}, APIToken: "tok"}); err != nil {
		t.Fatal(err)
	}
	if raw["shadow"] != true || raw["input_key"] != "live-1" || raw["snapshot_of"] != "live-1" || raw["api_token"] != "tok" ||
		len(raw["state"].([]any)) != 1 || raw["state"].([]any)[0] != "signal-fleet" {
		t.Fatalf("%v", raw)
	}
}

func TestHealthSaysTheMode(t *testing.T) {
	for body, capable := range map[string]bool{
		`{"ok":true,"mode":"shadow"}`: true, `{"ok":true,"mode":"live"}`: false, `{"ok":true}`: false,
	} {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(body)) }))
		h, err := New(ts.URL, "s").Health(context.Background())
		ts.Close()
		if err != nil || h.ShadowCapable() != capable {
			t.Errorf("%s: capable %v, want %v (%v)", body, h.ShadowCapable(), capable, err)
		}
	}
	var none *Health
	if none.ShadowCapable() {
		t.Error("no answer is not a shadow worker")
	}
}

func TestTakeAndDeleteASnapshot(t *testing.T) {
	var got struct {
		RunID string   `json:"run_id"`
		Paths []string `json:"paths"`
	}
	var auth string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		switch {
		case r.URL.Path == "/v1/snapshots" && r.Method == "POST":
			json.NewDecoder(r.Body).Decode(&got)
			switch got.RunID {
			case "exists":
				w.WriteHeader(409)
				w.Write([]byte(`{"error":"a snapshot of exists already exists"}`))
			case "big":
				w.WriteHeader(413)
				w.Write([]byte(`{"error":"the state is too big to snapshot: more than 5 bytes"}`))
			case "broken":
				w.WriteHeader(500)
				w.Write([]byte(`{"error":"snapshot: no space left"}`))
			default:
				w.Write([]byte(`{"run_id":"live-1","bytes":42,"files":3,"skipped":1,"missing":["gone"]}`))
			}
		case r.URL.Path == "/v1/snapshots/live-1/delete" && r.Method == "POST":
			w.Write([]byte(`{"ok":true,"run_id":"live-1","existed":true}`))
		case r.URL.Path == "/v1/snapshots/odd%2Fid/delete" || r.URL.RawPath == "/v1/snapshots/odd%2Fid/delete":
			w.WriteHeader(400)
			w.Write([]byte(`{"error":"a snapshot is named by its run id"}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer ts.Close()
	c := New(ts.URL, "sek")
	sn, err := c.TakeSnapshot(context.Background(), "live-1", []string{"signal-fleet"})
	if err != nil || sn.Bytes != 42 || sn.Files != 3 || sn.Skipped != 1 || len(sn.Missing) != 1 || got.RunID != "live-1" || got.Paths[0] != "signal-fleet" || auth != "Bearer sek" {
		t.Fatalf("%+v %v (sent %+v, auth %q)", sn, err, got, auth)
	}
	if _, err := c.TakeSnapshot(context.Background(), "exists", []string{"x"}); !errors.Is(err, ErrRefused) {
		t.Fatalf("a snapshot that exists: %v", err)
	}
	if _, err := c.TakeSnapshot(context.Background(), "big", []string{"x"}); !errors.Is(err, ErrTooBig) {
		t.Fatalf("over the caps: %v", err)
	}
	if _, err := c.TakeSnapshot(context.Background(), "broken", []string{"x"}); err == nil || errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "HTTP 500") {
		t.Fatalf("a worker failure is neither: %v", err)
	}
	if existed, err := c.DeleteSnapshot(context.Background(), "live-1"); err != nil || !existed {
		t.Fatalf("delete: %v %v", existed, err)
	}
	if _, err := c.DeleteSnapshot(context.Background(), "odd/id"); err == nil || !strings.Contains(err.Error(), "HTTP 400") {
		t.Fatalf("a run id with a slash is escaped, not a path: %v", err)
	}
}

func TestSnapshotUnreachable(t *testing.T) {
	c := &Client{URL: "http://127.0.0.1:1", Token: "t", HTTP: http.DefaultClient}
	if _, err := c.TakeSnapshot(context.Background(), "r", []string{"x"}); !errors.Is(err, ErrUnreachable) {
		t.Fatalf("%v", err)
	}
	if _, err := c.DeleteSnapshot(context.Background(), "r"); !errors.Is(err, ErrUnreachable) {
		t.Fatalf("%v", err)
	}
}

func TestFetchSnapshot(t *testing.T) {
	var auth, path string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth, path = r.Header.Get("Authorization"), r.URL.Path
		switch r.URL.Path {
		case "/v1/snapshots/live-1":
			w.Write([]byte("0123456789"))
		case "/v1/snapshots/gone":
			w.WriteHeader(404)
			w.Write([]byte(`{"error":"no such snapshot here"}`))
		default:
			w.WriteHeader(500)
			w.Write([]byte(`{"error":"disk"}`))
		}
	}))
	defer ts.Close()
	c := New(ts.URL, "sek")
	var buf strings.Builder
	n, err := c.FetchSnapshot(context.Background(), "live-1", &buf, 100)
	if err != nil || n != 10 || buf.String() != "0123456789" || auth != "Bearer sek" || path != "/v1/snapshots/live-1" {
		t.Fatalf("%d %q %v (%q %q)", n, buf.String(), err, auth, path)
	}
	buf.Reset()
	if n, err := c.FetchSnapshot(context.Background(), "live-1", &buf, 10); err != nil || n != 10 {
		t.Fatalf("exactly the cap is fine: %d %v", n, err)
	}
	buf.Reset()
	if _, err := c.FetchSnapshot(context.Background(), "live-1", &buf, 9); !errors.Is(err, ErrTooBig) {
		t.Fatalf("over the cap: %v", err)
	}
	if _, err := c.FetchSnapshot(context.Background(), "gone", &buf, 100); !errors.Is(err, ErrNoSuchSnapshot) {
		t.Fatalf("a snapshot the worker lost: %v", err)
	}
	if _, err := c.FetchSnapshot(context.Background(), "other", &buf, 100); err == nil || !strings.Contains(err.Error(), "HTTP 500") {
		t.Fatalf("%v", err)
	}
	dead := &Client{URL: "http://127.0.0.1:1", Token: "t", HTTP: http.DefaultClient}
	if _, err := dead.FetchSnapshot(context.Background(), "x", &buf, 1); !errors.Is(err, ErrUnreachable) {
		t.Fatalf("%v", err)
	}
}
