package ui

import (
	"context"
	"net/url"
	"strings"
	"testing"

	"github.com/chipi/flotilla/internal/cases"
	"github.com/chipi/flotilla/internal/store"
)

// ADR-0016 in the UI: a process change in Approvals shows its diff and what approving means; once
// approved, its case asks for the commit, and recording it closes the case as verified.
func TestProcessChangeInTheUI(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	target := "repo:host/registry.json/backup.rerun_observability/pre_shadow"
	k := store.Key{Kind: "target", Value: "process_change/" + target}
	id, _, err := f.st.OpenCase(ctx, store.NewCase{Kind: "process_change", Target: target, Keys: []store.Key{k}, Canonical: k,
		Title: "The pre_shadow threshold of backup.rerun_observability (20h) is below the source's 24h cycle"}, "ops/case-review", "")
	if err != nil {
		t.Fatal(err)
	}
	_ = f.st.MoveCase(ctx, id, cases.HandedOver, cases.ByDetector, "", "ops/case-review", "")
	_ = f.st.MoveCase(ctx, id, cases.Planned, cases.ByAct, "", "ops/process-change", "")
	change := "host/registry.json, action backup.rerun_observability, pre_shadow:\n- \"backup_age_above(20h)\"\n+ \"backup_age_above(25h)\""
	if _, err := f.st.ApplyPlan(ctx, store.PlanRecord{CaseID: id, Action: "repo.process_change",
		Params: map[string]string{"file": "host/registry.json", "rule": "below_cycle", "change": change}, PlanHash: "h-pc-0123456789",
		Outcome: "propose", Autonomy: "propose"}, cases.AwaitingApproval, "", target, true); err != nil {
		t.Fatal(err)
	}
	ck, csrf := f.csrfOf(f.addOperator())
	_, page := f.do("GET", "/approvals", ck, nil, nil)
	for _, want := range []string{`- &#34;backup_age_above(20h)&#34;`, `&#43; &#34;backup_age_above(25h)&#34;`, "nothing runs by itself",
		"record the commit here"} {
		if !strings.Contains(page, want) {
			t.Errorf("Approvals has no %q", want)
		}
	}
	resp, _ := f.do("POST", "/cases/"+id+"/approve", ck, url.Values{"csrf": {csrf}, "plan": {"h-pc-0123456789"}}, nil)
	if loc := resp.Header.Get("Location"); !strings.Contains(loc, "record+its+commit") && !strings.Contains(loc, "record%20its%20commit") {
		t.Fatalf("approving a repo change: %s", loc)
	}
	if _, page = f.do("GET", "/cases/"+id, ck, nil, nil); !strings.Contains(page, `action="/cases/`+id+`/applied"`) || !strings.Contains(page, "The change it proposes:") {
		t.Fatal("an approved repo change's case does not ask for the commit, or does not show the change")
	}
	resp, _ = f.do("POST", "/cases/"+id+"/applied", ck, url.Values{"csrf": {csrf}, "commit": {"1dd224c"}}, nil)
	if !strings.Contains(resp.Header.Get("Location"), "msg=") {
		t.Fatalf("recording the commit: %s", resp.Header.Get("Location"))
	}
	if c, _ := f.st.Case(ctx, id); c.Status != cases.Closed || c.Resolution != cases.Verified {
		t.Fatalf("after the commit: %s %s", c.Status, c.Resolution)
	}
}
