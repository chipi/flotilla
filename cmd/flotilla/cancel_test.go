package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
)

// last is the request a stand-in server saw most recently. The server's goroutine writes it and the test reads it
// after the command (a subprocess) has exited, which is no synchronisation the race detector can see.
type last struct {
	mu                       sync.Mutex
	method, path, auth, body string
}

func (l *last) record(r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	l.mu.Lock()
	defer l.mu.Unlock()
	l.method, l.path, l.auth, l.body = r.Method, r.URL.Path, r.Header.Get("Authorization"), string(b)
}

func (l *last) get() (method, path, auth, body string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.method, l.path, l.auth, l.body
}

// The tests run the real command: the test binary runs main when FLOTILLA_CLI_AS_MAIN is set.
func TestMain(m *testing.M) {
	if os.Getenv("FLOTILLA_CLI_AS_MAIN") == "1" {
		main()
		return
	}
	os.Exit(m.Run())
}

func cli(t *testing.T, url string, args ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command(os.Args[0], args...)
	cmd.Env = append(os.Environ(), "FLOTILLA_CLI_AS_MAIN=1", "FLOTILLA_URL="+url, "FLOTILLA_TOKEN=op-token")
	var out, errb strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return out.String(), errb.String(), code
}

// `flotilla cancel RUN_ID --reason R` posts the reason to the run's cancel endpoint with the operator's token.
func TestCancelCommand(t *testing.T) {
	var seen last
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.record(r)
		w.Write([]byte(`{"cancelled":"triage-cycle-abc","by":"operator"}`))
	}))
	defer ts.Close()
	out, errs, code := cli(t, ts.URL, "cancel", "triage-cycle-abc", "--reason", "burning spend")
	if code != 0 || !strings.Contains(out, `"cancelled":"triage-cycle-abc"`) {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errs)
	}
	method, path, auth, body := seen.get()
	if method != "POST" || path != "/v1/runs/triage-cycle-abc/cancel" || auth != "Bearer op-token" || body != `{"reason":"burning spend"}` {
		t.Fatalf("sent %s %s auth=%q body=%q", method, path, auth, body)
	}
}

func TestCancelCommandNeedsARunAndShowsARefusal(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"cancelling needs a reason"}`, 400)
	}))
	defer ts.Close()
	if _, errs, code := cli(t, ts.URL, "cancel"); code != 2 || !strings.Contains(errs, "cancel RUN_ID --reason") {
		t.Fatalf("no run id: exit %d, %q", code, errs)
	}
	if _, errs, code := cli(t, ts.URL, "cancel", "r-1"); code != 1 || !strings.Contains(errs, "HTTP 400") || !strings.Contains(errs, "needs a reason") {
		t.Fatalf("a refusal: exit %d, %q", code, errs)
	}
}
