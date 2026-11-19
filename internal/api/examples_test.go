package api

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chipi/flotilla/internal/spec"
)

// examplesServer is a server over the installation's one routine (triage/cycle) with the repo's own example fleets as its catalogue,
// which hold an example-triage/cycle beside it.
func examplesServer(t *testing.T) (*httptest.Server, *Server) {
	t.Helper()
	srv := newServer(t, func(*spec.Routine) {})
	c, err := spec.LoadExamples(filepath.Join("..", "..", "fleets"))
	if err != nil {
		t.Fatal(err)
	}
	srv.Examples = c
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, srv
}

// The status lists the examples apart from the routines: the installation's own routine is the only one in routines, and an example has the
// state the CLI shows, no run, no autonomy.
func TestStatusListsExamplesApartFromRoutines(t *testing.T) {
	ts, _ := examplesServer(t)
	code, out := call(t, ts, "GET", "/v1/status", opTok, "")
	if code != 200 {
		t.Fatal(code)
	}
	routines, _ := out["routines"].([]any)
	if len(routines) != 1 || routines[0].(map[string]any)["id"] != "triage/cycle" {
		t.Fatalf("routines: %v", routines)
	}
	ex, _ := out["examples"].([]any)
	if len(ex) < 30 {
		t.Fatalf("%d examples", len(ex))
	}
	ids := map[string]map[string]any{}
	for _, e := range ex {
		m := e.(map[string]any)
		ids[m["id"].(string)] = m
		if _, has := m["autonomy"]; has {
			t.Errorf("an example has no autonomy: %v", m)
		}
	}
	if ids["example-triage/cycle"] == nil || ids["example-triage/cycle"]["trigger"] != "every 10m" || ids["triage/cycle"] != nil {
		t.Errorf("examples: %v", ids["example-triage/cycle"])
	}
}

// Without a catalogue the status has no examples key at all.
func TestStatusWithoutACatalogueHasNoExamples(t *testing.T) {
	ts, _ := server(t)
	_, out := call(t, ts, "GET", "/v1/status", opTok, "")
	if _, has := out["examples"]; has {
		t.Errorf("examples key without a catalogue: %v", out["examples"])
	}
}

// Everything that names a routine refuses an example's id, and says it is an example: a run, a dry run, pause, the version controls, a
// shadow run. A routine the installation does not have is still a 404.
func TestNoActionAcceptsAnExampleAndNoneStartsARun(t *testing.T) {
	ts, srv := examplesServer(t)
	id := "example-triage/cycle"
	for _, c := range []struct{ method, path, body string }{
		{"POST", "/v1/routines/" + id + "/run", ""},
		{"POST", "/v1/routines/" + id + "/pause", `{"reason":"x"}`},
		{"POST", "/v1/routines/" + id + "/resume", `{"reason":"x"}`},
		{"GET", "/v1/routines/" + id + "/versions", ""},
		{"POST", "/v1/routines/" + id + "/versions/2/promote", `{"autonomy":"record","reason":"x"}`},
		{"POST", "/v1/routines/" + id + "/versions/2/retire", `{"reason":"x"}`},
		{"POST", "/v1/routines/" + id + "/versions/2/shadow-run", `{"input":"i"}`},
	} {
		code, out := call(t, ts, c.method, c.path, opTok, c.body)
		msg, _ := out["error"].(string)
		if code != 409 || !strings.Contains(msg, "is an example") {
			t.Errorf("%s %s: %d %v", c.method, c.path, code, out)
		}
	}
	if code, _ := call(t, ts, "POST", "/v1/routines/example-nothing/here/run", opTok, ""); code != 404 {
		t.Errorf("an id the catalogue does not have: %d", code)
	}
	if err := srv.RunNow(context.Background(), Caller{Name: "operator", Role: RoleOperator, TokenSHA256: HashToken(opTok)}, "test", id); err == nil || !strings.Contains(err.Error(), "is an example") {
		t.Errorf("RunNow: %v", err)
	}
	if _, err := srv.DryRunNow(context.Background(), Caller{Name: "operator", Role: RoleOperator, TokenSHA256: HashToken(opTok)}, "test", id); err == nil || !strings.Contains(err.Error(), "is an example") {
		t.Errorf("DryRunNow: %v", err)
	}
	runs, err := srv.Store.Runs(context.Background(), "", 50)
	if err != nil || len(runs) != 0 {
		t.Errorf("a run exists after refused requests: %v %v", runs, err)
	}
}

// A routine the installation's config disables does not run by hand either (a spec's enabled: false does: the e2e's by-hand routines).
func TestADisabledByConfigRoutineDoesNotRunByHand(t *testing.T) {
	ts, srv := serverWith2(t, func(j *spec.Routine) { j.Enabled, j.ConfigDisabled = false, true })
	code, out := call(t, ts, "POST", "/v1/routines/triage/cycle/run", opTok, "")
	if msg, _ := out["error"].(string); code != 409 || !strings.Contains(msg, "disabled by this installation's configuration") {
		t.Fatalf("%d %v", code, out)
	}
	if _, err := srv.Sched.RunNow(context.Background(), "triage/cycle", "manual", "operator"); err == nil || !strings.Contains(err.Error(), "disabled_routines") {
		t.Errorf("the scheduler runs it: %v", err)
	}
	if runs, _ := srv.Store.Runs(context.Background(), "", 5); len(runs) != 0 {
		t.Errorf("a run: %v", runs)
	}
}

func TestASpecDisabledRoutineStillRunsByHand(t *testing.T) {
	ts, srv := serverWith2(t, func(j *spec.Routine) { j.Enabled = false })
	if code, out := call(t, ts, "POST", "/v1/routines/triage/cycle/run", opTok, ""); code != 202 && code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	_ = srv
}

func serverWith2(t *testing.T, mod func(*spec.Routine)) (*httptest.Server, *Server) {
	t.Helper()
	srv := newServer(t, mod)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, srv
}
