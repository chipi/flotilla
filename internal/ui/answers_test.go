package ui

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/chipi/flotilla/internal/cases"
	"github.com/chipi/flotilla/internal/store"
)

// ADR-0023 in the UI: an escalated case offers Dismiss beside Close and Reassign, a watching case offers
// Acknowledge, and both answers go through the core.
func TestDecisionBoxOffersAnswers(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	open := func(key string) string {
		k := store.Key{Kind: "target", Value: "failed_backup/host:" + key}
		id, _, err := f.st.OpenCase(ctx, store.NewCase{Kind: "failed_backup", Target: "host:" + key, Keys: []store.Key{k}, Canonical: k}, "ops/backup-monitor", "")
		if err != nil {
			t.Fatal(err)
		}
		_ = f.st.MoveCase(ctx, id, cases.HandedOver, cases.ByDetector, "", "ops/backup-monitor", "")
		return id
	}
	esc, watch := open("e"), open("w")
	_ = f.st.MoveCase(ctx, esc, cases.Escalated, cases.ByTimer, "", "flotillad", "stuck")
	_ = f.st.MoveCase(ctx, watch, cases.Planned, cases.ByAct, "", "a", "")
	if _, err := f.st.ApplyPlan(ctx, store.PlanRecord{CaseID: watch, RunID: "r", Action: "backup.rerun", Params: map[string]string{"host": "w"},
		PlanHash: "h-w-0123456789", Outcome: "record", Autonomy: "record"}, cases.Watching, "", "host:w", false); err != nil {
		t.Fatal(err)
	}
	if err := f.st.DueNotice(ctx, watch, store.NoticeWatching, 1, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	ck, csrf := f.csrfOf(f.addOperator())
	_, page := f.do("GET", "/cases/"+esc, ck, nil, nil)
	for _, want := range []string{`action="/cases/` + esc + `/dismiss"`, `action="/cases/` + esc + `/close"`, `action="/cases/` + esc + `/reassign"`} {
		if !strings.Contains(page, want) {
			t.Errorf("an escalated case's page has no %s", want)
		}
	}
	_, page = f.do("GET", "/cases/"+watch, ck, nil, nil)
	if !strings.Contains(page, `action="/cases/`+watch+`/acknowledge"`) {
		t.Error("a watching case's page offers no Acknowledge")
	}
	vck := f.signin(f.vic)
	if _, vpage := f.do("GET", "/cases/"+watch, vck, nil, nil); strings.Contains(vpage, `/acknowledge"`) {
		t.Error("a viewer sees Acknowledge")
	}
	f.do("POST", "/cases/"+watch+"/acknowledge", ck, url.Values{"csrf": {csrf}}, nil)
	if ns, _ := f.st.Notices(ctx, watch); len(ns) != 1 || ns[0].AckedBy == "" {
		t.Fatalf("the notice after Acknowledge: %+v", ns)
	}
	resp, _ := f.do("POST", "/cases/"+esc+"/dismiss", ck, url.Values{"csrf": {csrf}, "reason": {"noise"}}, nil)
	if strings.Contains(resp.Header.Get("Location"), "err=") {
		t.Fatalf("dismiss: %s", resp.Header.Get("Location"))
	}
	if c, _ := f.st.Case(ctx, esc); c.Status != cases.Closed || c.Resolution != cases.Dismissed {
		t.Fatalf("after dismiss: %s %s", c.Status, c.Resolution)
	}
}
