package ui

import (
	"context"
	"net/url"
	"strings"
	"testing"

	"github.com/chipi/flotilla/internal/sched"
)

// The freeze in the UI: an operator pulls it on Now; every page says so, for every role; lifting it needs a
// reason, as through the API; a viewer has no control and is refused if they post the form anyway.
func TestFreezeInTheUI(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	op := f.addOperator()
	ck, csrf := f.csrfOf(op)
	_, home := f.do("GET", "/", ck, nil, nil)
	if !strings.Contains(home, `action="/freeze"`) {
		t.Fatal("an operator's Now page has no freeze control")
	}
	resp, _ := f.do("POST", "/freeze", ck, url.Values{"csrf": {csrf}}, nil)
	if resp.StatusCode != 303 || !strings.Contains(resp.Header.Get("Location"), "msg=") {
		t.Fatalf("freeze: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	if ev, _ := f.st.Events(ctx, 1); ev[0].Actor != "user:otto" || ev[0].Via != "ui" || ev[0].Subject != sched.FreezeSubject {
		t.Fatalf("the freeze event: %+v", ev[0])
	}
	vck, vcsrf := f.csrfOf(f.vic)
	for _, p := range []string{"/", "/runs", "/fleets", "/fleets/triage", "/autonomy", "/history"} {
		_, body := f.do("GET", p, vck, nil, nil)
		if !strings.Contains(body, "<strong>Frozen.</strong>") {
			t.Errorf("%s: no freeze banner for a viewer", p)
		}
	}
	_, vhome := f.do("GET", "/", vck, nil, nil)
	if strings.Contains(vhome, `action="/unfreeze"`) {
		t.Error("a viewer's Now page shows the unfreeze control")
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
	if _, body := f.do("GET", "/runs", vck, nil, nil); strings.Contains(body, "<strong>Frozen.</strong>") {
		t.Error("the banner outlived the freeze")
	}
}
