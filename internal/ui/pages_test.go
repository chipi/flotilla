package ui

import (
	"context"
	"net/url"
	"strings"
	"testing"

	"github.com/chipi/flotilla/internal/api"
	"github.com/chipi/flotilla/internal/sched"
	"github.com/chipi/flotilla/internal/store"
)

// csrfOf signs in and returns the cookie and the CSRF token from the home page.
func (f *fixture) csrfOf(tok string) (string, string) {
	f.t.Helper()
	ck := f.signin(tok)
	_, body := f.do("GET", "/", ck, nil, nil)
	m := csrfRe.FindStringSubmatch(body)
	if m == nil {
		f.t.Fatalf("no CSRF token: %s", body)
	}
	return ck, m[1]
}

func (f *fixture) addOperator() string {
	f.t.Helper()
	tok := "flt_otto-token-for-tests"
	if err := f.st.AddUser(context.Background(), "otto", store.UserOperator, api.HashToken(tok), "user:ada", "api", false); err != nil {
		f.t.Fatal(err)
	}
	return tok
}

func TestEveryPageRendersForEveryRole(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	evil := `<script>alert(1)</script>`
	_ = f.st.StartRun(ctx, store.Run{ID: "run-1", Routine: "triage/cycle", SpecHash: "h", Trigger: "schedule", Actor: "flotillad",
		Autonomy: "shadow", StartedAt: "2026-11-13T10:00:00Z", Status: "running", Detail: evil})
	_, _ = f.st.AppendEntry(ctx, "triage/cycle", "decision", map[string]string{"fingerprint": "glitchtip:X-1", "occurrence_id": "glitchtip:X-1@1",
		"disposition": "file", "ts": "2026-11-13T10:00:00Z", "reason": evil, "alertname": "an alert"}, "")
	op := f.addOperator()
	pages := []string{"/", "/runs", "/runs/run-1", "/fleets", "/fleets/triage", "/autonomy", "/history"}
	for _, who := range []struct{ name, tok string }{{"viewer", f.vic}, {"operator", op}, {"admin", f.admin}} {
		ck := f.signin(who.tok)
		for _, p := range pages {
			resp, body := f.do("GET", p, ck, nil, nil)
			if resp.StatusCode != 200 {
				t.Fatalf("%s %s: %d %s", who.name, p, resp.StatusCode, body)
			}
			if strings.Contains(body, evil) {
				t.Fatalf("%s %s: untrusted text reached the page unescaped", who.name, p)
			}
			hasControls := strings.Contains(body, `action="/routines/`)
			if p == "/fleets/triage" && hasControls != (who.name != "viewer") {
				t.Fatalf("%s sees controls=%v on %s", who.name, hasControls, p)
			}
		}
		resp, _ := f.do("GET", "/users", ck, nil, nil)
		if (resp.StatusCode == 200) != (who.name == "admin") {
			t.Fatalf("%s on /users: %d", who.name, resp.StatusCode)
		}
	}
	if resp, _ := f.do("GET", "/runs/no-such", f.signin(f.vic), nil, nil); resp.StatusCode != 404 {
		t.Fatalf("an unknown run: %d", resp.StatusCode)
	}
}

func TestUIActionsGoThroughTheSamePath(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	op := f.addOperator()
	ck, csrf := f.csrfOf(op)
	post := func(path string, v url.Values) (int, string) {
		v.Set("csrf", csrf)
		resp, _ := f.do("POST", path, ck, v, nil)
		return resp.StatusCode, resp.Header.Get("Location")
	}
	if c, loc := post("/routines/triage/cycle/pause", url.Values{}); c != 303 || !strings.Contains(loc, "msg=") {
		t.Fatalf("pause: %d %s", c, loc)
	}
	if ev, _ := f.st.Events(ctx, 1); ev[0].Actor != "user:otto" || ev[0].Via != "ui" || ev[0].Kind != "control.paused" {
		t.Fatalf("the event names the person and the UI: %+v", ev[0])
	}
	// Resume without a reason is refused by the same rule as the API's.
	if c, loc := post("/routines/triage/cycle/resume", url.Values{}); c != 303 || !strings.Contains(loc, "err=") {
		t.Fatalf("resume without a reason: %d %s", c, loc)
	}
	if v, _ := f.st.Control(ctx, "triage/cycle", sched.KeyPaused); v != "1" {
		t.Fatalf("a refused resume changed the control: %q", v)
	}
	if c, _ := post("/routines/triage/cycle/resume", url.Values{"reason": {"checked"}}); c != 303 {
		t.Fatalf("resume: %d", c)
	}
	if v, _ := f.st.Control(ctx, "triage/cycle", sched.KeyPaused); v != "0" {
		t.Fatalf("resume did not take: %q", v)
	}
	if c, loc := post("/routines/triage/cycle/autonomy", url.Values{"autonomy": {"propose"}}); c != 303 || !strings.Contains(loc, "err=") {
		t.Fatalf("raising without a reason: %d %s", c, loc)
	}
	if c, _ := post("/fleets/triage/stop", url.Values{}); c != 303 {
		t.Fatalf("stop: %d", c)
	}
	if c, loc := post("/fleets/triage/start", url.Values{}); c != 303 || !strings.Contains(loc, "err=") {
		t.Fatalf("start without a reason: %d %s", c, loc)
	}
	// An offsite redirect target is ignored.
	if _, loc := post("/routines/triage/cycle/pause", url.Values{"back": {"//evil.example/x"}}); strings.HasPrefix(loc, "//") {
		t.Fatalf("redirected off the site: %s", loc)
	}
	// A viewer posting the same form is refused by the table, whatever the page showed.
	vck, vcsrf := f.csrfOf(f.vic)
	v := url.Values{"csrf": {vcsrf}}
	resp, _ := f.do("POST", "/routines/triage/cycle/pause", vck, v, nil)
	if !strings.Contains(resp.Header.Get("Location"), "err=") {
		t.Fatalf("a viewer pausing: %s", resp.Header.Get("Location"))
	}
}

func TestUsersPage(t *testing.T) {
	f := setup(t)
	ck, csrf := f.csrfOf(f.admin)
	resp, body := f.do("POST", "/users", ck, url.Values{"csrf": {csrf}, "name": {"nina"}, "role": {"viewer"}}, nil)
	if resp.StatusCode != 200 || resp.Header.Get("Cache-Control") != "no-store" || !strings.Contains(body, "flt_") {
		t.Fatalf("adding a user shows the token once, uncached: %d %v", resp.StatusCode, resp.Header)
	}
	if strings.Contains(resp.Header.Get("Location"), "flt_") {
		t.Fatal("a token in a URL")
	}
	// The last admin cannot revoke themself here either.
	resp, _ = f.do("POST", "/users/ada/revoke", ck, url.Values{"csrf": {csrf}}, nil)
	if !strings.Contains(resp.Header.Get("Location"), "err=") {
		t.Fatalf("revoking the last admin from the UI: %s", resp.Header.Get("Location"))
	}
	// An operator cannot reach users by posting the form.
	op := f.addOperator()
	ock, ocsrf := f.csrfOf(op)
	resp, _ = f.do("POST", "/users", ock, url.Values{"csrf": {ocsrf}, "name": {"mallory"}, "role": {"admin"}}, nil)
	if !strings.Contains(resp.Header.Get("Location"), "err=") {
		t.Fatalf("an operator adding a user: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
}
