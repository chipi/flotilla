package ui

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/chipi/flotilla/internal/cases"
	"github.com/chipi/flotilla/internal/spec"
	"github.com/chipi/flotilla/internal/store"
)

// RFC-0010 §3 in the UI: a github.open_pr plan with an executing routine shows its diff and what
// approving starts; once approved nothing asks a person for a commit.
func TestOpenPRInTheUI(t *testing.T) {
	ex := &spec.Routine{Fleet: "bugfix", Routine: "open-pr", Description: "d", Enabled: true, Worker: "bugfix", Autonomy: "propose",
		Trigger: spec.Trigger{Execute: &spec.ExecuteTrigger{Action: "github.open_pr"}}, Run: spec.Run{Command: "true"}, Limit: time.Minute}
	f := setupWith(t, []*spec.Routine{ex})
	ctx := context.Background()
	target := "repo:host/registry.json/backup.rerun/pre_shadow"
	k := store.Key{Kind: "target", Value: "process_change/" + target}
	id, _, err := f.st.OpenCase(ctx, store.NewCase{Kind: "process_change", Target: target, Keys: []store.Key{k}, Canonical: k}, "ops/case-review", "")
	if err != nil {
		t.Fatal(err)
	}
	_ = f.st.MoveCase(ctx, id, cases.HandedOver, cases.ByDetector, "", "ops/case-review", "")
	_ = f.st.MoveCase(ctx, id, cases.Planned, cases.ByAct, "", "ops/process-change", "")
	diff := "--- a/host/registry.json\n+++ b/host/registry.json\n@@ -10 +10 @@\n-      \"pre_shadow\": [\"backup_age_above(20h)\"],\n+      \"pre_shadow\": [\"backup_age_above(25h)\"],\n"
	if _, err := f.st.ApplyPlan(ctx, store.PlanRecord{CaseID: id, Action: "github.open_pr",
		Params:   map[string]string{"repo": "chipi/flotilla", "base": "abc1234", "title": "pre_shadow: 20h -> 25h", "body": "why", "diff": diff},
		PlanHash: "h-pr-0123456789", Outcome: "propose", Autonomy: "propose"}, cases.AwaitingApproval, "", target, true); err != nil {
		t.Fatal(err)
	}
	ck, csrf := f.csrfOf(f.addOperator())
	_, page := f.do("GET", "/approvals", ck, nil, nil)
	for _, want := range []string{"github.open_pr on chipi/flotilla at abc1234", "pre_shadow: 20h -&gt; 25h",
		`&#43;      &#34;pre_shadow&#34;: [&#34;backup_age_above(25h)&#34;],`, "bugfix/open-pr opens it as a draft PR, once", "You merge, always"} {
		if !strings.Contains(page, want) {
			t.Errorf("Approvals has no %q", want)
		}
	}
	resp, _ := f.do("POST", "/cases/"+id+"/approve", ck, url.Values{"csrf": {csrf}, "plan": {"h-pr-0123456789"}}, nil)
	if loc := resp.Header.Get("Location"); !strings.Contains(loc, "draft+PR") && !strings.Contains(loc, "draft%20PR") {
		t.Fatalf("approving a PR: %s", loc)
	}
	_, page = f.do("GET", "/cases/"+id, ck, nil, nil)
	if strings.Contains(page, `action="/cases/`+id+`/applied"`) {
		t.Fatal("a PR its routine opens asks a person for the commit")
	}
	if !strings.Contains(page, "The diff it opens as a draft PR:") {
		t.Fatal("the case page does not show the diff")
	}
}

// Without an executing routine (the bugfix fleet not running), a PR plan still shows its diff, and a
// person applies it and records the commit.
func TestOpenPRByHandWithoutItsRoutine(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	target := "repo:host/registry.json/backup.rerun/pre_shadow"
	k := store.Key{Kind: "target", Value: "process_change/" + target}
	id, _, err := f.st.OpenCase(ctx, store.NewCase{Kind: "process_change", Target: target, Keys: []store.Key{k}, Canonical: k}, "ops/case-review", "")
	if err != nil {
		t.Fatal(err)
	}
	_ = f.st.MoveCase(ctx, id, cases.HandedOver, cases.ByDetector, "", "ops/case-review", "")
	_ = f.st.MoveCase(ctx, id, cases.Planned, cases.ByAct, "", "ops/process-change", "")
	if _, err := f.st.ApplyPlan(ctx, store.PlanRecord{CaseID: id, Action: "github.open_pr",
		Params:   map[string]string{"repo": "chipi/flotilla", "base": "abc1234", "title": "pre_shadow: 20h -> 25h", "body": "why", "diff": "+ the diff line"},
		PlanHash: "h-pr-hand-0123456789", Outcome: "propose", Autonomy: "propose"}, cases.AwaitingApproval, "", target, true); err != nil {
		t.Fatal(err)
	}
	ck, csrf := f.csrfOf(f.addOperator())
	_, page := f.do("GET", "/approvals", ck, nil, nil)
	for _, want := range []string{"github.open_pr on chipi/flotilla at abc1234", "&#43; the diff line", "nothing runs by itself"} {
		if !strings.Contains(page, want) {
			t.Errorf("Approvals has no %q", want)
		}
	}
	f.do("POST", "/cases/"+id+"/approve", ck, url.Values{"csrf": {csrf}, "plan": {"h-pr-hand-0123456789"}}, nil)
	if _, page = f.do("GET", "/cases/"+id, ck, nil, nil); !strings.Contains(page, `action="/cases/`+id+`/applied"`) {
		t.Fatal("without its routine, the approved PR plan does not ask for the commit")
	}
}
