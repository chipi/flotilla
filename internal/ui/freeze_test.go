package ui

import (
	"context"
	"net/url"
	"strings"
	"testing"

	"github.com/chipi/flotilla/internal/sched"
)

// The freeze in the UI: an operator pulls it on Autonomy (Govern owns autonomy changes); every page's
// autonomy pill says so, for every role; lifting it needs a reason, as through the API; a viewer has no
// control and is refused if they post the form anyway.
func TestFreezeInTheUI(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	op := f.addOperator()
	ck, csrf := f.csrfOf(op)
	_, brakes := f.do("GET", "/autonomy", ck, nil, nil)
	if !strings.Contains(brakes, `action="/freeze"`) {
		t.Fatal("an operator's Autonomy page has no freeze control")
	}
	resp, _ := f.do("POST", "/freeze", ck, url.Values{"csrf": {csrf}}, nil)
	if resp.StatusCode != 303 || !strings.Contains(resp.Header.Get("Location"), "msg=") {
		t.Fatalf("freeze: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	if ev, _ := f.st.Events(ctx, 1); ev[0].Actor != "user:otto" || ev[0].Via != "ui" || ev[0].Subject != sched.FreezeSubject {
		t.Fatalf("the freeze event: %+v", ev[0])
	}
	vck, vcsrf := f.csrfOf(f.vic)
	for _, p := range []string{"/", "/cases", "/runs", "/fleets", "/fleets/triage", "/hosts", "/autonomy", "/history", "/account"} {
		_, body := f.do("GET", p, vck, nil, nil)
		if !strings.Contains(body, "autopill--frozen") || !strings.Contains(body, "Autonomy: frozen") {
			t.Errorf("%s: the autonomy pill does not say frozen for a viewer", p)
		}
	}
	_, vbrakes := f.do("GET", "/autonomy", vck, nil, nil)
	if strings.Contains(vbrakes, `action="/unfreeze"`) || !strings.Contains(vbrakes, "Frozen by otto") {
		t.Error("a viewer's Autonomy page shows the unfreeze control, or not who froze it")
	}
	resp, _ = f.do("POST", "/unfreeze", vck, url.Values{"csrf": {vcsrf}, "reason": {"r"}}, nil)
	if !strings.Contains(resp.Header.Get("Location"), "err=") {
		t.Fatalf("a viewer lifting the freeze: %s", resp.Header.Get("Location"))
	}
	resp, _ = f.do("POST", "/unfreeze", ck, url.Values{"csrf": {csrf}}, nil)
	if !strings.Contains(resp.Header.Get("Location"), "err=") {
		t.Fatalf("lifting without a reason: %s", resp.Header.Get("Location"))
	}
	if v, _ := f.st.Control(ctx, sched.FreezeSubject, sched.KeyFrozen); v != "1" {
		t.Fatalf("a refused unfreeze changed the freeze: %q", v)
	}
	resp, _ = f.do("POST", "/unfreeze", ck, url.Values{"csrf": {csrf}, "reason": {"incident over"}}, nil)
	if !strings.Contains(resp.Header.Get("Location"), "msg=") {
		t.Fatalf("lifting with a reason: %s", resp.Header.Get("Location"))
	}
	if _, body := f.do("GET", "/runs", vck, nil, nil); strings.Contains(body, "autopill--frozen") || !strings.Contains(body, "Autonomy: normal") {
		t.Error("the pill outlived the freeze")
	}
}

// An action's autonomy on the Autonomy page: every role sees it; an operator changes it through the same
// rules as the API (a promotion needs a reason); a viewer gets no form and is refused if they post one.
func TestActionAutonomyInTheUI(t *testing.T) {
	f := setup(t)
	op := f.addOperator()
	ck, csrf := f.csrfOf(op)
	_, page := f.do("GET", "/autonomy", ck, nil, nil)
	if !strings.Contains(page, "backup.rerun") || !strings.Contains(page, `action="/actions/backup.rerun/autonomy"`) {
		t.Fatal("an operator's Autonomy page has no action or no form")
	}
	// G13: the record toward the next level, with its bar
	if !strings.Contains(page, "shadow plans you judged right: 0 of 5") || !strings.Contains(page, `class="evbar"`) {
		t.Error("the Autonomy page has no evidence toward the next level")
	}
	// G14: the recurrence rule for each kind an action is for
	if !strings.Contains(page, "same target within 7 days of a close: linked to the earlier case, at most propose after a verified fix; 0 in 30 days") {
		t.Error("the Autonomy page has no recurrence rule per kind")
	}
	vck, vcsrf := f.csrfOf(f.vic)
	_, vpage := f.do("GET", "/autonomy", vck, nil, nil)
	if !strings.Contains(vpage, "backup.rerun") || strings.Contains(vpage, `action="/actions/`) {
		t.Fatal("a viewer does not see the action, or sees the form")
	}
	resp, _ := f.do("POST", "/actions/backup.rerun/autonomy", ck, url.Values{"csrf": {csrf}, "autonomy": {"propose"}}, nil)
	if !strings.Contains(resp.Header.Get("Location"), "err=") {
		t.Fatalf("a promotion without a reason: %s", resp.Header.Get("Location"))
	}
	resp, _ = f.do("POST", "/actions/backup.rerun/autonomy", ck, url.Values{"csrf": {csrf}, "autonomy": {"propose"}, "reason": {"5 shadow plans agreed"}}, nil)
	if !strings.Contains(resp.Header.Get("Location"), "msg=") {
		t.Fatalf("a promotion with a reason: %s", resp.Header.Get("Location"))
	}
	if v, _ := f.st.Control(context.Background(), "action:backup.rerun", "autonomy"); v != "propose" {
		t.Fatalf("the action's autonomy is %q", v)
	}
	resp, _ = f.do("POST", "/actions/backup.rerun/autonomy", vck, url.Values{"csrf": {vcsrf}, "autonomy": {"shadow"}}, nil)
	if !strings.Contains(resp.Header.Get("Location"), "err=") {
		t.Fatalf("a viewer changing it: %s", resp.Header.Get("Location"))
	}
}
