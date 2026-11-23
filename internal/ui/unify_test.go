package ui

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/chipi/flotilla/internal/store"
)

// One UI (docs/wip/ui-unify-2026-11-23.md): a fleet's "needs you" opens its cases, a pivot Cases understands.
func TestCasesPivotOnFleet(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	for _, c := range []struct{ opener, target string }{{"jobs/backup-monitor", "host:a"}, {"seo/site-check", "site:b"}} {
		k := store.Key{Kind: "target", Value: c.target}
		if _, _, err := f.st.OpenCase(ctx, store.NewCase{Kind: "failed_backup", Target: c.target, Keys: []store.Key{k}, Canonical: k}, c.opener, ""); err != nil {
			t.Fatal(err)
		}
	}
	ck := f.signin(f.admin)
	_, page := f.do("GET", "/cases?p=fleet%3Ajobs", ck, nil, nil)
	if !strings.Contains(page, "host:a") || strings.Contains(page, "site:b") {
		t.Error("the fleet pivot does not keep only that fleet's cases")
	}
	if !strings.Contains(page, `fleet = <strong>jobs</strong>`) {
		t.Error("the fleet pivot is not shown as a removable filter")
	}
}

// A column that is empty on every row is left out: Hosts has no Roles column, and no load column while no host reports one.
func TestHostsLeaveOutEmptyColumns(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	if _, err := f.st.AddEnrolment(ctx, "backup-01", "tok-backup-01", "user:ada", "test"); err != nil {
		t.Fatal(err)
	}
	if err := f.st.Enrol(ctx, "backup-01", "tok-backup-01", func() (string, time.Time, error) {
		return "cert-sha-backup-01", time.Now().Add(27 * 24 * time.Hour), nil
	}); err != nil {
		t.Fatal(err)
	}
	ck := f.signin(f.admin)
	_, page := f.do("GET", "/hosts", ck, nil, nil)
	for _, gone := range []string{">Roles<", "CPU · mem · disk"} {
		if strings.Contains(page, gone) {
			t.Errorf("Hosts shows the empty column %q", gone)
		}
	}
}

// Instruments are one line unless a row is off its usual or past a limit.
func TestInstrumentsOpenOnlyOnWorry(t *testing.T) {
	calm := instruments{Lead: "All within limits", Rows: []instRow{{Label: "turns"}}}
	if calm.Worry() {
		t.Error("a calm card says it worries")
	}
	calm.Rows = append(calm.Rows, instRow{Label: "spend", Level: "alert"})
	if !calm.Worry() {
		t.Error("a card with an alert row does not open")
	}
}

// A staged case's last event reads cleanly when its actor is unknown.
func TestEventShortWithoutActor(t *testing.T) {
	if got := eventShort(store.CaseEvent{Kind: "case.staged", To: "decisions"}); got != "handed on to decisions" {
		t.Errorf("%q", got)
	}
	if got := eventShort(store.CaseEvent{Kind: "case.staged", Actor: "meetings/notes", To: "decisions"}); got != "meetings/notes: handed on to decisions" {
		t.Errorf("%q", got)
	}
}

// STOP is red only as an outline at rest, on the fleets list and a fleet's own page alike.
func TestStopIsOutlinedAtRest(t *testing.T) {
	f := setup(t)
	ck := f.signin(f.admin)
	_, page := f.do("GET", "/fleets", ck, nil, nil)
	if strings.Contains(page, `class="btn btn--stop"`) {
		t.Error("the fleets list fills STOP red at rest")
	}
}
