package ui

import (
	"context"
	"strings"
	"testing"

	"github.com/chipi/flotilla/internal/cases"
	"github.com/chipi/flotilla/internal/store"
)

// The Cases pages (slice 2, read side): every role reads them; a case's free text (a detection's detail,
// a plan's reasons and parameters) is escaped; a case that needs a person is on Now.
func TestCasesPages(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	evil := `<script>alert(1)</script>`
	k := store.Key{Kind: "target", Value: "failed_backup/host:mini"}
	id, _, err := f.st.OpenCase(ctx, store.NewCase{Kind: "failed_backup", Target: "host:mini", Keys: []store.Key{k}, Canonical: k}, "ops/backup-monitor", evil)
	if err != nil {
		t.Fatal(err)
	}
	_ = f.st.MoveCase(ctx, id, cases.HandedOver, cases.ByDetector, "", "ops/backup-monitor", "")
	_ = f.st.MoveCase(ctx, id, cases.Planned, cases.ByAct, "", "ops/rerun-backup", "run r: backup_age_above(20h): holds")
	if _, err := f.st.ApplyPlan(ctx, store.PlanRecord{CaseID: id, RunID: "r", Action: "backup.rerun_observability",
		Params: map[string]string{"host": evil}, PlanHash: "abc123", Outcome: "shadow", Autonomy: "shadow", Would: "propose",
		Reasons: []string{"registry: ok", evil}}, cases.Closed, cases.ShadowPlan, "host:mini", false); err != nil {
		t.Fatal(err)
	}
	k2 := store.Key{Kind: "target", Value: "failed_backup/host:dgx"}
	esc, _, _ := f.st.OpenCase(ctx, store.NewCase{Kind: "failed_backup", Target: "host:dgx", Keys: []store.Key{k2}, Canonical: k2}, "m", "")
	_ = f.st.MoveCase(ctx, esc, cases.HandedOver, cases.ByDetector, "", "m", "")
	_ = f.st.MoveCase(ctx, esc, cases.Escalated, cases.ByTimer, "", "flotillad", "handed over too long")

	op := f.addOperator()
	for _, who := range []struct{ name, tok string }{{"viewer", f.vic}, {"operator", op}, {"admin", f.admin}} {
		ck := f.signin(who.tok)
		for _, p := range []string{"/cases", "/cases?status=open", "/cases/" + id, "/cases/" + esc, "/", "/approvals", "/history?period=all"} {
			resp, body := f.do("GET", p, ck, nil, nil)
			if resp.StatusCode != 200 {
				t.Fatalf("%s %s: %d", who.name, p, resp.StatusCode)
			}
			if strings.Contains(body, evil) {
				t.Fatalf("%s %s: untrusted text reached the page unescaped", who.name, p)
			}
		}
	}
	ck := f.signin(f.vic)
	_, open := f.do("GET", "/cases?status=open", ck, nil, nil)
	if strings.Contains(open, `href="/cases/`+id) || !strings.Contains(open, `href="/cases/`+esc) {
		t.Fatal("/cases?status=open lists the closed case, or not the open one")
	}
	_, all := f.do("GET", "/cases", ck, nil, nil)
	if !strings.Contains(all, `href="/cases/`+id) || !strings.Contains(all, `href="/cases/`+esc) {
		t.Fatal("/cases (All) leaves out a case")
	}
	_, closed := f.do("GET", "/cases?status=closed", ck, nil, nil)
	if !strings.Contains(closed, `href="/cases/`+id) || strings.Contains(closed, `href="/cases/`+esc) {
		t.Fatal("/cases?status=closed is not only the closed case")
	}
	_, pivot := f.do("GET", "/cases?p=target%3Ahost%3Adgx", ck, nil, nil)
	if strings.Contains(pivot, `href="/cases/`+id) || !strings.Contains(pivot, `href="/cases/`+esc) || !strings.Contains(pivot, "Clear all") {
		t.Fatal("a target pivot does not keep only that target's cases")
	}
	_, page := f.do("GET", "/cases/"+id, ck, nil, nil)
	for _, want := range []string{"backup.rerun_observability", "would have been", "propose", "shadow_plan_recorded",
		"backup_age_above(20h): holds", "registry: ok", "Planned", "(act)", "abc123"} {
		if !strings.Contains(page, want) {
			t.Errorf("the case page has no %q", want)
		}
	}
	_, now := f.do("GET", "/", ck, nil, nil)
	if !strings.Contains(now, "Needs you") || !strings.Contains(now, `class="casecard" href="/cases/`+esc+`"`) || strings.Contains(now, `class="casecard" href="/cases/`+id+`"`) {
		t.Fatal("Now's Needs you does not list the escalated case, or lists the closed one")
	}
	if !strings.Contains(now, `<a href="/cases/`+id+`">`) {
		t.Fatal("Now's Handled does not list the case closed today")
	}
	if !strings.Contains(now, "1 needs you.") {
		t.Fatal("Now's headline does not count the escalated case")
	}
	if resp, _ := f.do("GET", "/cases/case-nope", ck, nil, nil); resp.StatusCode != 404 {
		t.Fatalf("an unknown case: %d", resp.StatusCode)
	}
}
