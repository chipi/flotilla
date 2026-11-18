package ui

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/chipi/flotilla/internal/cases"
	"github.com/chipi/flotilla/internal/spec"
	"github.com/chipi/flotilla/internal/store"
)

// A host's load comes from VictoriaMetrics by its metrics instance (G4): the Hosts column, the Host
// page's charts; a host without an instance in host_metrics says so.
func TestHostLoad(t *testing.T) {
	var queries []string
	vmsrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		q := r.PostFormValue("query")
		queries = append(queries, q)
		if !strings.Contains(q, `instance="example"`) || strings.Contains(q, "container_") {
			fmt.Fprint(w, `{"status":"success","data":{"result":[]}}`)
			return
		}
		if strings.HasSuffix(r.URL.Path, "query_range") {
			fmt.Fprint(w, `{"status":"success","data":{"result":[{"metric":{},"values":[[1700000000,"20"],[1700000060,"61"],[1700000120,"22"]]}]}}`)
			return
		}
		fmt.Fprint(w, `{"status":"success","data":{"result":[{"metric":{},"value":[1700000000,"41.6"]}]}}`)
	}))
	defer vmsrv.Close()
	f := setup(t)
	ctx := context.Background()
	for _, h := range []string{"backup-01", "model-host"} {
		if _, err := f.st.AddEnrolment(ctx, h, "t-"+h, "user:ada", "test"); err != nil {
			t.Fatal(err)
		}
		if err := f.st.Enrol(ctx, h, "t-"+h, func() (string, time.Time, error) { return "c-" + h, time.Now().Add(720 * time.Hour), nil }); err != nil {
			t.Fatal(err)
		}
	}
	f.ui.MetricsURL, f.ui.HostMetrics = vmsrv.URL, map[string]string{"backup-01": "example"}
	// G10: backup-01's agent reported its config; model-host's (older) did not
	if err := f.st.RecordHostContact(ctx, store.HostContact{Host: "backup-01", Connected: true, Version: "v", Ceiling: "propose", LastContact: time.Now(),
		Allowlist: `[{"name":"backup.rerun","argv":["/usr/bin/sudo","-n","/bin/launchctl","kickstart","system/com.example.db-backup"],"per_hour":1,"timeout":"30m0s"}]`}); err != nil {
		t.Fatal(err)
	}
	ck := f.signin(f.vic)
	if _, agent := f.do("GET", "/hosts/backup-01?tab=agent", ck, nil, nil); !strings.Contains(agent, "/bin/launchctl kickstart system/com.example.db-backup") || !strings.Contains(agent, "1 an hour") {
		t.Error("the Host page does not show the reported allowlist")
	}
	if _, agent := f.do("GET", "/hosts/model-host?tab=agent", ck, nil, nil); !strings.Contains(agent, "No allowlist reported.") {
		t.Error("an agent that reported no allowlist is not named")
	}
	if _, page := f.do("GET", "/hosts", ck, nil, nil); !strings.Contains(page, "42% · 42% · 42%") || !strings.Contains(page, "– · – · –") {
		t.Error("the Hosts column: backup-01's load, model-host's dashes")
	}
	// backup-01 was heard from just now
	if _, page := f.do("GET", "/hosts/backup-01", ck, nil, nil); !strings.Contains(page, "last heard just now ·") {
		t.Error(`a contact under a minute old reads "just now", not "just now ago"`)
	}
	_, page := f.do("GET", "/hosts/backup-01", ck, nil, nil)
	for _, want := range []string{"CPU", "Memory", "Disk IO", "peak 61% at", "runs no cAdvisor"} {
		if !strings.Contains(page, want) {
			t.Errorf("the Host page has no %q", want)
		}
	}
	if _, mh := f.do("GET", "/hosts/model-host", ck, nil, nil); !strings.Contains(mh, "host_metrics names no metrics instance for model-host") {
		t.Error("a host without a metrics instance does not say so")
	}
	found := false
	for _, q := range queries {
		found = found || strings.Contains(q, "node_memory_wired_bytes") && strings.Contains(q, "node_memory_MemAvailable_bytes")
	}
	if !found {
		t.Error("the memory query does not cover both macOS and Linux exporters")
	}
	// the agent's own lines, as flotillad logged them ("host log"), on the Logs tab
	var lq string
	vl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		lq = r.PostFormValue("query")
		fmt.Fprintln(w, `{"_time":"2026-11-15T07:00:00Z","level":"WARN","msg":"host log","host":"backup-01","line":"the backup job is slow","service":"flotillad"}`)
	}))
	defer vl.Close()
	f.ui.LogsURL = vl.URL
	_, logs := f.do("GET", "/hosts/backup-01?tab=logs&level=warn", ck, nil, nil)
	if !strings.Contains(lq, `"host log" host:"backup-01"`) || !strings.Contains(lq, "warn") || !strings.Contains(logs, "the backup job is slow") {
		t.Errorf("the Logs tab: query %q", lq)
	}
}

// The run and fleet pages read their lines from VictoriaLogs (G5): the run's lines oldest first, the
// fleet's Logs tab with its level filter in the query; without an address the page says so.
func TestLogLines(t *testing.T) {
	var queries []string
	vl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		queries = append(queries, r.PostFormValue("query"))
		fmt.Fprintln(w, `{"_time":"2026-11-14T17:09:09Z","level":"info","routine":"triage/cycle","run_id":"run-l","msg":"run output","line":"second","service":"flotilla-worker-triage"}`)
		fmt.Fprintln(w, `{"_time":"2026-11-14T17:09:07Z","level":"info","routine":"triage/cycle","run_id":"run-l","msg":"run start","service":"flotillad"}`)
		fmt.Fprintln(w, `{"_time":"2026-11-14T17:09:08Z","level":"error","routine":"triage/cycle","run_id":"run-l","msg":"run output","line":"first <b>"}`)
	}))
	defer vl.Close()
	f := setup(t)
	ctx := context.Background()
	if err := f.st.StartRun(ctx, store.Run{ID: "run-l", Routine: "triage/cycle", SpecHash: "h", Trigger: "schedule", Actor: "flotillad",
		Autonomy: "shadow", StartedAt: time.Now().UTC().Format(store.TimeFormat), Status: "running"}); err != nil {
		t.Fatal(err)
	}
	ck := f.signin(f.vic)
	if _, page := f.do("GET", "/runs/run-l", ck, nil, nil); !strings.Contains(page, "vlogs_url") {
		t.Error("without an address the run page does not say why it has no lines")
	}
	f.ui.LogsURL = vl.URL
	_, page := f.do("GET", "/runs/run-l", ck, nil, nil)
	if i, j := strings.Index(page, "first &lt;b&gt;"), strings.Index(page, "second"); i < 0 || j < 0 || i > j {
		t.Errorf("the run's lines, escaped and oldest first: %d %d", i, j)
	}
	if !strings.Contains(page, "worker-triage") || !strings.Contains(page, "flotillad ") {
		t.Error("each line names the service that wrote it (flotillad and the worker both log a run's start)")
	}
	f.do("GET", "/fleets/triage?tab=logs&level=error", ck, nil, nil)
	last := queries[len(queries)-1]
	if !strings.Contains(last, `routine:~"^triage/"`) || !strings.Contains(last, "error") || !strings.Contains(last, "limit 300") {
		t.Errorf("the fleet's query: %s", last)
	}
}

// The run page reads its trace from VictoriaTraces (G2): spans nested under their parent, the selected
// span's attributes; a trace VictoriaTraces does not have, or no trace id, says why instead.
func TestRunTrace(t *testing.T) {
	vt := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/select/jaeger/api/traces/aaaa1111" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `{"data":[{"spans":[
			{"spanID":"s1","operationName":"run platform/heartbeat","references":[],"startTime":1000000,"duration":3000000,"tags":[{"key":"flotilla.status","value":"ok"}],"processID":"p1"},
			{"spanID":"s2","operationName":"model.chat","references":[{"refType":"CHILD_OF","spanID":"s1"}],"startTime":1500000,"duration":1200000,"tags":[{"key":"gen_ai.request.model","value":"triage-pro"}],"processID":"p2"}],
			"processes":{"p1":{"serviceName":"flotillad"},"p2":{"serviceName":"flotilla-platform"}}}]}`)
	}))
	defer vt.Close()
	f := setup(t)
	f.sch.OTLPTracesURL = vt.URL + "/insert/opentelemetry/v1/traces"
	ctx := context.Background()
	for _, r := range []store.Run{{ID: "run-t", TraceID: "aaaa1111"}, {ID: "run-gone", TraceID: "bbbb2222"}, {ID: "run-none"}} {
		r.Routine, r.SpecHash, r.Trigger, r.Actor, r.Autonomy, r.Status = "platform/heartbeat", "h", "schedule", "flotillad", "auto", "running"
		r.StartedAt = time.Now().UTC().Format(store.TimeFormat)
		if err := f.st.StartRun(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	ck := f.signin(f.vic)
	_, page := f.do("GET", "/runs/run-t?span=s2", ck, nil, nil)
	for _, want := range []string{"run platform/heartbeat", `class="trace__name depth-1"`, "gen_ai.request.model", "2 spans in 3.0 s"} {
		if !strings.Contains(page, want) {
			t.Errorf("the trace page has no %q", want)
		}
	}
	if _, gone := f.do("GET", "/runs/run-gone", ck, nil, nil); !strings.Contains(gone, "No spans.") {
		t.Error("a trace VictoriaTraces does not have is not named")
	}
	if _, none := f.do("GET", "/runs/run-none", ck, nil, nil); !strings.Contains(none, "no trace id") {
		t.Error("a run without a trace id is not named")
	}
}

// A verdict and a note on a case (G8): an operator's verdict is kept (and shown on the action's record),
// a viewer's is refused; a note waits on Knowledge until activated, which a viewer may not do.
func TestFeedbackAndNotes(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	k := store.Key{Kind: "target", Value: "failed_backup/host:backup-01"}
	id, _, err := f.st.OpenCase(ctx, store.NewCase{Kind: "failed_backup", Target: "host:backup-01", Keys: []store.Key{k}, Canonical: k}, "ops/backup-monitor", "old")
	if err != nil {
		t.Fatal(err)
	}
	_ = f.st.MoveCase(ctx, id, cases.HandedOver, cases.ByDetector, "", "ops/backup-monitor", "")
	_ = f.st.MoveCase(ctx, id, cases.Planned, cases.ByAct, "", "ops/rerun-backup", "")
	if _, err := f.st.ApplyPlan(ctx, store.PlanRecord{CaseID: id, RunID: "r1", Action: "backup.rerun", Params: map[string]string{"host": "backup-01"},
		PlanHash: "abcdef0123", Outcome: "shadow", Autonomy: "shadow", Reasons: []string{"r"}}, cases.Closed, cases.ShadowPlan, "host:backup-01", false); err != nil {
		t.Fatal(err)
	}
	op := f.addOperator()
	ck, csrf := f.csrfOf(op)
	vck, vcsrf := f.csrfOf(f.vic)
	if resp, _ := f.do("POST", "/cases/"+id+"/right", vck, url.Values{"csrf": {vcsrf}}, nil); !strings.Contains(resp.Header.Get("Location"), "err=") {
		t.Fatal("a viewer's verdict was taken")
	}
	if resp, _ := f.do("POST", "/cases/"+id+"/right", ck, url.Values{"csrf": {csrf}}, nil); !strings.Contains(resp.Header.Get("Location"), "msg=") {
		t.Fatalf("the operator's verdict: %s", resp.Header.Get("Location"))
	}
	if fb, _ := f.st.CaseFeedback(ctx, id); len(fb) != 1 || fb[0].User != "otto" || fb[0].Verdict != "right" {
		t.Fatalf("the verdict: %+v", fb)
	}
	if _, page := f.do("GET", "/autonomy", ck, nil, nil); !strings.Contains(page, "diagnoses right 1, wrong 0") {
		t.Error("the verdict is not on the action's record")
	}
	if resp, _ := f.do("POST", "/cases/"+id+"/note", ck, url.Values{"csrf": {csrf}, "note": {"the LiteLLM dump is the slow step"}}, nil); !strings.Contains(resp.Header.Get("Location"), "msg=") {
		t.Fatalf("proposing a note: %s", resp.Header.Get("Location"))
	}
	notes, _ := f.st.Notes(ctx, store.NoteProposed)
	if len(notes) != 1 || notes[0].Scope != "kind:failed_backup" || notes[0].FromCase != id {
		t.Fatalf("the note: %+v", notes)
	}
	nid := fmt.Sprint(notes[0].ID)
	if _, page := f.do("GET", "/knowledge", ck, nil, nil); !strings.Contains(page, "the LiteLLM dump is the slow step") || !strings.Contains(page, `action="/notes/`+nid+`/activate"`) {
		t.Fatal("the note does not wait on Knowledge")
	}
	if resp, _ := f.do("POST", "/notes/"+nid+"/activate", vck, url.Values{"csrf": {vcsrf}}, nil); !strings.Contains(resp.Header.Get("Location"), "err=") {
		t.Fatal("a viewer activated a note")
	}
	if resp, _ := f.do("POST", "/notes/"+nid+"/activate", ck, url.Values{"csrf": {csrf}}, nil); !strings.Contains(resp.Header.Get("Location"), "msg=") {
		t.Fatalf("activating: %s", resp.Header.Get("Location"))
	}
	if resp, _ := f.do("POST", "/notes/"+nid+"/activate", ck, url.Values{"csrf": {csrf}}, nil); !strings.Contains(resp.Header.Get("Location"), "err=") {
		t.Fatal("an active note was activated twice")
	}
	if active, _ := f.st.Notes(ctx, store.NoteActive); len(active) != 1 || active[0].DecidedBy != "otto" {
		t.Fatalf("the active note: %+v", active)
	}
}

// The Decision box binds an answer to the plan it showed: an approval of another plan is refused, the
// shown plan's is accepted, and the note is kept with the answer.
func TestApproveBindsToThePlanShown(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	k := store.Key{Kind: "target", Value: "failed_backup/host:backup-01"}
	id, _, err := f.st.OpenCase(ctx, store.NewCase{Kind: "failed_backup", Target: "host:backup-01", Keys: []store.Key{k}, Canonical: k}, "ops/backup-monitor", "old backup")
	if err != nil {
		t.Fatal(err)
	}
	_ = f.st.MoveCase(ctx, id, cases.HandedOver, cases.ByDetector, "", "ops/backup-monitor", "")
	_ = f.st.MoveCase(ctx, id, cases.Planned, cases.ByAct, "", "ops/rerun-backup", "")
	if _, err := f.st.ApplyPlan(ctx, store.PlanRecord{CaseID: id, RunID: "r1", Action: "backup.rerun",
		Params: map[string]string{"host": "backup-01"}, PlanHash: "4f1c9a20b7d3e8c1", Outcome: "propose", Autonomy: "propose",
		Reasons: []string{"autonomy: routine propose, action propose, host ceiling propose"}}, cases.AwaitingApproval, "", "host:backup-01", true); err != nil {
		t.Fatal(err)
	}
	op := f.addOperator()
	ck, csrf := f.csrfOf(op)
	_, page := f.do("GET", "/cases/"+id, ck, nil, nil)
	if !strings.Contains(page, `name="plan" value="4f1c9a20b7d3e8c1"`) || !strings.Contains(page, "Approve plan 4f1c…e8c1") {
		t.Fatal("the Decision box does not carry the plan it shows")
	}
	vck := f.signin(f.vic)
	if _, vpage := f.do("GET", "/cases/"+id, vck, nil, nil); strings.Contains(vpage, `action="/cases/`+id+`/approve"`) {
		t.Fatal("a viewer sees the approve form")
	}
	resp, _ := f.do("POST", "/cases/"+id+"/approve", ck, url.Values{"csrf": {csrf}, "plan": {"deadbeef00"}}, nil)
	if !strings.Contains(resp.Header.Get("Location"), "err=") {
		t.Fatalf("approving another plan: %s", resp.Header.Get("Location"))
	}
	if c, _ := f.st.Case(ctx, id); c.Status != cases.AwaitingApproval {
		t.Fatalf("a refused approval moved the case to %s", c.Status)
	}
	resp, _ = f.do("POST", "/cases/"+id+"/approve", ck, url.Values{"csrf": {csrf}, "plan": {"4f1c9a20b7d3e8c1"}, "note": {"running late, fine"}}, nil)
	if !strings.Contains(resp.Header.Get("Location"), "msg=") {
		t.Fatalf("approving the shown plan: %s", resp.Header.Get("Location"))
	}
	c, _ := f.st.Case(ctx, id)
	evs, _ := f.st.CaseEvents(ctx, id)
	if last := evs[len(evs)-1]; c.Status != cases.Executing || last.Actor != "user:otto" || !strings.Contains(last.Detail, "running late, fine") {
		t.Fatalf("the approval: %s %+v", c.Status, last)
	}
}

// Now's "since you looked" is the end of the previous visit: reloading within an hour keeps it.
func TestSinceYouLooked(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	if s, _ := f.st.SeenNow(ctx, "vic"); s != "" {
		t.Fatalf("a first look: %q", s)
	}
	if s, _ := f.st.SeenNow(ctx, "vic"); s != "" {
		t.Fatalf("a reload in the first visit: %q", s)
	}
	if s, _ := f.st.LastLook(ctx, "vic"); s != "" {
		t.Fatalf("the last look before a second visit: %q", s)
	}
	ck := f.signin(f.vic)
	if _, body := f.do("GET", "/", ck, nil, nil); !strings.Contains(body, "the last 24 hours") {
		t.Fatal("Now without an earlier visit does not say it shows the last 24 hours")
	}
}

// Every table pages: 120 runs are 50 a page, with Next and the range.
func TestRunsPage(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	base := time.Now().UTC().Add(-3 * time.Hour)
	for i := 0; i < 120; i++ {
		id := fmt.Sprintf("triage-cycle-%03d", i)
		_ = f.st.StartRun(ctx, store.Run{ID: id, Routine: "triage/cycle", SpecHash: "h", Trigger: "schedule", Actor: "flotillad",
			Autonomy: "shadow", StartedAt: base.Add(time.Duration(i) * time.Minute).Format(store.TimeFormat), Status: "running"})
		st := store.StatusOK
		if i%10 == 0 {
			st = store.StatusError
		}
		_ = f.st.FinishRun(ctx, id, st, nil, 0, 1, "")
	}
	ck := f.signin(f.vic)
	_, p1 := f.do("GET", "/runs", ck, nil, nil)
	if !strings.Contains(p1, "1–50 of 120") || !strings.Contains(p1, "triage-cycle-119") || strings.Contains(p1, ">triage-cycle-069<") {
		t.Fatal("page 1 is not the newest 50 of 120")
	}
	_, p3 := f.do("GET", "/runs?page=3", ck, nil, nil)
	if !strings.Contains(p3, "101–120 of 120") || !strings.Contains(p3, ">triage-cycle-000<") {
		t.Fatal("page 3 is not the oldest 20")
	}
	_, bad := f.do("GET", "/runs?f=problems", ck, nil, nil)
	if !strings.Contains(bad, "1–12 of 12") {
		t.Fatal("Problems does not keep only the 12 failed runs")
	}
	_, asc := f.do("GET", "/runs?sort=started", ck, nil, nil)
	if strings.Index(asc, ">triage-cycle-000<") > strings.Index(asc, ">triage-cycle-001<") {
		t.Fatal("sorting by Started ascending does not put the oldest first")
	}
}

// G14: a case that came back links to the one it recurs.
func TestCaseRecurs(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	open := func(key string) string {
		k := store.Key{Kind: "target", Value: "failed_backup/host:backup-01" + key}
		id, _, err := f.st.OpenCase(ctx, store.NewCase{Kind: "failed_backup", Target: "host:backup-01", Keys: []store.Key{k}, Canonical: k, Title: "t"}, "m", "")
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	first := open("#1")
	_ = f.st.MoveCase(ctx, first, cases.HandedOver, cases.ByDetector, "", "m", "")
	if err := f.st.MoveCase(ctx, first, cases.Closed, cases.ByAct, cases.ResolvedItself, "a", ""); err != nil {
		t.Fatal(err)
	}
	again := open("#2")
	ck := f.signin(f.vic)
	if _, page := f.do("GET", "/cases/"+again, ck, nil, nil); !strings.Contains(page, `recurs <a class="exact" href="/cases/`+first+`">`) {
		t.Error("the case page does not link the case it recurs")
	}
}

// G15: the newest contract check is "Check now"; the selected alias lists its last checks; an operator
// can start the check.
func TestContractChecksOnModels(t *testing.T) {
	j := &spec.Routine{Fleet: "platform", Routine: "model-contract", Description: "d", Enabled: true, Worker: "triage", Autonomy: "auto",
		Run: spec.Run{Command: "true"}, Every: 24 * time.Hour, Limit: time.Minute, Fresh: 26 * time.Hour}
	f := setupWith(t, []*spec.Routine{j})
	ctx := context.Background()
	f.sch.ModelsJSON = `{"endpoints":{"gw":{"base_url":"http://gw/v1","kind":"litellm","defaults":{"tools":"native","json":"object","cost":"reported"}}},` +
		`"aliases":{"good":{"endpoint":"gw","model":"m-good"},"bad":{"endpoint":"gw","model":"m-bad"}}}`
	yes, no := true, false
	at := func(d time.Duration) string { return time.Now().Add(-d).UTC().Format(store.TimeFormat) }
	var calls []store.ModelCall
	for _, c := range []struct {
		run, alias, check string
		ok                *bool
		ago               time.Duration
	}{{"r1", "good", "json_object", &yes, 25 * time.Hour}, {"r1", "good", "tools", &yes, 25 * time.Hour},
		{"r2", "good", "json_object", &yes, time.Hour}, {"r2", "good", "tools", &yes, time.Hour},
		{"r2", "bad", "json_object", &yes, time.Hour}, {"r2", "bad", "tools", &no, time.Hour}} {
		calls = append(calls, store.ModelCall{RunID: c.run, Routine: "platform/model-contract", At: at(c.ago), Alias: c.alias,
			Check: c.check, CheckOK: c.ok, Detail: "d"})
	}
	if err := f.st.RecordCalls(ctx, calls); err != nil {
		t.Fatal(err)
	}
	op := f.addOperator()
	ck, _ := f.csrfOf(op)
	_, page := f.do("GET", "/models?tab=inv&alias=good", ck, nil, nil)
	for _, want := range []string{"pass at ", "fail: tools at ", "json_object, tools · pass", `action="/routines/platform/model-contract/run"`, "Check now"} {
		if !strings.Contains(page, want) {
			t.Errorf("Models has no %q", want)
		}
	}
	if strings.Count(page, "json_object, tools · pass") != 2 {
		t.Error("the selected alias does not list both of its check runs")
	}
	if !strings.Contains(page, `<span class="score__label">Models down</span>
  <div class="score__value"><strong>1</strong><span>of 2 with calls</span>`) {
		t.Error("Models down does not count the alias whose last check failed")
	}
}

// G7: what a run read, with its trust, on the run page; a run with nothing untrusted says so.
func TestWhatTheAgentRead(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	for _, id := range []string{"run-r", "run-clean"} {
		if err := f.st.StartRun(ctx, store.Run{ID: id, Routine: "triage/cycle", SpecHash: "h", Trigger: "schedule", Actor: "flotillad",
			Autonomy: "shadow", StartedAt: time.Now().UTC().Format(store.TimeFormat), Status: "running"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.st.RecordLoaded(ctx, []store.Loaded{
		{RunID: "run-r", Routine: "triage/cycle", Kind: "playbook", Trust: "source", Title: "triage playbook", Source: "fleets/triage/playbooks/triage.md @ ab12", SHA256: "abcdef0123456789"},
		{RunID: "run-r", Routine: "triage/cycle", Kind: "input", Trust: "untrusted", Title: "glitchtip: <PLAYER-1I>", Source: "summary"},
		{RunID: "run-clean", Routine: "triage/cycle", Kind: "skill", Trust: "source", Title: "hint-external", Source: "fleet skill hint-external @ v"}}); err != nil {
		t.Fatal(err)
	}
	if err := f.st.RecordLoaded(ctx, []store.Loaded{{RunID: "run-r", Kind: "input", Trust: "trusted", Title: "x"}}); err == nil {
		t.Error("an unknown trust was kept")
	}
	ck := f.signin(f.vic)
	_, page := f.do("GET", "/runs/run-r", ck, nil, nil)
	for _, want := range []string{`<span class="chip chip--ok">source</span><strong>triage playbook</strong>`, "sha256 abcdef012345",
		`<span class="chip chip--stop">untrusted</span><strong>glitchtip: &lt;PLAYER-1I&gt;</strong>`, "1 untrusted item entered this run"} {
		if !strings.Contains(page, want) {
			t.Errorf("the run page has no %q", want)
		}
	}
	if _, clean := f.do("GET", "/runs/run-clean", ck, nil, nil); !strings.Contains(clean, "Nothing untrusted entered this run.") {
		t.Error("a run with only sources does not say nothing untrusted entered")
	}
}

// The v1 gate (operator, 2026-11-15): a shadow plan's case says what its verdict judges.
func TestShadowVerdictMeaning(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	k := store.Key{Kind: "target", Value: "failed_backup/host:backup-01#s"}
	id, _, err := f.st.OpenCase(ctx, store.NewCase{Kind: "failed_backup", Target: "host:backup-01", Keys: []store.Key{k}, Canonical: k, Title: "t"}, "m", "")
	if err != nil {
		t.Fatal(err)
	}
	_ = f.st.MoveCase(ctx, id, cases.HandedOver, cases.ByDetector, "", "m", "")
	_ = f.st.MoveCase(ctx, id, cases.Planned, cases.ByAct, "", "a", "")
	if _, err := f.st.ApplyPlan(ctx, store.PlanRecord{CaseID: id, Action: "backup.rerun", Params: map[string]string{"host": "backup-01"},
		PlanHash: "h", Outcome: "shadow", Autonomy: "shadow", Would: "propose"}, cases.Closed, cases.ShadowPlan, "host:backup-01", false); err != nil {
		t.Fatal(err)
	}
	ck, _ := f.csrfOf(f.addOperator())
	if _, page := f.do("GET", "/cases/"+id, ck, nil, nil); !strings.Contains(page, "A shadow plan: judge the chain at its threshold.") {
		t.Error("a shadow plan's case does not say what its verdict judges")
	}
}
