package ui

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chipi/flotilla/internal/cases"
	"github.com/chipi/flotilla/internal/spec"
	"github.com/chipi/flotilla/internal/store"
)

const evilSop = "---\nname: backup-stale\ndescription: Use when a backup is stale.\napplies_to: {\"kinds\":[\"<b>x</b>\"]}\nrung: 1\n---\nRerun <script>alert(1)</script> it.\n"

// fleetsWithKnowledge writes a fleets folder: triage declares a sops collection, bugfix declares nothing.
func fleetsWithKnowledge(t *testing.T) map[string]spec.Fleet {
	t.Helper()
	d := t.TempDir()
	for rel, body := range map[string]string{
		"triage/fleet.yaml":                 "fleet: triage\ndescription: x\nknowledge:\n  - name: sops\n    title: Standard operating procedures\n    path: sops\n    format: skill\n    fields: [applies_to, rung]\n    proposals: true\n",
		"triage/sops/backup-stale/SKILL.md": evilSop,
		"bugfix/fleet.yaml":                 "fleet: bugfix\ndescription: y\n",
	} {
		p := filepath.Join(d, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	fs, err := spec.LoadFleets(d, nil)
	if err != nil {
		t.Fatal(err)
	}
	return fs
}

func openProposal(t *testing.T, f *fixture, sop string) string {
	t.Helper()
	data, _ := json.Marshal(map[string]any{"sop_id": sop, "mode": "add", "fleet": "triage", "path": "triage/sops/" + sop + "/SKILL.md",
		"body": "x", "evidence": []string{"case-1"}, "diff": "--- /dev/null\n+++ b/triage/sops/" + sop + "/SKILL.md\n+<i>new</i>\n"})
	k := store.Key{Kind: "target", Value: "sop_proposal/sop:triage/" + sop}
	id, _, err := f.st.OpenCase(context.Background(), store.NewCase{Kind: "sop_proposal", Target: "sop:triage/" + sop, Title: "SOP add: triage/" + sop,
		Keys: []store.Key{k}, Canonical: k, Data: string(data)}, "ops/sop-draft", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.st.MoveCase(context.Background(), id, cases.HandedOver, cases.ByDetector, "", "ops/sop-draft", ""); err != nil {
		t.Fatal(err)
	}
	return id
}

// A fleet that declares knowledge gets a tab, one without gets none, and a fleet that is gone takes its tab with it.
func TestKnowledgeTabPerDeclaringFleet(t *testing.T) {
	f := setup(t)
	ck, _ := f.csrfOf(f.addOperator())
	_, page := f.do("GET", "/knowledge", ck, nil, nil)
	if strings.Contains(page, "Standard operating procedures") {
		t.Fatal("a tab without any fleet declaring it")
	}
	f.ui.Fleets = fleetsWithKnowledge(t)
	_, page = f.do("GET", "/knowledge", ck, nil, nil)
	if !strings.Contains(page, `href="/knowledge?tab=triage.sops"`) || !strings.Contains(page, "Triage:") || strings.Contains(page, "Bugfix:") {
		t.Fatalf("the tabs: %s", page)
	}
	resp, tab := f.do("GET", "/knowledge?tab=triage.sops", ck, nil, nil)
	if resp.StatusCode != 200 || !strings.Contains(tab, `aria-selected="true">Standard operating procedures`) || !strings.Contains(tab, "backup-stale") {
		t.Fatalf("the tab: %d %s", resp.StatusCode, tab)
	}
	// the declared fields are columns, as escaped text; nothing from a file is HTML
	if !strings.Contains(tab, "&lt;b&gt;x&lt;/b&gt;") || strings.Contains(tab, "<b>x</b>") {
		t.Fatalf("a field was not escaped: %s", tab)
	}
	if !strings.Contains(tab, ">applies_to<") || !strings.Contains(tab, ">rung<") || !strings.Contains(tab, "not read yet") {
		t.Fatalf("columns or use missing: %s", tab)
	}
	// the item's page: the full body as escaped text
	resp, item := f.do("GET", "/knowledge/triage/sops/backup-stale", ck, nil, nil)
	if resp.StatusCode != 200 || !strings.Contains(item, "&lt;script&gt;alert(1)&lt;/script&gt;") || strings.Contains(item, "<script>alert") {
		t.Fatalf("the item page: %d %s", resp.StatusCode, item)
	}
	if resp, _ := f.do("GET", "/knowledge/triage/sops/nope", ck, nil, nil); resp.StatusCode != 404 {
		t.Fatalf("an unknown item: %d", resp.StatusCode)
	}
	if resp, _ := f.do("GET", "/knowledge/triage/nope/backup-stale", ck, nil, nil); resp.StatusCode != 404 {
		t.Fatalf("an unknown collection: %d", resp.StatusCode)
	}
	// the fleet's page links to its knowledge
	if _, fp := f.do("GET", "/fleets/triage", ck, nil, nil); !strings.Contains(fp, `href="/knowledge?tab=triage.sops"`) {
		t.Fatalf("the fleet page has no link to its knowledge: %s", fp)
	}
	// removed: its tab is gone and the old address falls back to Memory
	delete(f.ui.Fleets, "triage")
	_, page = f.do("GET", "/knowledge?tab=triage.sops", ck, nil, nil)
	if strings.Contains(page, "Standard operating procedures") || !strings.Contains(page, "Notes waiting for you") {
		t.Fatalf("a removed fleet's tab: %s", page)
	}
}

// An item shows which runs read it, by the sha256 of its page, with the latest linked.
func TestKnowledgeItemUse(t *testing.T) {
	f := setup(t)
	f.ui.Fleets = fleetsWithKnowledge(t)
	it := f.ui.Fleets["triage"].Knowledge[0].Items[0]
	ctx := context.Background()
	for _, run := range []string{"run-a", "run-b"} {
		if err := f.st.RecordLoaded(ctx, []store.Loaded{{RunID: run, Routine: "triage/sop-act", Kind: "skill", Trust: "source", Title: "backup-stale",
			Source: "fleet sop backup-stale @ v1", SHA256: it.Hash}}); err != nil {
			t.Fatal(err)
		}
	}
	// another fleet's run of a same-named skill is not this item's use
	if err := f.st.RecordLoaded(ctx, []store.Loaded{{RunID: "run-z", Routine: "bugfix/fix", Kind: "skill", Trust: "source", Title: "backup-stale", Source: "fleet skill backup-stale @ v1"}}); err != nil {
		t.Fatal(err)
	}
	ck, _ := f.csrfOf(f.addOperator())
	_, tab := f.do("GET", "/knowledge?tab=triage.sops", ck, nil, nil)
	if !strings.Contains(tab, "2 runs read it") || !strings.Contains(tab, `href="/runs/run-b"`) || strings.Contains(tab, "run-z") {
		t.Fatalf("the use: %s", tab)
	}
}

// The proposals for a collection wait on its tab with their diff, and only a person with the operator role sees the buttons.
func TestKnowledgeProposalsAcceptAndReject(t *testing.T) {
	f := setup(t)
	f.ui.Fleets = fleetsWithKnowledge(t)
	ctx := context.Background()
	acc, rej := openProposal(t, f, "accepted-one"), openProposal(t, f, "rejected-one")
	ck, csrf := f.csrfOf(f.addOperator())
	_, tab := f.do("GET", "/knowledge?tab=triage.sops", ck, nil, nil)
	for _, want := range []string{"Waiting for you", "sops/accepted-one/SKILL.md", "&#43;&lt;i&gt;new&lt;/i&gt;", `action="/cases/` + acc + `/accept"`, `action="/cases/` + rej + `/reject"`} {
		if !strings.Contains(tab, want) {
			t.Errorf("the tab lacks %q", want)
		}
	}
	if strings.Contains(tab, "<i>new</i>") {
		t.Error("a diff was not escaped")
	}
	vck, vcsrf := f.csrfOf(f.vic)
	_, vtab := f.do("GET", "/knowledge?tab=triage.sops", vck, nil, nil)
	if !strings.Contains(vtab, "Waiting for you") || !strings.Contains(vtab, "accepted-one") || strings.Contains(vtab, "/accept\"") || strings.Contains(vtab, "/reject\"") {
		t.Fatalf("a viewer's tab: %s", vtab)
	}
	// a viewer's answer is refused, even with a valid session and token
	f.do("POST", "/cases/"+acc+"/accept", vck, url.Values{"csrf": {vcsrf}}, nil)
	if c, _ := f.st.Case(ctx, acc); c.Status != cases.HandedOver {
		t.Fatalf("a viewer accepted: %s", c.Status)
	}
	// without the CSRF token nothing moves
	if resp, _ := f.do("POST", "/cases/"+acc+"/accept", ck, url.Values{}, nil); resp.StatusCode != 403 {
		t.Fatalf("accept without CSRF: %d", resp.StatusCode)
	}
	resp, _ := f.do("POST", "/cases/"+acc+"/accept", ck, url.Values{"csrf": {csrf}, "back": {"/knowledge?tab=triage.sops"}}, nil)
	if loc := resp.Header.Get("Location"); strings.Contains(loc, "err=") || !strings.HasPrefix(loc, "/knowledge?tab=triage.sops&msg=") {
		t.Fatalf("accept: %s", loc)
	}
	c, _ := f.st.Case(ctx, acc)
	if c.Status != cases.Closed || c.Resolution != cases.OperatorResolved {
		t.Fatalf("after accept: %s %s", c.Status, c.Resolution)
	}
	ev, _ := f.st.CaseEvents(ctx, acc)
	last := ev[len(ev)-1]
	if last.Kind != "case.closed" || last.Actor != "user:otto" || last.By != cases.ByOperator || !strings.Contains(last.Detail, "accepted: commit the diff") {
		t.Fatalf("the accept's event: %+v", last)
	}
	// a reject needs a reason
	resp, _ = f.do("POST", "/cases/"+rej+"/reject", ck, url.Values{"csrf": {csrf}}, nil)
	if !strings.Contains(resp.Header.Get("Location"), "err=") {
		t.Fatalf("reject without a reason: %s", resp.Header.Get("Location"))
	}
	f.do("POST", "/cases/"+rej+"/reject", ck, url.Values{"csrf": {csrf}, "reason": {"too wide"}}, nil)
	c, _ = f.st.Case(ctx, rej)
	if c.Status != cases.Closed || c.Resolution != cases.Dismissed {
		t.Fatalf("after reject: %s %s", c.Status, c.Resolution)
	}
	ev, _ = f.st.CaseEvents(ctx, rej)
	if last := ev[len(ev)-1]; last.Actor != "user:otto" || !strings.HasSuffix(last.Detail, "rejected: too wide") {
		t.Fatalf("the reject's event: %+v", last)
	}
	// both are answered: nothing waits any more
	_, tab = f.do("GET", "/knowledge?tab=triage.sops", ck, nil, nil)
	if !strings.Contains(tab, "No proposal waits") {
		t.Fatalf("answered proposals still wait: %s", tab)
	}
}

// Accept is no answer on any other case: a plan's page and an escalation keep the answers they had.
func TestAcceptRefusedOnOtherCases(t *testing.T) {
	f := setup(t)
	f.ui.Fleets = fleetsWithKnowledge(t)
	k := store.Key{Kind: "target", Value: "failed_backup/host:a"}
	id, _, err := f.st.OpenCase(context.Background(), store.NewCase{Kind: "failed_backup", Target: "host:a", Keys: []store.Key{k}, Canonical: k}, "ops/backup-monitor", "")
	if err != nil {
		t.Fatal(err)
	}
	_ = f.st.MoveCase(context.Background(), id, cases.HandedOver, cases.ByDetector, "", "ops/backup-monitor", "")
	ck, csrf := f.csrfOf(f.addOperator())
	resp, _ := f.do("POST", "/cases/"+id+"/accept", ck, url.Values{"csrf": {csrf}}, nil)
	if !strings.Contains(resp.Header.Get("Location"), "err=") {
		t.Fatalf("accept on a backup case: %s", resp.Header.Get("Location"))
	}
	if c, _ := f.st.Case(context.Background(), id); c.Status != cases.HandedOver {
		t.Fatalf("moved: %s", c.Status)
	}
}
