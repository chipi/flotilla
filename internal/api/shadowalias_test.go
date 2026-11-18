package api

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/chipi/flotilla/internal/sched"
	"github.com/chipi/flotilla/internal/spec"
)

const shadowNote = "shadow is now called record"

// shadow, the old name of the lowest level, still works everywhere a level is given: it is stored as record and
// the answer says so.
func TestShadowIsAnAliasOfRecordOverHTTP(t *testing.T) {
	ctx := context.Background()
	ts, st := serverWith(t, func(*spec.Routine) {})
	c, out := call(t, ts, "POST", "/v1/routines/triage/cycle/autonomy", opTok, `{"autonomy":"shadow"}`)
	if c != 200 || out["autonomy"] != "record" || out["note"] != shadowNote {
		t.Fatalf("routine autonomy shadow: %d %v", c, out)
	}
	if v, _ := st.Control(ctx, "triage/cycle", sched.KeyAutonomy); v != "record" {
		t.Fatalf("stored %q, want record", v)
	}
	if c, out := call(t, ts, "POST", "/v1/routines/triage/cycle/autonomy", opTok, `{"autonomy":"record"}`); c != 200 || out["note"] != nil {
		t.Fatalf("record carries no note: %d %v", c, out)
	}

	c, out = call(t, ts, "POST", "/v1/actions/"+rerun+"/autonomy", opTok, `{"autonomy":"shadow"}`)
	if c != 200 || out["autonomy"] != "record" || out["note"] != shadowNote {
		t.Fatalf("action autonomy shadow: %d %v", c, out)
	}
	if v, _ := st.Control(ctx, actionSubject(rerun), "autonomy"); v != "record" {
		t.Fatalf("action stored %q, want record", v)
	}
	if c, out := call(t, ts, "POST", "/v1/routines/triage/cycle/autonomy", opTok, `{"autonomy":"yolo"}`); c != 400 || !strings.Contains(out["error"].(string), "record, propose or auto") {
		t.Fatalf("an unknown level is still refused: %d %v", c, out)
	}
}

func TestShadowIsAnAliasOfRecordForVersions(t *testing.T) {
	ctx := context.Background()
	s := versioned(t, "")
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	c, out := call(t, ts, "POST", "/v1/routines/triage/cycle/versions/2/promote", opTok, `{"autonomy":"shadow","reason":"no pairs yet; the diff was read"}`)
	if c != 200 || out["autonomy"] != "record" || out["note"] != shadowNote {
		t.Fatalf("promote shadow: %d %v", c, out)
	}
	if v, _ := s.Store.Control(ctx, "triage/cycle", sched.KeyAutonomy); v != "record" {
		t.Fatalf("promote stored %q, want record", v)
	}
	c, out = call(t, ts, "POST", "/v1/routines/triage/cycle/rollback", opTok, `{"autonomy":"shadow","reason":"v2 files too much"}`)
	if c != 200 || out["note"] != shadowNote {
		t.Fatalf("rollback shadow: %d %v", c, out)
	}
	if v, _ := s.Store.Control(ctx, "triage/cycle", sched.KeyAutonomy); v != "record" {
		t.Fatalf("rollback stored %q, want record", v)
	}
}

// The Go entry points the UI calls take it too.
func TestShadowIsAnAliasOfRecordInTheActions(t *testing.T) {
	ctx := context.Background()
	s := newServer(t, func(*spec.Routine) {})
	if err := s.SetAutonomy(ctx, opCaller, "test", "triage/cycle", "shadow", ""); err != nil {
		t.Fatal(err)
	}
	if v, _ := s.Store.Control(ctx, "triage/cycle", sched.KeyAutonomy); v != "record" {
		t.Fatalf("stored %q", v)
	}
	if err := s.SetActionAutonomy(ctx, opCaller, "test", rerun, "shadow", ""); err != nil {
		t.Fatal(err)
	}
	if v, _ := s.Store.Control(ctx, actionSubject(rerun), "autonomy"); v != "record" {
		t.Fatalf("action stored %q", v)
	}
}
