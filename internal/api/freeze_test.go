package api

import (
	"context"
	"testing"

	"github.com/chipi/flotilla/internal/sched"
)

// ADR-0014 decision 8 through the API: an agent may pull the freeze and never lift it; lifting needs a
// reason; status shows what runs next to what is set; raising autonomy while frozen is measured against
// what is set, not against the freeze's shadow.
func TestFreezeAPI(t *testing.T) {
	ts, st := server(t)
	ctx := context.Background()
	if c, _ := call(t, ts, "POST", "/v1/routines/triage/cycle/autonomy", opTok, `{"autonomy":"propose","reason":"r"}`); c != 200 {
		t.Fatalf("raise to propose: %d", c)
	}
	if c, out := call(t, ts, "POST", "/v1/freeze", agentTok, ""); c != 200 {
		t.Fatalf("an agent pulls the freeze: %d %v", c, out)
	}
	_, out := call(t, ts, "GET", "/v1/status", agentTok, "")
	fr := out["freeze"].(map[string]any)
	r := out["routines"].([]any)[0].(map[string]any)
	if fr["frozen"] != true || fr["by"] != "claude-homelab" || r["autonomy"] != "shadow" || r["autonomy_set"] != "propose" {
		t.Fatalf("status while frozen: freeze %v, routine autonomy %v set %v", fr, r["autonomy"], r["autonomy_set"])
	}
	// propose -> propose is no raise, even though propose is above what runs now.
	if c, out := call(t, ts, "POST", "/v1/routines/triage/cycle/autonomy", opTok, `{"autonomy":"propose"}`); c != 200 {
		t.Fatalf("setting what is already set while frozen: %d %v", c, out)
	}
	if c, _ := call(t, ts, "POST", "/v1/routines/triage/cycle/autonomy", opTok, `{"autonomy":"auto"}`); c != 400 {
		t.Fatalf("raising while frozen without a reason: %d, want 400", c)
	}
	if c, _ := call(t, ts, "POST", "/v1/unfreeze", agentTok, `{"reason":"r"}`); c != 403 {
		t.Fatalf("an agent lifting the freeze: %d, want 403", c)
	}
	if c, _ := call(t, ts, "POST", "/v1/unfreeze", opTok, ""); c != 400 {
		t.Fatalf("lifting without a reason: %d, want 400", c)
	}
	if c, _ := call(t, ts, "POST", "/v1/unfreeze", opTok, `{"reason":"incident over"}`); c != 200 {
		t.Fatalf("lifting with a reason: %d", c)
	}
	if ev, _ := st.Events(ctx, 1); ev[0].Detail != "0: incident over" || ev[0].Subject != sched.FreezeSubject {
		t.Fatalf("the unfreeze event: %+v", ev[0])
	}
	_, out = call(t, ts, "GET", "/v1/status", agentTok, "")
	if out["freeze"].(map[string]any)["frozen"] != false || out["routines"].([]any)[0].(map[string]any)["autonomy"] != "propose" {
		t.Fatalf("after the freeze: %v", out)
	}
}
