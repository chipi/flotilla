package ui

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chipi/flotilla/internal/sched"
	"github.com/chipi/flotilla/internal/spec"
	"github.com/chipi/flotilla/internal/versions"
	"github.com/chipi/flotilla/internal/worker"
)

type okWorker struct{}

func (okWorker) Run(ctx context.Context, r worker.Request) (*worker.Result, error) {
	z := 0
	return &worker.Result{Status: "ok", ExitCode: &z}, nil
}

// versionedFixture is the UI over triage/cycle as a routine folder with v1 live, v2 and v3 shadows.
func versionedFixture(t *testing.T, extra string) *fixture {
	t.Helper()
	d := t.TempDir()
	put := func(rel, body string) {
		p := filepath.Join(d, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	put("triage/cycle/routine.yaml", "fleet: triage\nroutine: cycle\ndescription: one pass\nenabled: true\nworker: triage\ntrigger: {every: 10m}\nautonomy: record\nfreshness: 35m\n"+extra)
	for _, v := range []string{"v1", "v2", "v3"} {
		put("triage/cycle/"+v+"/spec.yaml", "run: {command: \"echo "+v+"\", timeout: 20m}\n")
	}
	rs, err := spec.LoadVersions(d)
	if err != nil {
		t.Fatal(err)
	}
	f := setupWith(t, nil)
	bound, _, err := versions.Reconcile(context.Background(), f.st, rs)
	if err != nil {
		t.Fatal(err)
	}
	f.sch.SetRoutines(bound)
	f.sch.Workers = map[string]sched.Runner{"triage": okWorker{}}
	return f
}

func (f *fixture) post(path, ck, csrf string, form url.Values) string {
	f.t.Helper()
	form.Set("csrf", csrf)
	resp, _ := f.do("POST", path, ck, form, nil)
	if resp.StatusCode != 303 {
		f.t.Fatalf("POST %s: %d", path, resp.StatusCode)
	}
	loc, _ := url.QueryUnescape(resp.Header.Get("Location"))
	return loc
}

// The routine page lists every version with its state, hash, who moved it and why; the operator's forms
// promote, retire and roll back through the API's rules; a viewer sees the table and no form.
func TestRoutineVersions(t *testing.T) {
	f := versionedFixture(t, "")
	ck, csrf := f.csrfOf(f.addOperator())
	_, page := f.do("GET", "/fleets/triage/cycle", ck, nil, nil)
	for _, want := range []string{`id="versions"`, "v1", "v2", "v3", "chip--ok", "chip--neutral", "shadow: not run yet (arrives with step 2)",
		"none yet: shadow runs arrive with step 2", "the first version, live at deploy", "Promote…", "Retire…", "No compared pairs exist yet",
		`action="/routines/triage/cycle/versions/2/promote"`, `action="/routines/triage/cycle/versions/3/retire"`, `name="without_pairs"`,
		`href="https://github.com/chipi/flotilla/tree/main/fleets/triage/cycle/"`, ">fleets/triage/cycle/<",
		"A promote needs: <span class=\"exact\">pairs</span>"} {
		if !strings.Contains(page, want) {
			t.Errorf("the routine page lacks %q", want)
		}
	}
	if strings.Contains(page, "Roll back…") {
		t.Error("a roll back form with no previous version")
	}
	// a viewer, while v2 and v3 are shadows an operator could promote or retire, sees the table and no form
	vck0, _ := f.csrfOf(f.vic)
	_, vpage0 := f.do("GET", "/fleets/triage/cycle", vck0, nil, nil)
	if !strings.Contains(vpage0, `id="versions"`) || !strings.Contains(vpage0, "shadow: not run yet") {
		t.Error("a viewer does not see the versions table")
	}
	for _, not := range []string{"/versions/", "/rollback", "Promote…", "Retire…", "Roll back…"} {
		if strings.Contains(vpage0, not) {
			t.Errorf("a viewer's routine page has %q", not)
		}
	}
	// the autonomy box is pre-filled with what the routine is set to
	if !strings.Contains(page, `<option value="record" selected>`) {
		t.Error("promote's autonomy is not pre-filled")
	}

	// refused: no reason (nothing says why it is fine without pairs)
	loc := f.post("/routines/triage/cycle/versions/2/promote", ck, csrf, url.Values{"autonomy": {"record"}})
	if !strings.Contains(loc, "err=") || !strings.Contains(loc, "lacks the evidence its acceptance names (pairs)") {
		t.Fatalf("a promote with no reason: %s", loc)
	}
	// refused: a bad gate line
	loc = f.post("/routines/triage/cycle/versions/2/promote", ck, csrf, url.Values{"autonomy": {"record"}, "reason": {"x"}, "gates": {"replay=maybe"}})
	if !strings.Contains(loc, "err=") || !strings.Contains(loc, "NAME=pass|fail") {
		t.Fatalf("a bad gate: %s", loc)
	}
	loc = f.post("/routines/triage/cycle/versions/2/promote", ck, csrf, url.Values{"autonomy": {"propose"}, "reason": {"no pairs yet; read the diff"},
		"without_pairs": {"1"}, "gates": {"replay-corpus=pass@ci/12\n"}, "evals": {"run-7=pass"}})
	if !strings.Contains(loc, "msg=") || !strings.Contains(loc, "v2 of triage/cycle is live, at propose") {
		t.Fatalf("a promote: %s", loc)
	}
	_, page = f.do("GET", "/fleets/triage/cycle", ck, nil, nil)
	for _, want := range []string{"chip--wait", "otto: no pairs yet; read the diff", "promoted with: pairs · gate replay-corpus pass · eval run-7 pass · without: pairs",
		"Roll back…", `action="/routines/triage/cycle/rollback"`, "Make v1 live again?", `<option value="propose" selected>`} {
		if !strings.Contains(page, want) {
			t.Errorf("after the promote the page lacks %q", want)
		}
	}
	if ev, _ := f.st.LatestEvent(context.Background(), "version.promote", "triage/cycle@v2"); ev == nil || ev.Actor != "user:otto" || ev.Via != "ui" {
		t.Fatalf("the promote's event: %+v", ev)
	}
	if loc = f.post("/routines/triage/cycle/versions/3/retire", ck, csrf, url.Values{}); !strings.Contains(loc, "err=") {
		t.Fatalf("a retire with no reason: %s", loc)
	}
	if loc = f.post("/routines/triage/cycle/versions/3/retire", ck, csrf, url.Values{"reason": {"not needed"}}); !strings.Contains(loc, "msg=Retired v3") {
		t.Fatalf("a retire: %s", loc)
	}
	if loc = f.post("/routines/triage/cycle/versions/1/retire", ck, csrf, url.Values{"reason": {"x"}}); !strings.Contains(loc, "roll back or promote instead") {
		t.Fatalf("retiring the previous version: %s", loc)
	}
	if loc = f.post("/routines/triage/cycle/rollback", ck, csrf, url.Values{"autonomy": {"record"}}); !strings.Contains(loc, "err=") {
		t.Fatalf("a rollback with no reason: %s", loc)
	}
	loc = f.post("/routines/triage/cycle/rollback", ck, csrf, url.Values{"autonomy": {"record"}, "reason": {"v2 files too much"}})
	if !strings.Contains(loc, "msg=Rolled back triage/cycle: v1 is live again.") {
		t.Fatalf("a rollback: %s", loc)
	}
	_, page = f.do("GET", "/fleets/triage/cycle", ck, nil, nil)
	for _, want := range []string{"rolled back at", "by otto: v2 files too much", "retired", "not needed"} {
		if !strings.Contains(page, want) {
			t.Errorf("after the rollback the page lacks %q", want)
		}
	}
	if strings.Contains(page, "Roll back…") {
		t.Error("there is no previous version after a rollback")
	}
	if v, _ := f.st.Control(context.Background(), "triage/cycle", "autonomy"); v != "record" {
		t.Fatalf("the rollback's autonomy: %q", v)
	}

	// the Autonomy page's recent changes list the version events
	_, auto := f.do("GET", "/autonomy", ck, nil, nil)
	if !strings.Contains(auto, "You promoted v2 of triage/cycle.") || !strings.Contains(auto, "You rolled back v2 of triage/cycle.") {
		t.Error("the Autonomy page does not list the version changes")
	}

	// a viewer sees the table and no form; and cannot post one
	vck, vcsrf := f.csrfOf(f.vic)
	_, vpage := f.do("GET", "/fleets/triage/cycle", vck, nil, nil)
	if !strings.Contains(vpage, `id="versions"`) || strings.Contains(vpage, "/versions/") || strings.Contains(vpage, "/rollback") {
		t.Error("a viewer's routine page: no table, or a version form")
	}
	if loc = f.post("/routines/triage/cycle/versions/2/promote", vck, vcsrf, url.Values{"autonomy": {"record"}, "reason": {"x"}}); !strings.Contains(loc, "err=") {
		t.Fatalf("a viewer promoted: %s", loc)
	}
}

// With an acceptance that names evidence flotilla can hold, the promote form needs a reason only when the
// pairs (which shadows do not record yet) are declared.
func TestPromoteFormWithoutPairsAcceptance(t *testing.T) {
	f := versionedFixture(t, "acceptance: [replay]\n")
	ck, csrf := f.csrfOf(f.addOperator())
	_, page := f.do("GET", "/fleets/triage/cycle", ck, nil, nil)
	if strings.Contains(page, `rows="2" required data-needs></textarea></label>
              <label class="field">Gate`) {
		t.Error("the reason is required though no pairs are declared")
	}
	if loc := f.post("/routines/triage/cycle/versions/2/promote", ck, csrf, url.Values{"autonomy": {"record"}}); !strings.Contains(loc, "(replay)") {
		t.Fatalf("a promote with no gate result: %s", loc)
	}
	loc := f.post("/routines/triage/cycle/versions/2/promote", ck, csrf, url.Values{"autonomy": {"record"}, "gates": {"replay-corpus=pass"}})
	if !strings.Contains(loc, "msg=v2 of triage/cycle is live") {
		t.Fatalf("a promote with the declared evidence: %s", loc)
	}
}

// The decisions tab says what the ledger's stage shadow is.
func TestDecisionsLabelsV1sShadow(t *testing.T) {
	data, err := os.ReadFile("templates/fleet.html")
	if err != nil || !strings.Contains(string(data), "v1's word for record") {
		t.Fatalf("the decisions note: %v", err)
	}
}

// Phone width (360 to 430 px): the versions table is the site's stacking table (a .table-box, every cell with a
// data-label the CSS prints as its heading), and the forms are the site's own details/confirm. Measured in a
// real browser by deploy/e2e/ui_browser.py; here, the markup that rule needs, in each state.
func TestVersionsTableStacksOnAPhone(t *testing.T) {
	f := versionedFixture(t, "")
	ck, csrf := f.csrfOf(f.addOperator())
	f.post("/routines/triage/cycle/versions/2/promote", ck, csrf, url.Values{"autonomy": {"record"}, "reason": {"x"}})
	_, page := f.do("GET", "/fleets/triage/cycle", ck, nil, nil)
	sec := page[strings.Index(page, `id="versions"`):]
	sec = sec[:strings.Index(sec, "</section>")]
	if !strings.Contains(sec, `<div class="table-box"><table class="data data--small">`) {
		t.Error("the versions table is not the stacking table")
	}
	rows := strings.Split(sec, "<tr>")[2:] // a header row, then one per version
	if len(rows) != 3 {
		t.Fatalf("%d version rows, want 3 (live, previous, shadow)", len(rows))
	}
	for _, r := range rows {
		cells := strings.Count(r, "<td")
		if cells != strings.Count(r, "data-label=") || cells != 8 {
			t.Errorf("a row has %d cells and %d data-labels, want 8 of each: %s", cells, strings.Count(r, "data-label="), r)
		}
	}
	for _, want := range []string{"chip--ok", "chip--wait", "chip--neutral"} {
		if !strings.Contains(sec, want) {
			t.Errorf("no row in the state %s", want)
		}
	}
	// the long texts (a why, a hash) are in cells that stack and wrap; no fixed-width element is added
	if strings.Contains(sec, "width=") || strings.Contains(sec, "nowrap") {
		t.Error("a fixed width in the versions section")
	}
}
