package ui

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/chipi/flotilla/internal/cases"
	"github.com/chipi/flotilla/internal/store"
)

// The ledger explorer reads the three streams as one, follows one thing everywhere, filters by kind and exports the view.
func TestLedgerExplorer(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	k := store.Key{Kind: "target", Value: "failed_backup/host:backup-01"}
	id, _, err := f.st.OpenCase(ctx, store.NewCase{Kind: "failed_backup", Target: "host:backup-01", Keys: []store.Key{k}, Canonical: k, Title: "old backup"}, "ops/backup-monitor", "")
	if err != nil {
		t.Fatal(err)
	}
	_ = f.st.MoveCase(ctx, id, cases.HandedOver, cases.ByDetector, "", "ops/backup-monitor", "")
	if _, err := f.st.SyncPeople(ctx, []store.Person{{Email: "ada@example.org", Name: "Ada", IDs: map[string]string{"slack": "U0ADA"}}}, "directory:file"); err != nil {
		t.Fatal(err)
	}
	ck := f.signin(f.vic)
	_, page := f.do("GET", "/ledger", ck, nil, nil)
	for _, want := range []string{"<h1>Ledger</h1>", "person.added", "case.handed_over", `href="/ledger?ref=` + id + `"`, `href="/cases/` + id + `"`, "Export this view (JSON)", `href="/ledger"`} {
		if !strings.Contains(page, want) {
			t.Errorf("the explorer lacks %q", want)
		}
	}
	_, page = f.do("GET", "/ledger?ref="+id, ck, nil, nil)
	if strings.Contains(page, "person.added") || !strings.Contains(page, "case.handed_over") || !strings.Contains(page, "following = <strong>"+id+"</strong>") {
		t.Error("following the case does not keep only its rows, or does not say so")
	}
	_, page = f.do("GET", "/ledger?kind=person.added", ck, nil, nil)
	if !strings.Contains(page, "ada@example.org") || strings.Contains(page, ">case.handed_over<") {
		t.Error("the kind filter does not keep only that kind")
	}
	resp, body := f.do("GET", "/ledger.json?kind=person.added", ck, nil, nil)
	if !strings.Contains(resp.Header.Get("Content-Type"), "application/json") {
		t.Fatalf("export: %s", resp.Header.Get("Content-Type"))
	}
	var out struct {
		Total int
		Rows  []store.ExploreRow
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil || out.Total != 1 || out.Rows[0].Subject != "ada@example.org" {
		t.Fatalf("export: %v %+v", err, out)
	}
}

// Each page where a flow ends links to its records in the explorer: a case, a run, a routine, a fleet, a host, an action,
// a user, and History.
func TestLedgerEntryPoints(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	k := store.Key{Kind: "target", Value: "failed_backup/host:backup-01"}
	id, _, err := f.st.OpenCase(ctx, store.NewCase{Kind: "failed_backup", Target: "host:backup-01", Keys: []store.Key{k}, Canonical: k, Title: "old backup"}, "ops/backup-monitor", "")
	if err != nil {
		t.Fatal(err)
	}
	ck := f.signin(f.admin)
	for path, want := range map[string]string{
		"/cases/" + id: `href="/ledger?ref=` + id + `"`,
		"/autonomy":    `href="/ledger?kind=control.autonomy"`,
		"/users":       `href="/ledger?ref=user%3A`,
		"/history":     `href="/ledger"`,
	} {
		if _, page := f.do("GET", path, ck, nil, nil); !strings.Contains(page, want) {
			t.Errorf("%s lacks %s", path, want)
		}
	}
	for _, tmpl := range []string{"run.html", "routine.html", "fleet.html", "host.html"} {
		b, err := templatesFS.ReadFile("templates/" + tmpl)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), "ledger") {
			t.Errorf("%s has no link into the ledger", tmpl)
		}
	}
}
