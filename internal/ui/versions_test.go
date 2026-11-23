package ui

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

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
	return versionedFixtureN(t, extra, 3)
}

// versionedFixtureN is versionedFixture with n version folders, v1 live and the rest shadows.
func versionedFixtureN(t *testing.T, extra string, n int) *fixture {
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
	for i := 1; i <= n; i++ {
		v := "v" + strconv.Itoa(i)
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

// versionsSection is the routine page's #versions section and nothing else, so an assertion about the table
// cannot be satisfied by a "v1" or a chip class somewhere else on the page.
func versionsSection(t *testing.T, page string) string {
	t.Helper()
	i := strings.Index(page, `id="versions"`)
	if i < 0 {
		t.Fatal("the page has no #versions section")
	}
	sec := page[i:]
	j := strings.Index(sec, "</section>")
	if j < 0 {
		t.Fatal("the #versions section is not closed")
	}
	return sec[:j]
}

func (f *fixture) versionStates() string {
	f.t.Helper()
	rows, err := f.st.AllVersions(context.Background())
	if err != nil {
		f.t.Fatal(err)
	}
	var out []string
	for _, r := range rows {
		out = append(out, "v"+strconv.Itoa(r.Version)+"="+r.State)
	}
	return strings.Join(out, " ")
}

func (f *fixture) promoteV2(ck, csrf string) {
	f.t.Helper()
	loc := f.post("/routines/triage/cycle/versions/2/promote", ck, csrf, url.Values{"autonomy": {"propose"}, "reason": {"no pairs yet; read the diff"},
		"without_pairs": {"1"}, "gates": {"replay-corpus=pass@ci/12\n"}, "evals": {"run-7=pass"}})
	if !strings.Contains(loc, "msg=") {
		f.t.Fatalf("a promote: %s", loc)
	}
}

// The routine page lists every version with its state, hash, who moved it and why; the operator's forms
// promote, retire and roll back through the API's rules; a viewer sees the table and no form. Each case
// builds its own state, so one failing does not hide the next.
func TestRoutineVersions(t *testing.T) {
	t.Run("the table", func(t *testing.T) {
		f := versionedFixture(t, "")
		ck, _ := f.csrfOf(f.addOperator())
		_, page := f.do("GET", "/fleets/triage/cycle", ck, nil, nil)
		sec := versionsSection(t, page)
		for _, want := range []string{"v1", "v2", "v3", "chip--ok", "chip--neutral", "shadow: runs beside v1 when shadows are on",
			"none yet", "the first version, live at deploy", "Promote…", "Retire…", "Compared pairs so far: none yet",
			`action="/routines/triage/cycle/versions/2/promote"`, `action="/routines/triage/cycle/versions/3/retire"`, `name="without_pairs"`,
			"A promote needs: <span class=\"exact\">pairs</span>"} {
			if !strings.Contains(sec, want) {
				t.Errorf("the versions section lacks %q", want)
			}
		}
		for _, want := range []string{`href="https://github.com/chipi/flotilla/tree/main/fleets/triage/cycle/"`, ">fleets/triage/cycle/<"} {
			if !strings.Contains(page, want) {
				t.Errorf("the routine page lacks %q", want)
			}
		}
		if strings.Contains(sec, "Roll back…") {
			t.Error("a roll back form with no previous version")
		}
		// the autonomy box is pre-filled with what the routine is set to
		if !strings.Contains(sec, `<option value="record" selected>`) {
			t.Error("promote's autonomy is not pre-filled")
		}
	})

	t.Run("a viewer sees the table and no form", func(t *testing.T) {
		f := versionedFixture(t, "")
		ck, csrf := f.csrfOf(f.addOperator())
		f.promoteV2(ck, csrf) // so a roll back form exists for an operator to have
		vck, _ := f.csrfOf(f.vic)
		_, page := f.do("GET", "/fleets/triage/cycle", vck, nil, nil)
		sec := versionsSection(t, page)
		if !strings.Contains(sec, "v3") || !strings.Contains(sec, "shadow: runs beside v") {
			t.Error("a viewer does not see the versions table")
		}
		for _, not := range []string{"/versions/", "/rollback", "Promote…", "Retire…", "Roll back…"} {
			if strings.Contains(sec, not) {
				t.Errorf("a viewer's versions section has %q", not)
			}
		}
	})

	t.Run("promote refusals", func(t *testing.T) {
		f := versionedFixture(t, "")
		ck, csrf := f.csrfOf(f.addOperator())
		// no reason: nothing says why it is fine without pairs
		loc := f.post("/routines/triage/cycle/versions/2/promote", ck, csrf, url.Values{"autonomy": {"record"}})
		if !strings.Contains(loc, "err=") || !strings.Contains(loc, "lacks the evidence its acceptance names (pairs)") {
			t.Fatalf("a promote with no reason: %s", loc)
		}
		loc = f.post("/routines/triage/cycle/versions/2/promote", ck, csrf, url.Values{"autonomy": {"record"}, "reason": {"x"}, "gates": {"replay=maybe"}})
		if !strings.Contains(loc, "err=") || !strings.Contains(loc, "NAME=pass|fail") {
			t.Fatalf("a bad gate: %s", loc)
		}
		if got := f.versionStates(); got != "v1=live v2=shadow v3=shadow" {
			t.Fatalf("a refused promote changed the states: %s", got)
		}
	})

	t.Run("promote", func(t *testing.T) {
		f := versionedFixture(t, "")
		ck, csrf := f.csrfOf(f.addOperator())
		loc := f.post("/routines/triage/cycle/versions/2/promote", ck, csrf, url.Values{"autonomy": {"propose"}, "reason": {"no pairs yet; read the diff"},
			"without_pairs": {"1"}, "gates": {"replay-corpus=pass@ci/12\n"}, "evals": {"run-7=pass"}})
		if !strings.Contains(loc, "msg=") || !strings.Contains(loc, "v2 of triage/cycle is live, at propose") {
			t.Fatalf("a promote: %s", loc)
		}
		_, page := f.do("GET", "/fleets/triage/cycle", ck, nil, nil)
		sec := versionsSection(t, page)
		for _, want := range []string{"chip--wait", "otto: no pairs yet; read the diff", "promoted with: pairs · gate replay-corpus pass · eval run-7 pass · without: pairs",
			"Roll back…", `action="/routines/triage/cycle/rollback"`, "Make v1 live again?", `<option value="propose" selected>`} {
			if !strings.Contains(sec, want) {
				t.Errorf("after the promote the versions section lacks %q", want)
			}
		}
		if ev, _ := f.st.LatestEvent(context.Background(), "version.promote", "triage/cycle@v2"); ev == nil || ev.Actor != "user:otto" || ev.Via != "ui" {
			t.Fatalf("the promote's event: %+v", ev)
		}
		_, auto := f.do("GET", "/autonomy", ck, nil, nil)
		if !strings.Contains(auto, "You promoted v2 of triage/cycle.") {
			t.Error("the Autonomy page does not list the promote")
		}
	})

	t.Run("retire", func(t *testing.T) {
		f := versionedFixture(t, "")
		ck, csrf := f.csrfOf(f.addOperator())
		f.promoteV2(ck, csrf)
		if loc := f.post("/routines/triage/cycle/versions/3/retire", ck, csrf, url.Values{}); !strings.Contains(loc, "err=") {
			t.Fatalf("a retire with no reason: %s", loc)
		}
		if loc := f.post("/routines/triage/cycle/versions/3/retire", ck, csrf, url.Values{"reason": {"not needed"}}); !strings.Contains(loc, "msg=Retired v3") {
			t.Fatalf("a retire: %s", loc)
		}
		if loc := f.post("/routines/triage/cycle/versions/1/retire", ck, csrf, url.Values{"reason": {"x"}}); !strings.Contains(loc, "roll back or promote instead") {
			t.Fatalf("retiring the previous version: %s", loc)
		}
		if got := f.versionStates(); got != "v1=previous v2=live v3=retired" {
			t.Fatalf("states %s", got)
		}
	})

	t.Run("roll back", func(t *testing.T) {
		f := versionedFixture(t, "")
		ck, csrf := f.csrfOf(f.addOperator())
		f.promoteV2(ck, csrf)
		if loc := f.post("/routines/triage/cycle/rollback", ck, csrf, url.Values{"autonomy": {"record"}}); !strings.Contains(loc, "err=") {
			t.Fatalf("a rollback with no reason: %s", loc)
		}
		loc := f.post("/routines/triage/cycle/rollback", ck, csrf, url.Values{"autonomy": {"record"}, "reason": {"v2 files too much"}})
		if !strings.Contains(loc, "msg=Rolled back triage/cycle: v1 is live again.") {
			t.Fatalf("a rollback: %s", loc)
		}
		_, page := f.do("GET", "/fleets/triage/cycle", ck, nil, nil)
		sec := versionsSection(t, page)
		for _, want := range []string{"rolled back at", "by otto: v2 files too much", "retired"} {
			if !strings.Contains(sec, want) {
				t.Errorf("after the rollback the versions section lacks %q", want)
			}
		}
		if strings.Contains(sec, "Roll back…") {
			t.Error("there is no previous version after a rollback")
		}
		if v, _ := f.st.Control(context.Background(), "triage/cycle", "autonomy"); v != "record" {
			t.Fatalf("the rollback's autonomy: %q", v)
		}
		_, auto := f.do("GET", "/autonomy", ck, nil, nil)
		if !strings.Contains(auto, "You rolled back v2 of triage/cycle.") {
			t.Error("the Autonomy page does not list the rollback")
		}
	})

	t.Run("a viewer cannot post", func(t *testing.T) {
		f := versionedFixture(t, "")
		vck, vcsrf := f.csrfOf(f.vic)
		if loc := f.post("/routines/triage/cycle/versions/2/promote", vck, vcsrf, url.Values{"autonomy": {"record"}, "reason": {"x"}}); !strings.Contains(loc, "err=") {
			t.Fatalf("a viewer promoted: %s", loc)
		}
	})
}

// A viewer's POST to retire or roll back is refused and changes nothing; a POST with no CSRF token never reaches
// the handler; a version that is not a positive number is a 404.
func TestVersionFormsRefuseWhatTheyShould(t *testing.T) {
	f := versionedFixture(t, "")
	ck, csrf := f.csrfOf(f.addOperator())
	f.promoteV2(ck, csrf) // v1 previous, v2 live, v3 shadow: every form has something to act on
	want := f.versionStates()
	if want != "v1=previous v2=live v3=shadow" {
		t.Fatal(want)
	}
	vck, vcsrf := f.csrfOf(f.vic)
	for _, p := range []struct{ path, form string }{
		{"/routines/triage/cycle/versions/3/retire", "reason"},
		{"/routines/triage/cycle/rollback", "reason"},
	} {
		loc := f.post(p.path, vck, vcsrf, url.Values{"autonomy": {"record"}, "reason": {"x"}})
		if !strings.Contains(loc, "err=") {
			t.Errorf("a viewer's POST to %s: %s", p.path, loc)
		}
	}
	for _, path := range []string{"/routines/triage/cycle/versions/3/retire", "/routines/triage/cycle/rollback", "/routines/triage/cycle/versions/3/promote"} {
		resp, _ := f.do("POST", path, ck, url.Values{"autonomy": {"record"}, "reason": {"x"}}, nil) // no csrf field
		if resp.StatusCode != 403 {
			t.Errorf("POST %s without a CSRF token: %d, want 403", path, resp.StatusCode)
		}
	}
	for _, n := range []string{"abc", "0", "-1"} {
		for _, action := range []string{"promote", "retire"} {
			resp, _ := f.do("POST", "/routines/triage/cycle/versions/"+n+"/"+action, ck, url.Values{"csrf": {csrf}, "autonomy": {"record"}, "reason": {"x"}}, nil)
			if resp.StatusCode != 404 {
				t.Errorf("POST /versions/%s/%s: %d, want 404", n, action, resp.StatusCode)
			}
		}
	}
	if got := f.versionStates(); got != want {
		t.Fatalf("a refused form changed the states: %s", got)
	}
}

// A routine that is a flat spec has one legacy v1, live: the table has that row, the .yaml link and no form; a
// routine with no recorded versions gets the section's empty line; a reason with markup is escaped.
func TestVersionsPageStates(t *testing.T) {
	t.Run("a flat routine", func(t *testing.T) {
		d := t.TempDir()
		p := filepath.Join(d, "triage", "cycle.yaml")
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		flat := "fleet: triage\nroutine: cycle\ndescription: one pass\nenabled: true\nworker: triage\ntrigger: {every: 10m}\nautonomy: record\nfreshness: 35m\nrun: {command: \"echo flat\", timeout: 20m}\n"
		if err := os.WriteFile(p, []byte(flat), 0o644); err != nil {
			t.Fatal(err)
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
		ck, _ := f.csrfOf(f.addOperator())
		_, page := f.do("GET", "/fleets/triage/cycle", ck, nil, nil)
		sec := versionsSection(t, page)
		if rows := strings.Split(sec, "<tr>"); len(rows) != 3 { // before the header, the header, one row
			t.Fatalf("%d parts, want the header and one version: %s", len(rows), sec)
		}
		for _, want := range []string{"v1", "chip--ok"} {
			if !strings.Contains(sec, want) {
				t.Errorf("a flat routine's versions section lacks %q", want)
			}
		}
		for _, not := range []string{"Promote…", "Retire…", "Roll back…", "<form"} {
			if strings.Contains(sec, not) {
				t.Errorf("a flat routine has one version and no form, but the section has %q", not)
			}
		}
		if !strings.Contains(page, "fleets/triage/cycle.yaml") {
			t.Error("a flat routine links its .yaml")
		}
	})

	t.Run("no recorded versions", func(t *testing.T) {
		f := setup(t) // the routine is loaded, nothing was reconciled into the store
		ck, _ := f.csrfOf(f.addOperator())
		resp, page := f.do("GET", "/fleets/triage/cycle", ck, nil, nil)
		if resp.StatusCode != 200 {
			t.Fatalf("a routine with no recorded versions: %d", resp.StatusCode)
		}
		if sec := versionsSectionOrEmpty(page); strings.Contains(sec, "<form") || strings.Contains(sec, "Promote…") {
			t.Errorf("a form in a versions section with no versions: %s", sec)
		}
	})

	t.Run("a reason is escaped", func(t *testing.T) {
		f := versionedFixture(t, "")
		ck, csrf := f.csrfOf(f.addOperator())
		f.post("/routines/triage/cycle/versions/3/retire", ck, csrf, url.Values{"reason": {`<script>alert("x")</script>`}})
		_, page := f.do("GET", "/fleets/triage/cycle", ck, nil, nil)
		sec := versionsSection(t, page)
		if strings.Contains(sec, "<script>alert") || !strings.Contains(sec, "&lt;script&gt;alert") {
			t.Errorf("a reason with markup is not escaped: %s", sec)
		}
	})
}

func versionsSectionOrEmpty(page string) string {
	i := strings.Index(page, `id="versions"`)
	if i < 0 {
		return ""
	}
	sec := page[i:]
	if j := strings.Index(sec, "</section>"); j >= 0 {
		return sec[:j]
	}
	return sec
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

// The decisions tab says what the ledger's stage shadow is: rendered, with a decision that carries it, not read
// from the template's bytes.
func TestDecisionsLabelsV1sShadow(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	ck, _ := f.csrfOf(f.addOperator())
	now := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
	for i, stage := range []string{"propose", "shadow"} {
		if _, err := f.st.AppendEntry(ctx, "triage/cycle", "decision", map[string]string{"ts": now, "disposition": "file", "stage": stage,
			"occurrence_id": "o" + strconv.Itoa(i), "fingerprint": "glitchtip:SHOP-PROD-25"}, "r1"); err != nil {
			t.Fatal(err)
		}
	}
	_, page := f.do("GET", "/fleets/triage?tab=decisions", ck, nil, nil)
	if !strings.Contains(page, "1 with the stage shadow, v1&#39;s word for record, are not counted") && !strings.Contains(page, "1 with the stage shadow, v1's word for record, are not counted") {
		t.Fatalf("the decisions tab does not say what stage shadow is: %s", page)
	}
}

// Phone width (360 to 430 px): the versions table is the site's stacking table (a .table-box, every cell with a
// data-label the CSS prints as its heading), and the forms are the site's own details/confirm. What this checks is
// the markup that rule needs, in each state. The width itself is measured only in a real browser
// (deploy/e2e/ui_browser.py: no sideways scroll at 360 to 430 px); the absence of a fixed width in the markup,
// checked last, is necessary and not sufficient, since the CSS could add one.
func TestVersionsTableStacksOnAPhone(t *testing.T) {
	f := versionedFixture(t, "")
	ck, csrf := f.csrfOf(f.addOperator())
	f.post("/routines/triage/cycle/versions/2/promote", ck, csrf, url.Values{"autonomy": {"record"}, "reason": {"x"}})
	_, page := f.do("GET", "/fleets/triage/cycle", ck, nil, nil)
	sec := versionsSection(t, page)
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
	if strings.Contains(sec, "width=") || strings.Contains(sec, "nowrap") {
		t.Error("a fixed width in the versions section")
	}
}
