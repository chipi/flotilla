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
	"github.com/chipi/flotilla/internal/store"
)

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

// The Decision box binds an answer to the plan it showed: an approval of another plan is refused, the
// shown plan's is accepted, and the note is kept with the answer.
func TestApproveBindsToThePlanShown(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	k := store.Key{Kind: "target", Value: "failed_backup/host:mini"}
	id, _, err := f.st.OpenCase(ctx, store.NewCase{Kind: "failed_backup", Target: "host:mini", Keys: []store.Key{k}, Canonical: k}, "ops/backup-monitor", "old backup")
	if err != nil {
		t.Fatal(err)
	}
	_ = f.st.MoveCase(ctx, id, cases.HandedOver, cases.ByDetector, "", "ops/backup-monitor", "")
	_ = f.st.MoveCase(ctx, id, cases.Planned, cases.ByAct, "", "ops/rerun-backup", "")
	if _, err := f.st.ApplyPlan(ctx, store.PlanRecord{CaseID: id, RunID: "r1", Action: "backup.rerun_observability",
		Params: map[string]string{"host": "mini"}, PlanHash: "4f1c9a20b7d3e8c1", Outcome: "propose", Autonomy: "propose",
		Reasons: []string{"autonomy: routine propose, action propose, host ceiling propose"}}, cases.AwaitingApproval, "", "host:mini", true); err != nil {
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
