package ui

import (
	"context"
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/chipi/flotilla/internal/store"
)

func cells(re *regexp.Regexp, page string) []string {
	var out []string
	for _, m := range re.FindAllStringSubmatch(page, -1) {
		out = append(out, m[1])
	}
	return out
}

func enrol(t *testing.T, f *fixture, host string) {
	t.Helper()
	ctx := context.Background()
	if _, err := f.st.AddEnrolment(ctx, host, "t-"+host, "user:ada", "test"); err != nil {
		t.Fatal(err)
	}
	if err := f.st.Enrol(ctx, host, "t-"+host, func() (string, time.Time, error) { return "c-" + host, time.Now().Add(720 * time.Hour), nil }); err != nil {
		t.Fatal(err)
	}
}

var execIDRe = regexp.MustCompile(`data-label="Execution" class="exact">(exe-\d+)<`)
var execActionRe = regexp.MustCompile(`data-label="Action" class="exact">([a-z.]+)<`)

// A host's Executions sort by when, action and result, search on action and case, and page; the keys carry "e" so they cannot
// meet the containers' "c" (the operator, 2026-11-23: a sortable-looking header sorts).
func TestHostExecutionsSortFilterAndPage(t *testing.T) {
	f := setup(t)
	enrol(t, f, "backup-01")
	ctx := context.Background()
	for i := 1; i <= 55; i++ {
		id, action := fmt.Sprintf("exe-%02d", i), "disk.cleanup"
		if i%2 == 0 {
			action = "backup.rerun"
		}
		if err := f.st.StartHostExecution(ctx, store.HostExecution{ID: id, CaseID: fmt.Sprintf("case-%02d", i), Host: "backup-01", Action: action, Approved: "user:ada", Envelope: []byte("{}")}); err != nil {
			t.Fatal(err)
		}
		if i%5 == 0 {
			if err := f.st.EndHostExecution(ctx, id, "failed", 1, "ab", "exit 1", "9b"); err != nil {
				t.Fatal(err)
			}
		}
	}
	ck := f.signin(f.vic)
	get := func(q string) string {
		_, page := f.do("GET", "/hosts/backup-01?tab=exec"+q, ck, nil, nil)
		return html.UnescapeString(page)
	}

	page := get("")
	ids := cells(execIDRe, page)
	if len(ids) != perPage || ids[0] != "exe-55" {
		t.Fatalf("the first page holds %d executions from %v, want %d, newest first", len(ids), ids[:1], perPage)
	}
	for _, want := range []string{`href="/hosts/backup-01?esort=when&tab=exec#executions"`, `href="/hosts/backup-01?esort=action&tab=exec#executions"`,
		`href="/hosts/backup-01?esort=result&tab=exec#executions"`, `name="eq"`, `name="tab" value="exec"`, `rel="next"`} {
		if !strings.Contains(page, want) {
			t.Errorf("Executions lacks %q", want)
		}
	}
	if ids := cells(execIDRe, get("&epage=3")); len(ids) != 15 {
		t.Errorf("page 3 holds %d executions, want 15", len(ids))
	}

	if ids := cells(execIDRe, get("&esort=when")); ids[0] != "exe-01" {
		t.Errorf("sorted by when ascending starts at %s, want exe-01", ids[0])
	}
	acts := cells(execActionRe, get("&esort=action&edir=desc"))
	for i := 1; i < len(acts); i++ {
		if acts[i-1] < acts[i] {
			t.Fatalf("sorted by action, descending: %v", acts)
		}
	}
	if page := get("&esort=result&edir=desc"); !strings.Contains(strings.SplitN(page, "<tbody>", 2)[1][:600], "sent") {
		t.Error("sorted by result, descending, a sent execution does not lead")
	}

	ids = cells(execIDRe, get("&eq=disk"))
	if len(ids) != perPage || !strings.Contains(get("&eq=disk"), "of 28") {
		t.Errorf("a search for disk shows %d executions, want a page of 28", len(ids))
	}
	if ids = cells(execIDRe, get("&eq=case-07")); len(ids) != 1 || ids[0] != "exe-07" {
		t.Errorf("a search on the case finds %v", ids)
	}
	if page := get("&eq=nothing-like-this"); !strings.Contains(page, "No execution matches these filters.") {
		t.Error("an empty search does not say so")
	}
}

var containerNameRe = regexp.MustCompile(`data-label="Container" class="exact">(c\d+)<`)

// A host's Containers sort, search and page on keys of their own; cAdvisor's answer is stubbed.
func TestHostContainersSortFilterAndPage(t *testing.T) {
	vm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		q := r.PostFormValue("query")
		series := func(val func(i int) string) string {
			var rs []string
			for i := 1; i <= 25; i++ {
				rs = append(rs, fmt.Sprintf(`{"metric":{"name":"c%02d"},"value":[1700000000,"%s"]}`, i, val(i)))
			}
			return `{"status":"success","data":{"result":[` + strings.Join(rs, ",") + `]}}`
		}
		switch {
		case strings.Contains(q, "container_memory_usage_bytes"):
			fmt.Fprint(w, series(func(i int) string { return fmt.Sprint(i * 1000000) }))
		case strings.Contains(q, "container_cpu_usage_seconds_total"):
			fmt.Fprint(w, series(func(i int) string { return fmt.Sprint(26 - i) }))
		case strings.Contains(q, "container_start_time_seconds"):
			fmt.Fprint(w, series(func(i int) string { return fmt.Sprint(1600000000 + i*1000) }))
		case strings.HasSuffix(r.URL.Path, "query_range"):
			fmt.Fprint(w, `{"status":"success","data":{"result":[{"metric":{},"values":[[1700000000,"20"],[1700000060,"61"]]}]}}`)
		default:
			fmt.Fprint(w, `{"status":"success","data":{"result":[{"metric":{},"value":[1700000000,"41.6"]}]}}`)
		}
	}))
	defer vm.Close()
	f := setup(t)
	enrol(t, f, "backup-01")
	f.ui.MetricsURL, f.ui.HostMetrics = vm.URL, map[string]string{"backup-01": "example"}
	ck := f.signin(f.vic)
	get := func(q string) string {
		_, page := f.do("GET", "/hosts/backup-01"+q, ck, nil, nil)
		return html.UnescapeString(page)
	}

	page := get("")
	names := cells(containerNameRe, page)
	if len(names) != 20 || names[0] != "c25" {
		t.Fatalf("the first page holds %d containers from %v, want 20 from c25 (the most memory)", len(names), names[:1])
	}
	for _, want := range []string{"Containers (25)", `href="/hosts/backup-01?csort=name#containers"`, `href="/hosts/backup-01?csort=cpu#containers"`,
		`href="/hosts/backup-01?csort=up#containers"`, `name="cq"`, `href="/hosts/backup-01?cpage=2#containers"`} {
		if !strings.Contains(page, want) {
			t.Errorf("Containers lacks %q", want)
		}
	}
	if names := cells(containerNameRe, get("?cpage=2")); len(names) != 5 || names[0] != "c05" {
		t.Errorf("page 2 holds %v, want c05 to c01", names)
	}
	if names := cells(containerNameRe, get("?csort=name")); names[0] != "c01" {
		t.Errorf("sorted by name ascending starts at %s", names[0])
	}
	if names := cells(containerNameRe, get("?csort=cpu&cdir=desc")); names[0] != "c01" {
		t.Errorf("sorted by cpu, descending, starts at %s, want c01 (the most cpu)", names[0])
	}
	if names := cells(containerNameRe, get("?csort=up&cdir=desc")); names[0] != "c01" {
		t.Errorf("sorted by uptime, descending, starts at %s, want c01 (the earliest start)", names[0])
	}
	if names := cells(containerNameRe, get("?cq=c1")); len(names) != 10 {
		t.Errorf("a search for c1 finds %d containers, want 10", len(names))
	}
	if page := get("?cq=zzz"); !strings.Contains(page, "No container matches these filters.") {
		t.Error("an empty search does not say so")
	}
}

var noteRowRe = regexp.MustCompile(`data-label="Note" class="exact">N-(\d+)<`)
var proposedRe = regexp.MustCompile(`<span class="meta">N-(\d+) · scope`)

// Knowledge: the proposed notes show three and fold the rest behind "Show all N", the active notes sort, search and page.
func TestKnowledgeNotesTable(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	for i := 1; i <= 5; i++ {
		if _, err := f.st.ProposeNote(ctx, "triage", fmt.Sprintf("waits %d", i), "", "user:ada"); err != nil {
			t.Fatal(err)
		}
	}
	for i := 1; i <= 12; i++ {
		scope := "triage"
		if i%3 == 0 {
			scope = "bugfix"
		}
		id, err := f.st.ProposeNote(ctx, scope, fmt.Sprintf("active %02d", i), "", "user:ada")
		if err != nil {
			t.Fatal(err)
		}
		by := "user:ada"
		if i%2 == 0 {
			by = "user:vic"
		}
		if err := f.st.DecideNote(ctx, id, store.NoteActive, by); err != nil {
			t.Fatal(err)
		}
	}
	ck := f.signin(f.addOperator())
	get := func(q string) string {
		_, page := f.do("GET", "/knowledge"+q, ck, nil, nil)
		return html.UnescapeString(page)
	}

	page := get("")
	if n := len(cells(proposedRe, page)); n != 5 {
		t.Errorf("%d proposed notes are on the page, want all 5 (3 shown, 2 folded)", n)
	}
	i := strings.Index(page, `<details class="wrongbox__more">`)
	if i < 0 || !strings.Contains(page[i:], "Show all 5") {
		t.Fatal(`the proposed notes have no "Show all 5" fold`)
	}
	if n := len(cells(proposedRe, page[:i])); n != 3 {
		t.Errorf("%d proposed notes are shown before the fold, want 3", n)
	}
	if n := len(cells(proposedRe, page[i:])); n != 2 {
		t.Errorf("%d proposed notes are folded, want 2", n)
	}

	ids := cells(noteRowRe, page)
	if len(ids) != 10 || ids[0] != "17" {
		t.Fatalf("the first page of active notes is %v, want 10 from N-17 (the latest activated)", ids)
	}
	for _, want := range []string{`href="/knowledge?sort=scope#active"`, `href="/knowledge?sort=by#active"`, `href="/knowledge?sort=when#active"`,
		`name="q"`, `href="/knowledge?page=2#active"`} {
		if !strings.Contains(page, want) {
			t.Errorf("Active notes lacks %q", want)
		}
	}
	if ids := cells(noteRowRe, get("?page=2")); len(ids) != 2 {
		t.Errorf("page 2 holds %v, want 2 notes", ids)
	}
	if ids := cells(noteRowRe, get("?sort=when")); ids[0] != "6" {
		t.Errorf("sorted by activation ascending starts at N-%s, want N-6", ids[0])
	}
	if ids := cells(noteRowRe, get("?sort=scope")); ids[0] != "8" {
		t.Errorf("sorted by scope ascending starts at N-%s, want N-8 (bugfix, the lowest)", ids[0])
	}
	if ids := cells(noteRowRe, get("?sort=by&dir=desc")); ids[0] != "17" {
		t.Errorf("sorted by who, descending, starts at N-%s, want N-17 (vic, the latest)", ids[0])
	}
	if ids := cells(noteRowRe, get("?q=bugfix")); len(ids) != 4 {
		t.Errorf("a search on the scope finds %d notes, want 4", len(ids))
	}
	if ids := cells(noteRowRe, get("?q=active+07")); len(ids) != 1 {
		t.Errorf("a search on the text finds %d notes, want 1", len(ids))
	}
	if page := get("?q=nothing-like-this"); !strings.Contains(page, "No note matches these filters.") {
		t.Error("an empty search does not say so")
	}

	// three or fewer need no fold
	f2 := setup(t)
	if _, err := f2.st.ProposeNote(ctx, "triage", "one", "", "user:ada"); err != nil {
		t.Fatal(err)
	}
	_, page = f2.do("GET", "/knowledge", f2.signin(f2.addOperator()), nil, nil)
	if strings.Contains(page, "wrongbox__more") {
		t.Error("one proposed note has a fold")
	}
}

var versionRowRe = regexp.MustCompile(`data-label="Version" class="exact">v(\d+)<`)

// The routine page's Versions sort by version and when, newest first, 10 a page, on keys of their own.
func TestRoutineVersionsTable(t *testing.T) {
	f := versionedFixtureN(t, "", 12)
	ck := f.signin(f.addOperator())
	get := func(q string) string {
		_, page := f.do("GET", "/fleets/triage/cycle"+q, ck, nil, nil)
		return html.UnescapeString(versionsSection(t, page))
	}

	sec := get("")
	vs := cells(versionRowRe, sec)
	if len(vs) != 10 || vs[0] != "12" || vs[9] != "3" {
		t.Fatalf("the first page of versions is %v, want v12 down to v3", vs)
	}
	for _, want := range []string{`href="/fleets/triage/cycle?vsort=version#versions"`, `href="/fleets/triage/cycle?vsort=when#versions"`,
		`href="/fleets/triage/cycle?vpage=2#versions"`, `rel="next"`} {
		if !strings.Contains(sec, want) {
			t.Errorf("Versions lacks %q", want)
		}
	}
	if vs := cells(versionRowRe, get("?vpage=2")); len(vs) != 2 || vs[0] != "2" {
		t.Errorf("page 2 holds %v, want v2 and v1", vs)
	}
	if vs := cells(versionRowRe, get("?vsort=version")); vs[0] != "1" {
		t.Errorf("sorted by version ascending starts at v%s", vs[0])
	}
	if vs := cells(versionRowRe, get("?vsort=when")); vs[0] != "1" {
		t.Errorf("sorted by when ascending starts at v%s", vs[0])
	}
	if vs := cells(versionRowRe, get("?vsort=when&vdir=desc")); vs[0] != "12" {
		t.Errorf("sorted by when descending starts at v%s", vs[0])
	}
	if !strings.Contains(sec, "Promote…") {
		t.Error("a shadow on the page has no Promote form")
	}
}

var briefLinkRe = regexp.MustCompile(`href="/brief\?day=(\d{4}-\d{2}-\d{2})"><strong>`)

// The archive of earlier briefs pages at 30, newest first.
func TestBriefArchivePages(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	day := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 35; i++ {
		d := day.AddDate(0, 0, i).Format("2006-01-02")
		if err := f.st.SaveBrief(ctx, store.Brief{Day: d, WrittenAt: d + "T06:30:00Z", WrittenBy: briefWriter, Headline: "Nothing needs you."}); err != nil {
			t.Fatal(err)
		}
	}
	ck := f.signin(f.vic)
	_, page := f.do("GET", "/brief/archive", ck, nil, nil)
	page = html.UnescapeString(page)
	days := cells(briefLinkRe, page)
	if len(days) != 30 || days[0] != "2026-11-04" || days[29] != "2026-10-06" {
		t.Fatalf("the first page of briefs is %d from %v, want 30 from 2026-11-04 to 2026-10-06", len(days), days[:1])
	}
	for _, want := range []string{"1–30 of 35", `href="/brief/archive?page=2"`, `rel="next"`} {
		if !strings.Contains(page, want) {
			t.Errorf("the archive lacks %q", want)
		}
	}
	_, page = f.do("GET", "/brief/archive?page=2", ck, nil, nil)
	if days := cells(briefLinkRe, page); len(days) != 5 || days[4] != "2026-10-01" {
		t.Errorf("page 2 of the archive is %v, want 5 down to 2026-10-01", days)
	}
}
