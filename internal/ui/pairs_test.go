package ui

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/chipi/flotilla/internal/store"
)

// pairsFixture is triage/cycle with v1 live and v2, v3 shadows; v2 ran beside v1 on one input and decided four things:
// two the same, one differently, one the live version did not, and the live version decided one the shadow did not.
func pairsFixture(t *testing.T) (*fixture, string) {
	f := versionedFixture(t, "")
	ctx := context.Background()
	st := f.st
	now := time.Now().UTC().Format(store.TimeFormat)
	key := "triage-cycle-live-1"
	if err := st.StartRun(ctx, store.Run{ID: key, Routine: "triage/cycle", SpecHash: "h", Trigger: "schedule", Actor: "flotillad", Autonomy: "record", StartedAt: now, Status: store.StatusRunning}); err != nil {
		t.Fatal(err)
	}
	if err := st.NewInput(ctx, store.Input{Key: key, Routine: "triage/cycle", Kind: store.InputTick, Version: 1, StartedAt: now}); err != nil {
		t.Fatal(err)
	}
	live := func(fp, disp, reason string) {
		if _, err := st.AppendEntry(ctx, "triage/cycle", "decision", map[string]string{"fingerprint": fp, "disposition": disp, "reason": reason}, key); err != nil {
			t.Fatal(err)
		}
	}
	live("glitchtip:api-500", "file", "a real error")
	live("grafana:disk-usage-mini", "clear", "recovered")
	live("grafana:only-live", "dismiss", "noise")
	live("glitchtip:web-404", "file", "a new one")
	id, err := st.QueueShadowRun(ctx, store.ShadowRun{InputKey: key, Routine: "triage/cycle", Version: 2, VersionHash: "abcdef0123"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ShadowRunStep(ctx, id, store.ShadowRunning, "", "triage-cycle-v2-run"); err != nil {
		t.Fatal(err)
	}
	if err := st.ShadowRunStep(ctx, id, store.ShadowDone, "", ""); err != nil {
		t.Fatal(err)
	}
	rec := func(subject, payload string) {
		if _, err := st.RecordShadow(ctx, store.ShadowRecord{RunID: "triage-cycle-v2-run", Routine: "triage/cycle@v2", Version: 2, VersionHash: "abcdef0123", InputKey: key, Subject: subject, Kind: "decision", Payload: json.RawMessage(payload)}); err != nil {
			t.Fatal(err)
		}
	}
	rec("glitchtip:api-500", `{"disposition":"attach","reason":"it is case #812"}`)
	rec("grafana:disk-usage-mini", `{"disposition":"clear"}`)
	rec("glitchtip:web-404", `{"disposition":"file"}`)
	rec("grafana:only-shadow", `{"disposition":"escalate"}`)
	in, cost := int64(120), 0.002
	st.RecordCalls(ctx, []store.ModelCall{{RunID: key, Routine: "triage/cycle", Alias: "a", Role: "triage", TokensIn: &in, CostUSD: &cost}, {RunID: "triage-cycle-v2-run", Routine: "triage/cycle@v2", Alias: "a", Role: "triage", TokensIn: &in, CostUSD: &cost}})
	// a station on each side, reading two texts: its task is the same text on both, its system text differs
	txt := func(s string) *string { return &s }
	same, live1, shadow1 := "the same task", "the live system text", "the shadow system text"
	mk := func(run, routine, sys string) store.ModelCall {
		return store.ModelCall{RunID: run, Routine: routine, Alias: "a", Station: "decide", TokensIn: &in, CostUSD: &cost, TextBodies: []store.CallText{
			{Name: "task", SHA256: shaOf(same), Bytes: int64(len(same)), Text: txt(same), Redacted: true},
			{Name: "system", SHA256: shaOf(sys), Bytes: int64(len(sys)), Text: txt(sys), Redacted: true}}}
	}
	st.RecordCalls(ctx, []store.ModelCall{mk(key, "triage/cycle", live1), mk("triage-cycle-v2-run", "triage/cycle@v2", shadow1)})
	return f, key
}

func shaOf(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func pairsSection(t *testing.T, page string) string {
	t.Helper()
	i := strings.Index(page, `id="pairs"`)
	if i < 0 {
		t.Fatal("the page has no #pairs section")
	}
	sec := page[i:]
	j := strings.Index(sec, "</section>")
	if j < 0 {
		t.Fatal("the #pairs section is not closed")
	}
	return sec[:j]
}

// The routine page's pairs list as the operator drew it: a header with the counts, the filter, and per row a mark, the subject, the time
// and what each side decided.
func TestRoutinePagePairsList(t *testing.T) {
	f, key := pairsFixture(t)
	ck, _ := f.csrfOf(f.addOperator())
	_, page := f.do("GET", "/fleets/triage/cycle", ck, nil, nil)
	sec := pairsSection(t, page)
	for _, want := range []string{"triage/cycle · v2 shadow vs v1 live", "3 pairs · 1 differ", ">all<", ">differ<", ">unpaired 2<",
		"● differ", "● same", "● unpaired", "glitchtip:api-500", "live v1", "file", "attach", "did not decide it",
		`href="/fleets/triage/cycle/pairs/2/` + key + `?subject=glitchtip%3Aapi-500"`, `aria-current="true"`} {
		if !strings.Contains(sec, want) {
			t.Errorf("the pairs list lacks %q:\n%s", want, sec)
		}
	}
	if strings.Contains(sec, "<table") {
		t.Error("pairs are rows that wrap, never a table that scrolls sideways")
	}
	// the filter
	_, page = f.do("GET", "/fleets/triage/cycle?pairs=2&show=differ", ck, nil, nil)
	sec = pairsSection(t, page)
	if !strings.Contains(sec, "glitchtip:api-500") || strings.Contains(sec, "grafana:disk-usage-mini") || strings.Contains(sec, "grafana:only-shadow") {
		t.Errorf("differ shows the pairs that differ only:\n%s", sec)
	}
	_, page = f.do("GET", "/fleets/triage/cycle?show=unpaired", ck, nil, nil)
	sec = pairsSection(t, page)
	if !strings.Contains(sec, "grafana:only-live") || !strings.Contains(sec, "grafana:only-shadow") || strings.Contains(sec, "glitchtip:api-500") {
		t.Errorf("unpaired shows the subjects one side decided:\n%s", sec)
	}
	// the versions table links to them, and says how many
	vs := versionsSection(t, mustGet(f, ck, "/fleets/triage/cycle"))
	if !strings.Contains(vs, `href="/fleets/triage/cycle?pairs=2#pairs"`) || !strings.Contains(vs, "3 compared pairs") {
		t.Errorf("the versions table does not lead to the pairs: %s", vs)
	}
}

func mustGet(f *fixture, ck, path string) string {
	_, page := f.do("GET", path, ck, nil, nil)
	return page
}

// A routine with no finished shadow run has no pairs section.
func TestNoPairsSectionWithoutAShadowRun(t *testing.T) {
	f := versionedFixture(t, "")
	ck, _ := f.csrfOf(f.addOperator())
	if page := mustGet(f, ck, "/fleets/triage/cycle"); strings.Contains(page, `id="pairs"`) {
		t.Error("a pairs section with no shadow run")
	}
}

// Tapping a row opens the two runs side by side.
func TestAPairOpensItsTwoSides(t *testing.T) {
	f, key := pairsFixture(t)
	ck, _ := f.csrfOf(f.addOperator())
	path := "/fleets/triage/cycle/pairs/2/" + key + "?subject=" + url.QueryEscape("glitchtip:api-500")
	resp, page := f.do("GET", path, ck, nil, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("%d", resp.StatusCode)
	}
	for _, want := range []string{"glitchtip:api-500", "pairmark--differ", "live v1", ">v2<", "a real error", "it is case #812", "abcdef0", "Turns", "triage",
		`href="/runs/` + key + `"`, `href="/runs/triage-cycle-v2-run"`, "$0.002"} {
		if !strings.Contains(page, want) {
			t.Errorf("the pair page lacks %q", want)
		}
	}
	_, page = f.do("GET", "/fleets/triage/cycle/pairs/2/"+key+"?subject="+url.QueryEscape("grafana:only-live"), ck, nil, nil)
	if !strings.Contains(page, "did not decide this subject on this input") || !strings.Contains(page, "pairmark--unpaired") {
		t.Errorf("an unpaired subject says which side did not decide it")
	}
	for _, p := range []string{"/fleets/triage/cycle/pairs/2/" + key + "?subject=nope", "/fleets/triage/cycle/pairs/9/" + key + "?subject=x", "/fleets/triage/cycle/pairs/zz/" + key} {
		if resp, _ := f.do("GET", p, ck, nil, nil); resp.StatusCode != 404 {
			t.Errorf("%s: %d", p, resp.StatusCode)
		}
	}
}

// A shadow run is a run: its page opens, and the run lists do not hold it.
func TestAShadowRunHasAPageButIsNotInTheRunList(t *testing.T) {
	f, _ := pairsFixture(t)
	ck, _ := f.csrfOf(f.addOperator())
	f.st.StartRun(context.Background(), store.Run{ID: "triage-cycle-v2-run", Routine: "triage/cycle@v2", SpecHash: "h", Trigger: "shadow", Actor: "flotillad", Autonomy: "record",
		StartedAt: time.Now().UTC().Format(store.TimeFormat), Status: store.StatusOK, Version: 2, Shadow: true, InputKey: "triage-cycle-live-1"})
	if _, list := f.do("GET", "/runs", ck, nil, nil); strings.Contains(list, "triage-cycle-v2-run") || strings.Contains(list, "@v2") {
		t.Error("a shadow run is in the run list")
	}
}

// The brief is the operator's morning page: a shadow's failed run, its cost and its records must not move any line of it.
func TestABriefHoldsNoShadowRow(t *testing.T) {
	f := versionedFixture(t, "")
	ctx := context.Background()
	now := time.Now()
	before, err := f.ui.composeBrief(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	at := now.UTC().Format(store.TimeFormat)
	if err := f.st.StartRun(ctx, store.Run{ID: "triage-cycle-v2-bad", Routine: "triage/cycle@v2", SpecHash: "h", Trigger: "shadow", Actor: "flotillad", Autonomy: "record",
		StartedAt: at, Status: store.StatusError, Detail: "boom", Version: 2, Shadow: true, InputKey: "triage-cycle-live-9"}); err != nil {
		t.Fatal(err)
	}
	in, cost := int64(1000), 9.5
	f.st.RecordCalls(ctx, []store.ModelCall{{RunID: "triage-cycle-v2-bad", Routine: "triage/cycle@v2", Alias: "a", Role: "triage", TokensIn: &in, CostUSD: &cost}})
	if _, err := f.st.RecordShadow(ctx, store.ShadowRecord{RunID: "triage-cycle-v2-bad", Routine: "triage/cycle@v2", Version: 2, InputKey: "triage-cycle-live-9", Subject: "s", Kind: "decision",
		Payload: json.RawMessage(`{"disposition":"file"}`)}); err != nil {
		t.Fatal(err)
	}
	after, err := f.ui.composeBrief(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal([]any{before.Summary, before.Sections})
	b, _ := json.Marshal([]any{after.Summary, after.Sections})
	if string(a) != string(b) || strings.Contains(string(b), "@v") || strings.Contains(string(b), "9.5") {
		t.Fatalf("a shadow moved the brief:\n%s\n%s", a, b)
	}
}

// A shadow run's page opens, and its routine link goes to a page that exists: the live routine's pairs of that version, not a key no page has.
func TestAShadowRunPageLinksToItsRoutinesPairs(t *testing.T) {
	f, key := pairsFixture(t)
	ck, _ := f.csrfOf(f.addOperator())
	if err := f.st.StartRun(context.Background(), store.Run{ID: "triage-cycle-v2-page", Routine: "triage/cycle@v2", SpecHash: "h", Trigger: "shadow", Actor: "flotillad", Autonomy: "record",
		StartedAt: time.Now().UTC().Format(store.TimeFormat), Status: store.StatusOK, Version: 2, Shadow: true, InputKey: key}); err != nil {
		t.Fatal(err)
	}
	resp, page := f.do("GET", "/runs/triage-cycle-v2-page", ck, nil, nil)
	if resp.StatusCode != 200 || !strings.Contains(page, `href="/fleets/triage/cycle?pairs=2#pairs"`) || strings.Contains(page, "?sel=triage/cycle@v2") {
		t.Fatalf("%d: the routine link of a shadow run: %s", resp.StatusCode, page[:min(len(page), 600)])
	}
	if resp, rp := f.do("GET", "/fleets/triage/cycle?pairs=2", ck, nil, nil); resp.StatusCode != 200 || !strings.Contains(rp, `id="pairs"`) {
		t.Fatalf("the link's target: %d", resp.StatusCode)
	}
	resp, page = f.do("GET", "/runs/"+key, ck, nil, nil)
	if resp.StatusCode != 200 || !strings.Contains(page, `href="/fleets/triage?sel=triage/cycle"`) {
		t.Fatalf("a live run's link is unchanged: %d", resp.StatusCode)
	}
}

// The rendered texts of each station show on both sides, each marked equal, differs or only here, and wrap inside the card (no sideways scroll).
func TestThePairPageShowsEachStationsTextsAndWhetherTheyAreEqual(t *testing.T) {
	f, key := pairsFixture(t)
	ck, _ := f.csrfOf(f.addOperator())
	_, page := f.do("GET", "/fleets/triage/cycle/pairs/2/"+key+"?subject="+url.QueryEscape("glitchtip:api-500"), ck, nil, nil)
	for _, want := range []string{"decide.task", "decide.system", "the same task", "the live system text", "the shadow system text", `class="stext"`, `class="stext__body"`} {
		if !strings.Contains(page, want) {
			t.Errorf("the pair page lacks %q", want)
		}
	}
	if strings.Count(page, "pairmark--same\">equal") != 2 || strings.Count(page, "pairmark--differ\">differs") != 2 {
		t.Errorf("the task is equal on both sides and the system text differs on both:\n%s", page)
	}
	if strings.Contains(page, "recorded from build step 3") {
		t.Error("the stale note about texts is gone")
	}
	css, _ := os.ReadFile("static/flotilla.css")
	for _, want := range []string{".stext__body", "white-space: pre-wrap", "overflow-wrap: anywhere", "max-width: 100%"} {
		if !strings.Contains(string(css), want) {
			t.Errorf("a text is wrapped inside the card at 360 px: the stylesheet lacks %q", want)
		}
	}
}
