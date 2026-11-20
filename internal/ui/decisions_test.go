package ui

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/chipi/flotilla/internal/store"
)

// The counting rules of the fleet session's decision_mix.py (the installation's research repo), one
// case each, so the Decisions tab and that script agree for a day.
func TestBuildMixCountsLikeDecisionMix(t *testing.T) {
	dec := func(ts, disp, stage, gates, fp string) store.LedgerDecision {
		return store.LedgerDecision{TS: ts, Kind: "decision", Disposition: disp, Stage: stage, Gates: gates, Fingerprint: fp, PromptSHA: "p1", Model: "m"}
	}
	ds := []store.LedgerDecision{
		dec("2026-11-14T10:00:00Z", "file", "propose", "i:passed/c:n/a/d:downgraded (no corroboration)/e:n/a", "glitchtip:SHOP-PROD-25"),
		dec("2026-11-14T11:00:00Z", "escalate", "live", "i:passed/c:n/a/d:n/a/e:fired", "grafana:abc"),
		dec("2026-11-14T12:00:00Z", "dismiss", "shadow", "", "glitchtip:SHOP-PROD-25"),       // shadow: not counted
		dec("2026-11-14T13:00:00Z", "file", "", "", "glitchtip:SHOP-PROD-25"),                // no stage: not counted
		dec("2026-11-14T14:00:00Z", "weird", "propose", "operational:x", "glitchtip:NODASH"), // other; operational is no gate
		dec("2026-11-15T09:00:00Z", "file", "propose", "", "sentry:x"),
		{TS: "2026-11-15T09:30:00Z", Kind: "recurrence", Disposition: "recurrence", Stage: "propose", Fingerprint: "glitchtip:SHOP-PROD-25"},
	}
	m := buildMix(ds, []string{"2026-11-14", "2026-11-15"})
	if len(m.Days) != 2 || m.Days[0].Day != "2026-11-15" || m.Days[1].Day != "2026-11-14" {
		t.Fatalf("days newest first: %+v", m.Days)
	}
	d14, d15 := m.Days[1], m.Days[0]
	if d14.Total != 3 || d15.Total != 1 || d15.Rec != 1 || d14.Rec != 0 || m.Total != 4 || m.Shadow != 1 {
		t.Fatalf("totals: 14th %d, 15th %d rec %d, all %d, shadow %d", d14.Total, d15.Total, d15.Rec, m.Total, m.Shadow)
	}
	got := map[string]int{}
	for _, s := range d14.Segs {
		got[s.Label] = s.N
	}
	if got["file"] != 1 || got["escalate"] != 1 || got["other"] != 1 || got["dismiss"] != 0 {
		t.Fatalf("14th by disposition: %v", got)
	}
	if d14.Gates != "d 1 · e 1" {
		t.Fatalf("gates fired: %q", d14.Gates)
	}
	proj := map[string]string{}
	for _, p := range m.Projects {
		proj[p.Name] = p.Line
	}
	if proj["SHOP-PROD"] != "file 1" || proj["grafana"] != "escalate 1" || proj["unknown"] != "other 1" || proj["other"] != "file 1" {
		t.Fatalf("projects: %v", proj)
	}
	// the busiest day's bar is full width, the others to scale, each segment after the one before
	if last := d14.Segs[len(d14.Segs)-1]; last.X+last.Pct < 99.9 {
		t.Fatalf("busiest day bar: %+v", d14.Segs)
	}
	if d15.Segs[0].Pct > 34 {
		t.Fatalf("a 1-decision day against a 3-decision one: %+v", d15.Segs)
	}
}

// A prompt or model change is marked on the day it happened, and one before the window is not.
func TestBuildMixMarksChanges(t *testing.T) {
	d := func(ts, sha, model string) store.LedgerDecision {
		return store.LedgerDecision{TS: ts, Kind: "decision", Disposition: "file", Stage: "propose", PromptSHA: sha, Model: model}
	}
	ds := []store.LedgerDecision{
		d("2026-11-10T08:00:00Z", "aaaaaaaaaa", "flash"),
		d("2026-11-11T08:00:00Z", "bbbbbbbbbb", "flash"), // before the window
		d("2026-11-14T08:00:00Z", "bbbbbbbbbb", "flash"),
		d("2026-11-15T09:12:00Z", "cccccccccc", "flash"),
		d("2026-11-15T10:00:00Z", "cccccccccc", "pro"),
	}
	m := buildMix(ds, []string{"2026-11-14", "2026-11-15"})
	if len(m.Days[1].Changes) != 0 {
		t.Fatalf("14th: %v", m.Days[1].Changes)
	}
	want := []string{"2026-11-15 09:12 UTC: prompt bbbbbbb → ccccccc", "2026-11-15 10:00 UTC: model flash → pro"}
	if strings.Join(m.Days[0].Changes, "|") != strings.Join(want, "|") {
		t.Fatalf("15th: %v", m.Days[0].Changes)
	}
}

// The tab exists only for a fleet with decisions, reads the store, and the brief has the line.
func TestDecisionsTab(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	ck, _ := f.csrfOf(f.addOperator())
	if _, page := f.do("GET", "/fleets/triage", ck, nil, nil); strings.Contains(page, "?tab=decisions") {
		t.Fatal("a Decisions tab on a fleet with no decision")
	}
	now := time.Now().UTC()
	// the page counts a UTC day's decisions together, so the three must be on one UTC day whatever the time is: minutes before now straddled
	// midnight for the first three minutes of every UTC day (the failing run of 2026-11-20 00:02 UTC)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	for i, disp := range []string{"file", "file", "escalate"} {
		ts := today.Format(time.RFC3339)
		if _, err := f.st.AppendEntry(ctx, "triage/cycle", "decision", map[string]string{"ts": ts, "disposition": disp, "stage": "propose",
			"occurrence_id": "o" + string(rune('a'+i)), "fingerprint": "glitchtip:SHOP-PROD-25", "gates": "i:passed/d:fired"}, "r1"); err != nil {
			t.Fatal(err)
		}
	}
	_, page := f.do("GET", "/fleets/triage?tab=decisions", ck, nil, nil)
	for _, want := range []string{"?tab=decisions", "<strong>3</strong> decisions", "file 2 · escalate 1 · gates fired: d 3", "SHOP-PROD"} {
		if !strings.Contains(page, want) {
			t.Fatalf("decisions page lacks %q", want)
		}
	}
	yesterday := now.AddDate(0, 0, -1).Format(time.RFC3339)
	if _, err := f.st.AppendEntry(ctx, "triage/cycle", "decision", map[string]string{"ts": yesterday, "disposition": "dismiss", "stage": "propose",
		"occurrence_id": "oy", "fingerprint": "grafana:x"}, "r0"); err != nil {
		t.Fatal(err)
	}
	b, err := f.ui.composeBrief(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	var line string
	for _, s := range b.Sections {
		if s.Title == "Decisions" && len(s.Items) == 1 {
			line = s.Items[0].Text
		}
	}
	if !strings.HasPrefix(line, "Triage decided 1 on "+now.AddDate(0, 0, -1).Format("2006-01-02")+" (dismiss 1), against 0 a day over 14 days") {
		t.Fatalf("brief line: %q", line)
	}
}
