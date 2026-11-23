package ui

import (
	"fmt"
	"html"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/chipi/flotilla/internal/spec"
)

var actionRowRe = regexp.MustCompile(`<tr id="action-([a-z_.]+)"`)

func actionNames(page string) []string {
	var out []string
	for _, m := range actionRowRe.FindAllStringSubmatch(page, -1) {
		out = append(out, m[1])
	}
	return out
}

// Autonomy's two tables sort, filter and page like every other table (the operator, 2026-11-14 and 2026-11-23), each with
// keys of its own, so moving one leaves the other where it was, and each link lands back on its table.
func TestAutonomyTablesSortFilterAndPage(t *testing.T) {
	var routines []*spec.Routine
	for i := 0; i < 12; i++ {
		a := "record"
		if i%4 == 0 {
			a = "propose"
		}
		routines = append(routines, &spec.Routine{Fleet: "ops", Routine: fmt.Sprintf("r%02d", i), Description: "d", Enabled: true, Worker: "w", Autonomy: a,
			Run: spec.Run{Command: "true"}, Limit: time.Minute})
	}
	f := setupWith(t, routines)
	ck := f.signin(f.addOperator())

	_, page := f.do("GET", "/autonomy", ck, nil, nil)
	all := actionNames(page)
	if len(all) != autonomyPer {
		t.Fatalf("the first page holds %d actions, want %d", len(all), autonomyPer)
	}
	for _, want := range []string{`href="/autonomy?sort=action#caps"`, `href="/autonomy?rsort=at#routines"`, `name="rq"`, `name="q"`,
		`href="/autonomy?level=ready#caps"`, `href="/autonomy?rlevel=propose#routines"`, `rel="next"`} {
		if !strings.Contains(page, want) {
			t.Errorf("Autonomy lacks %q", want)
		}
	}

	_, page = f.do("GET", "/autonomy?sort=action&dir=desc", ck, nil, nil)
	names := actionNames(page)
	for i := 1; i < len(names); i++ {
		if names[i-1] < names[i] {
			t.Fatalf("sorted by action, descending: %v", names)
		}
	}

	_, page = f.do("GET", "/autonomy?q=plumbing", ck, nil, nil)
	for _, n := range actionNames(page) {
		if !strings.HasPrefix(n, "plumbing.") {
			t.Errorf("a search for plumbing shows %s", n)
		}
	}

	// the routines' filter keeps the actions' sort, and the other way round
	_, page = f.do("GET", "/autonomy?sort=action&rlevel=propose", ck, nil, nil)
	page = html.UnescapeString(page)
	if n := strings.Count(page, `<a class="exact" href="/fleets/ops?sel=`); n != 3 {
		t.Errorf("%d routines at propose shown, want 3", n)
	}
	if !strings.Contains(page, `href="/autonomy?rlevel=propose&sort=risk#caps"`) {
		t.Error("an actions sort link drops the routines' filter")
	}
	if !strings.Contains(page, `href="/autonomy?rdir=desc&rlevel=propose&rsort=routine&sort=action#routines"`) {
		t.Error("a routines sort link drops the actions' sort")
	}
}

// Wrong right now is a card beside Needs you: three items, the rest folded behind "Show all N", so Needs you leads the page.
func TestWrongRightNowIsFoldedBesideNeedsYou(t *testing.T) {
	var routines []*spec.Routine
	for i := 0; i < 5; i++ {
		routines = append(routines, &spec.Routine{Fleet: "ops", Routine: fmt.Sprintf("r%d", i), Description: "d", Enabled: true, Worker: "w", Autonomy: "record",
			Run: spec.Run{Command: "true"}, Limit: time.Minute})
	}
	f := setupWith(t, routines)
	for _, rt := range routines {
		_ = f.st.SetControl(t.Context(), rt.ID(), "paused", "1", "user:ada", "api")
	}
	ck := f.signin(f.admin)
	_, now := f.do("GET", "/", ck, nil, nil)
	needs, wrong := strings.Index(now, `id="needs"`), strings.Index(now, `id="status-h"`)
	if needs < 0 || wrong < 0 || wrong < needs {
		t.Fatalf("Needs you at %d, Wrong right now at %d: Needs you comes first", needs, wrong)
	}
	more := strings.Index(now, `<details class="wrongbox__more">`)
	if more < 0 || strings.Count(now[wrong:more], `<li><a href=`) != 3 || !strings.Contains(now, "Show all 5") {
		t.Errorf("Wrong right now does not show 3 and fold the rest of 5: %s", now[max(wrong, 0):min(len(now), max(wrong, 0)+1500)])
	}
}
