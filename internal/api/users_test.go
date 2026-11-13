package api

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/chipi/flotilla/internal/sched"
)

func TestBootstrapBreakGlassAndRetiredOperatorToken(t *testing.T) {
	ts, st := server(t)
	ctx := context.Background()
	// The agent never makes users, even on an empty table.
	if c, _ := call(t, ts, "POST", "/v1/users", agentTok, `{"name":"mallory","role":"admin"}`); c != 403 {
		t.Fatalf("an agent adding the first admin: %d, want 403", c)
	}
	// The first user must be an admin.
	if c, _ := call(t, ts, "POST", "/v1/users", opTok, `{"name":"ann","role":"operator"}`); c != 400 {
		t.Fatalf("first user as operator: %d, want 400", c)
	}
	// Before any admin, the callers-file operator token is an operator (the installation keeps working).
	if c, _ := call(t, ts, "POST", "/v1/routines/triage/cycle/resume", opTok, `{"reason":"before users"}`); c != 200 {
		t.Fatalf("operator token before any admin: %d", c)
	}
	c, out := call(t, ts, "POST", "/v1/users", opTok, `{"name":"marko","role":"admin"}`)
	if c != 201 || !strings.HasPrefix(fmt.Sprint(out["token"]), "flt_") || len(fmt.Sprint(out["token"])) != 68 {
		t.Fatalf("bootstrap: %d %v", c, out)
	}
	if ev, _ := st.Events(ctx, 1); ev[0].Kind != "user.bootstrap" || ev[0].Subject != "user:marko" {
		t.Fatalf("the bootstrap is recorded: %+v", ev[0])
	}
	admin := out["token"].(string)
	// Now the callers-file operator token is retired for everything but break-glass.
	if c, _ := call(t, ts, "POST", "/v1/routines/triage/cycle/resume", opTok, `{"reason":"x"}`); c != 403 {
		t.Fatalf("retired operator token resuming: %d, want 403", c)
	}
	if c, _ := call(t, ts, "GET", "/v1/status", opTok, ""); c != 403 {
		t.Fatalf("retired operator token reading: %d, want 403", c)
	}
	for _, p := range []string{"/v1/users/marko/role", "/v1/users/marko/revoke", "/v1/users/marko/rotate"} {
		if c, _ := call(t, ts, "POST", p, opTok, `{"role":"viewer"}`); c != 403 {
			t.Fatalf("retired operator token on %s: %d, want 403", p, c)
		}
	}
	if c, _ := call(t, ts, "GET", "/v1/users", opTok, ""); c != 403 {
		t.Fatalf("retired operator token listing users: %d, want 403", c)
	}
	if c, _ := call(t, ts, "POST", "/v1/users", opTok, `{"name":"eve","role":"operator"}`); c != 400 {
		t.Fatalf("the operator token adding a non-admin: %d, want 400", c)
	}
	// Break-glass: the host-held token may add an admin when the admins' tokens are lost; recorded as such.
	if c, _ := call(t, ts, "POST", "/v1/users", opTok, `{"name":"marko2","role":"admin"}`); c != 201 {
		t.Fatalf("break-glass: %d", c)
	}
	if ev, _ := st.Events(ctx, 1); ev[0].Kind != "user.break-glass" || ev[0].Actor != "operator" {
		t.Fatalf("break-glass is marked: %+v", ev[0])
	}
	// The admin acts as user:marko.
	if c, _ := call(t, ts, "POST", "/v1/routines/triage/cycle/pause", admin, ""); c != 200 {
		t.Fatalf("admin pause: %d", c)
	}
	if ev, _ := st.Events(ctx, 1); ev[0].Actor != "user:marko" {
		t.Fatalf("the event names the person: %+v", ev[0])
	}
}

func TestPermissionTable(t *testing.T) {
	ts, _ := server(t)
	_, out := call(t, ts, "POST", "/v1/users", opTok, `{"name":"ada","role":"admin"}`)
	admin := out["token"].(string)
	tok := map[string]string{RoleAdmin: admin, RoleAgent: agentTok, RoleWorker: workerTok}
	for _, role := range []string{RoleOperator, RoleViewer} {
		_, out := call(t, ts, "POST", "/v1/users", admin, fmt.Sprintf(`{"name":"u-%s","role":%q}`, role, role))
		tok[role] = out["token"].(string)
	}
	reqs := []struct {
		a                  Action
		method, path, body string
	}{
		{ActRead, "GET", "/v1/status", ""},
		{ActRead, "GET", "/v1/runs", ""},
		{ActRead, "GET", "/v1/events", ""},
		{ActRead, "GET", "/v1/cases", ""},
		{ActPause, "POST", "/v1/routines/triage/cycle/pause", ""},
		{ActResume, "POST", "/v1/routines/triage/cycle/resume", `{"reason":"t"}`},
		{ActAutonomy, "POST", "/v1/routines/triage/cycle/autonomy", `{"autonomy":"shadow"}`},
		{ActStop, "POST", "/v1/fleets/triage/stop", ""},
		{ActStart, "POST", "/v1/fleets/triage/start", `{"reason":"t"}`},
		{ActUsers, "GET", "/v1/users", ""},
		{ActLedger, "GET", "/v1/ledger/counts?ns=triage/cycle", ""},
		{ActFreeze, "POST", "/v1/freeze", ""},
		{ActUnfreeze, "POST", "/v1/unfreeze", `{"reason":"t"}`},
		{ActRead, "GET", "/v1/holds", ""},
		{ActHold, "POST", "/v1/holds", `{"scope":"host:mini","for":"1h","reason":"t"}`},
		{ActUnhold, "POST", "/v1/holds/1/lift", `{"reason":"t"}`},
		{ActRead, "GET", "/v1/actions", ""},
		{ActAutonomy, "POST", "/v1/actions/backup.rerun_observability/autonomy", `{"autonomy":"shadow"}`},
	}
	for _, role := range []string{RoleWorker, RoleAgent, RoleViewer, RoleOperator, RoleAdmin} {
		for _, q := range reqs {
			c, out := call(t, ts, q.method, q.path, tok[role], q.body)
			if got, want := c != 403 && c != 401, Authorize(role, q.a); got != want {
				t.Errorf("%s %s %s: HTTP %d %v; the table says allowed=%v", role, q.method, q.path, c, out, want)
			}
		}
	}
	// The table itself, as ADR-0014 writes it.
	for _, x := range []struct {
		role string
		a    Action
		ok   bool
	}{
		{RoleViewer, ActRun, false}, {RoleViewer, ActPause, false}, {RoleViewer, ActRead, true},
		{RoleAgent, ActResume, false}, {RoleAgent, ActStop, true}, {RoleAgent, ActUsers, false},
		{RoleOperator, ActUsers, false}, {RoleAdmin, ActResume, true}, {RoleAdmin, ActUsers, true},
		{RoleWorker, ActRead, false}, {RoleAdmin, ActLedger, false},
		{RoleAgent, ActFreeze, true}, {RoleAgent, ActUnfreeze, false}, {RoleViewer, ActFreeze, false},
		{RoleOperator, ActUnfreeze, true}, {RoleWorker, ActFreeze, false},
	} {
		if Authorize(x.role, x.a) != x.ok {
			t.Errorf("Authorize(%s, %s) != %v", x.role, x.a, x.ok)
		}
	}
}

func TestLooseningNeedsAReason(t *testing.T) {
	ts, st := server(t)
	for _, p := range []string{"/v1/routines/triage/cycle/resume", "/v1/fleets/triage/start", "/v1/unfreeze"} {
		if c, _ := call(t, ts, "POST", p, opTok, ""); c != 400 {
			t.Fatalf("%s without a reason: %d, want 400", p, c)
		}
		if c, _ := call(t, ts, "POST", p, opTok, `{"reason":"  "}`); c != 400 {
			t.Fatalf("%s with a blank reason: %d, want 400", p, c)
		}
	}
	if c, _ := call(t, ts, "POST", "/v1/routines/triage/cycle/resume", opTok, `{"reason":"backup done"}`); c != 200 {
		t.Fatalf("resume with a reason: %d", c)
	}
	if ev, _ := st.Events(context.Background(), 1); ev[0].Detail != "0: backup done" {
		t.Fatalf("the reason is in the event: %q", ev[0].Detail)
	}
	if c, _ := call(t, ts, "POST", "/v1/routines/triage/cycle/autonomy", opTok, `{"autonomy":"propose","reason":"r"}`); c != 200 {
		t.Fatalf("raise with a reason: %d", c)
	}
	// Lowering needs none.
	if c, _ := call(t, ts, "POST", "/v1/routines/triage/cycle/autonomy", opTok, `{"autonomy":"shadow"}`); c != 200 {
		t.Fatalf("lowering without a reason: %d", c)
	}
	if v, _ := st.Control(context.Background(), "triage/cycle", sched.KeyAutonomy); v != "shadow" {
		t.Fatalf("autonomy %q", v)
	}
}

func TestAlwaysAnAdmin(t *testing.T) {
	ts, st := server(t)
	_, out := call(t, ts, "POST", "/v1/users", opTok, `{"name":"ada","role":"admin"}`)
	a1 := out["token"].(string)
	if c, _ := call(t, ts, "POST", "/v1/users/ada/role", a1, `{"role":"operator"}`); c != 409 {
		t.Fatalf("the last admin demoting themself: %d, want 409", c)
	}
	if c, _ := call(t, ts, "POST", "/v1/users/ada/revoke", a1, ""); c != 409 {
		t.Fatalf("revoking the last admin: %d, want 409", c)
	}
	_, out = call(t, ts, "POST", "/v1/users", a1, `{"name":"bob","role":"admin"}`)
	a2 := out["token"].(string)
	// Two admins demote each other at once: exactly one may succeed.
	var wg sync.WaitGroup
	codes := make([]int, 2)
	for i, x := range []struct{ by, who string }{{a1, "bob"}, {a2, "ada"}} {
		wg.Add(1)
		go func(i int, by, who string) {
			defer wg.Done()
			codes[i], _ = call(t, ts, "POST", "/v1/users/"+who+"/role", by, `{"role":"viewer"}`)
		}(i, x.by, x.who)
	}
	wg.Wait()
	if n, _ := st.ActiveAdmins(context.Background()); n != 1 {
		t.Fatalf("after two concurrent demotions %d admins (codes %v), want exactly 1", n, codes)
	}
}

func TestUserNamesAndTokens(t *testing.T) {
	ts, _ := server(t)
	_, out := call(t, ts, "POST", "/v1/users", opTok, `{"name":"ada","role":"admin"}`)
	admin := out["token"].(string)
	for _, name := range []string{"operator", "claude-homelab", "flotillad", "worker-x", "A", "x", "1abc", "has space"} {
		if c, _ := call(t, ts, "POST", "/v1/users", admin, fmt.Sprintf(`{"name":%q,"role":"viewer"}`, name)); c != 409 && c != 400 {
			t.Errorf("name %q accepted: %d", name, c)
		}
	}
	if c, _ := call(t, ts, "POST", "/v1/users", admin, `{"name":"ada","role":"viewer"}`); c != 409 {
		t.Fatalf("a taken name: %d", c)
	}
	_, out = call(t, ts, "POST", "/v1/users", admin, `{"name":"vic","role":"viewer"}`)
	old := out["token"].(string)
	_, out = call(t, ts, "POST", "/v1/users/vic/rotate", admin, "")
	if c, _ := call(t, ts, "GET", "/v1/status", old, ""); c != 401 {
		t.Fatalf("the old token after a rotation: %d, want 401", c)
	}
	if c, _ := call(t, ts, "GET", "/v1/status", out["token"].(string), ""); c != 200 {
		t.Fatalf("the new token: %d", c)
	}
	call(t, ts, "POST", "/v1/users/vic/revoke", admin, "")
	if c, _ := call(t, ts, "GET", "/v1/status", out["token"].(string), ""); c != 401 {
		t.Fatalf("a revoked user's token: %d, want 401", c)
	}
	if c, _ := call(t, ts, "POST", "/v1/users", admin, `{"name":"vic","role":"viewer"}`); c != 409 {
		t.Fatalf("reusing a revoked name: %d, want 409", c)
	}
	_, out = call(t, ts, "POST", "/v1/users", admin, `{"name":"otto","role":"operator"}`)
	if c, _ := call(t, ts, "GET", "/v1/users", out["token"].(string), ""); c != 403 {
		t.Fatalf("an operator listing users: %d, want 403", c)
	}
}
