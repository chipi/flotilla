package ui

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/chipi/flotilla/internal/api"
	"github.com/chipi/flotilla/internal/sched"
	"github.com/chipi/flotilla/internal/spec"
	"github.com/chipi/flotilla/internal/store"
	"github.com/chipi/flotilla/internal/worker"
)

const (
	host      = "flotilla.test"
	opTok     = "operator-token-0123456789abcdef0123456789"
	agentTok  = "agent-token-0123456789abcdef0123456789abc"
	workerTok = "worker-token-0123456789abcdef0123456789ab"
)

type fixture struct {
	t     *testing.T
	ts    *httptest.Server
	st    *store.Store
	admin string // ada's token
	vic   string // a viewer's token
}

func setup(t *testing.T) *fixture {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "u.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	j := &spec.Routine{Fleet: "triage", Routine: "cycle", Description: "d", Enabled: true, Worker: "triage", Autonomy: "shadow",
		Run: spec.Run{Command: "true"}, Every: 10 * time.Minute, Limit: time.Minute, Fresh: 35 * time.Minute}
	s := &sched.Scheduler{Store: st, Routines: []*spec.Routine{j}, Workers: map[string]sched.Runner{}}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := &api.Server{Sched: s, Store: st, Workers: map[string]*worker.Client{}, Version: "test", Log: log,
		Callers: append([]api.Caller{{Name: "operator", Role: api.RoleOperator, TokenSHA256: api.HashToken(opTok)},
			{Name: "claude-homelab", Role: api.RoleAgent, TokenSHA256: api.HashToken(agentTok)}},
			api.WorkerCallers(map[string]string{"triage": workerTok})...)}
	u := &Server{API: a, Host: host, Log: log}
	ts := httptest.NewServer(u.Handler())
	t.Cleanup(ts.Close)
	f := &fixture{t: t, ts: ts, st: st}
	ctx := context.Background()
	f.admin = "flt_ada-token-for-tests"
	if err := st.AddUser(ctx, "ada", store.UserAdmin, api.HashToken(f.admin), "operator", "api", true); err != nil {
		t.Fatal(err)
	}
	f.vic = "flt_vic-token-for-tests"
	if err := st.AddUser(ctx, "vic", store.UserViewer, api.HashToken(f.vic), "user:ada", "api", false); err != nil {
		t.Fatal(err)
	}
	return f
}

// do sends a request with the UI's Host; cookie and headers optional.
func (f *fixture) do(method, path, cookie string, form url.Values, hdr map[string]string) (*http.Response, string) {
	f.t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, _ := http.NewRequest(method, f.ts.URL+path, body)
	req.Host = host
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if cookie != "" {
		req.Header.Set("Cookie", cookieName+"="+cookie)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

// signin returns the session cookie's value for a token.
func (f *fixture) signin(tok string) string {
	f.t.Helper()
	resp, body := f.do("POST", "/signin", "", url.Values{"token": {tok}}, nil)
	if resp.StatusCode != 303 {
		f.t.Fatalf("sign in: %d %s", resp.StatusCode, body)
	}
	for _, c := range resp.Cookies() {
		if c.Name == cookieName {
			if !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteStrictMode || c.Path != "/" {
				f.t.Fatalf("cookie flags: %+v", c)
			}
			return c.Value
		}
	}
	f.t.Fatal("no session cookie")
	return ""
}

var csrfRe = regexp.MustCompile(`name="csrf" value="([0-9a-f]{64})"`)

func TestHostAndHeaders(t *testing.T) {
	f := setup(t)
	req, _ := http.NewRequest("GET", f.ts.URL+"/signin", nil) // Host is 127.0.0.1:port
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMisdirectedRequest {
		t.Fatalf("another Host: %d, want 421", resp.StatusCode)
	}
	resp, _ = f.do("GET", "/signin", "", nil, nil)
	csp := resp.Header.Get("Content-Security-Policy")
	if resp.StatusCode != 200 || !strings.Contains(csp, "frame-ancestors 'none'") || !strings.Contains(csp, "script-src 'self'") ||
		resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("headers: %d %v", resp.StatusCode, resp.Header)
	}
}

func TestSignInOnlyPeople(t *testing.T) {
	f := setup(t)
	if resp, _ := f.do("GET", "/", "", nil, nil); resp.StatusCode != 303 || resp.Header.Get("Location") != "/signin" {
		t.Fatalf("no session: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	for _, tok := range []string{agentTok, opTok, workerTok, "nonsense", ""} {
		if resp, _ := f.do("POST", "/signin", "", url.Values{"token": {tok}}, nil); resp.StatusCode != 401 {
			t.Fatalf("sign in with %q: %d, want 401", tok, resp.StatusCode)
		}
	}
	if ev, _ := f.st.Events(context.Background(), 1); ev[0].Kind != "user.sign-in-failed" || strings.Contains(ev[0].Detail, "token-") {
		t.Fatalf("a failure is recorded without the token: %+v", ev[0])
	}
	c1, c2 := f.signin(f.vic), f.signin(f.vic)
	if c1 == c2 || len(c1) != 64 {
		t.Fatalf("each sign-in gets a fresh 256-bit session id: %q %q", c1, c2)
	}
	resp, body := f.do("GET", "/", c1, nil, nil)
	if resp.StatusCode != 200 || !strings.Contains(body, "vic") || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("the page with a session: %d %s", resp.StatusCode, body)
	}
	// Cross-site sign-in is refused.
	if resp, _ := f.do("POST", "/signin", "", url.Values{"token": {f.vic}}, map[string]string{"Origin": "https://evil.example"}); resp.StatusCode != 403 {
		t.Fatalf("cross-site sign-in: %d, want 403", resp.StatusCode)
	}
}

func TestCSRFAndSignOutEverywhere(t *testing.T) {
	f := setup(t)
	ck := f.signin(f.vic)
	other := f.signin(f.vic)
	_, body := f.do("GET", "/", ck, nil, nil)
	m := csrfRe.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no CSRF token in the page: %s", body)
	}
	if resp, _ := f.do("POST", "/signout-everywhere", ck, url.Values{"csrf": {"wrong"}}, nil); resp.StatusCode != 403 {
		t.Fatalf("a POST with the wrong CSRF token: %d, want 403", resp.StatusCode)
	}
	if resp, _ := f.do("POST", "/signout-everywhere", ck, url.Values{}, nil); resp.StatusCode != 403 {
		t.Fatalf("a POST without a CSRF token: %d, want 403", resp.StatusCode)
	}
	if resp, _ := f.do("POST", "/signout-everywhere", ck, url.Values{"csrf": {m[1]}}, nil); resp.StatusCode != 303 {
		t.Fatalf("sign out everywhere: %d", resp.StatusCode)
	}
	if resp, _ := f.do("GET", "/", other, nil, nil); resp.StatusCode != 303 {
		t.Fatalf("the other device's session lived: %d", resp.StatusCode)
	}
}

func TestRevokeEndsTheSessionAtOnce(t *testing.T) {
	f := setup(t)
	ck := f.signin(f.vic)
	if err := f.st.RevokeUser(context.Background(), "vic", "user:ada", "api"); err != nil {
		t.Fatal(err)
	}
	if resp, _ := f.do("GET", "/", ck, nil, nil); resp.StatusCode != 303 {
		t.Fatalf("a revoked user's session: %d, want a redirect to sign in", resp.StatusCode)
	}
}

func TestV1OnTheUIListener(t *testing.T) {
	f := setup(t)
	bearer := func(tok string) map[string]string { return map[string]string{"Authorization": "Bearer " + tok} }
	if resp, body := f.do("GET", "/v1/status", "", nil, bearer(f.vic)); resp.StatusCode != 200 {
		t.Fatalf("a person's token on /v1: %d %s", resp.StatusCode, body)
	}
	for _, tok := range []string{agentTok, opTok, workerTok} {
		if resp, _ := f.do("GET", "/v1/status", "", nil, bearer(tok)); resp.StatusCode != 403 {
			t.Fatalf("a callers-file or worker token on the UI listener: %d, want 403", resp.StatusCode)
		}
	}
	if resp, _ := f.do("GET", "/v1/ledger/counts?ns=triage/cycle", "", nil, bearer(workerTok)); resp.StatusCode != 404 {
		t.Fatalf("the ledger on the UI listener: %d, want 404", resp.StatusCode)
	}
	ck := f.signin(f.vic)
	if resp, _ := f.do("GET", "/v1/status", ck, nil, nil); resp.StatusCode != 400 {
		t.Fatalf("the session cookie on /v1: %d, want 400", resp.StatusCode)
	}
	// A viewer's token still cannot act (the API's own table).
	if resp, _ := f.do("POST", "/v1/routines/triage/cycle/pause", "", nil, bearer(f.vic)); resp.StatusCode != 403 {
		t.Fatalf("a viewer pausing through the UI listener: %d, want 403", resp.StatusCode)
	}
}

func TestSignInFailureLimit(t *testing.T) {
	f := setup(t)
	for i := 0; i < failLimit; i++ {
		f.do("POST", "/signin", "", url.Values{"token": {"nope"}}, nil)
	}
	if resp, _ := f.do("POST", "/signin", "", url.Values{"token": {f.vic}}, nil); resp.StatusCode != 429 {
		t.Fatalf("after %d failures: %d, want 429", failLimit, resp.StatusCode)
	}
}

func TestStaticIsServed(t *testing.T) {
	f := setup(t)
	resp, body := f.do("GET", "/static/flotilla.css", "", nil, nil)
	if resp.StatusCode != 200 || !strings.Contains(body, "--signal-blue") {
		t.Fatalf("the stylesheet: %d", resp.StatusCode)
	}
	// A font the stylesheet names but the binary lacks fails silently in the browser (a fallback face).
	urls := regexp.MustCompile(`url\("([^"]+)"\)`).FindAllStringSubmatch(body, -1)
	if len(urls) < 3 {
		t.Fatalf("expected the three self-hosted fonts in the stylesheet, found %d url()s", len(urls))
	}
	for _, u := range urls {
		r, _ := f.do("GET", "/static/"+u[1], "", nil, nil)
		if r.StatusCode != 200 {
			t.Errorf("%s: %d", u[1], r.StatusCode)
		}
		if strings.HasSuffix(u[1], ".woff2") && r.Header.Get("Content-Type") != "font/woff2" {
			t.Errorf("%s: Content-Type %q", u[1], r.Header.Get("Content-Type"))
		}
	}
}
