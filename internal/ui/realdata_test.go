package ui

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chipi/flotilla/internal/api"
	"github.com/chipi/flotilla/internal/registry"
	"github.com/chipi/flotilla/internal/sched"
	"github.com/chipi/flotilla/internal/spec"
	"github.com/chipi/flotilla/internal/store"
	"github.com/chipi/flotilla/internal/worker"
)

// TestRealDataPages renders every page from a copy of an installation's store and its fleets, for a review of how the
// pages look with real data (the operator, 2026-11-23). It never runs in CI: it needs UI_REALDATA_DB (a store file, which
// is copied before it is opened), UI_REALDATA_FLEETS (the installation's fleets directory) and UI_SNAPSHOT_DIR.
func TestRealDataPages(t *testing.T) {
	db, fleets, dir := os.Getenv("UI_REALDATA_DB"), os.Getenv("UI_REALDATA_FLEETS"), os.Getenv("UI_SNAPSHOT_DIR")
	if db == "" || fleets == "" || dir == "" {
		t.Skip("UI_REALDATA_DB, UI_REALDATA_FLEETS and UI_SNAPSHOT_DIR not set")
	}
	cp := filepath.Join(t.TempDir(), "real.db")
	b, err := os.ReadFile(db)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cp, b, 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(cp)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	routines, err := spec.Load(fleets)
	if err != nil {
		t.Fatal(err)
	}
	reg, err := registry.Load(filepath.Join(fleets, "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	s := &sched.Scheduler{Store: st, Workers: map[string]sched.Runner{}}
	s.SetRoutines(routines)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := &api.Server{Sched: s, Store: st, Workers: map[string]*worker.Client{}, Version: "review", Log: log, Registry: reg}
	u := &Server{API: a, Host: host, Log: log}
	ts := httptest.NewServer(u.Handler())
	t.Cleanup(ts.Close)
	f := &fixture{t: t, ts: ts, st: st, sch: s, ui: u}
	ctx := context.Background()
	tok := "flt_review-token-for-the-copy"
	if err := st.AddUser(ctx, "review", store.UserAdmin, api.HashToken(tok), "operator", "review", true); err != nil {
		t.Fatal(err)
	}
	ck := f.signin(tok)

	pages := map[string]string{"now": "/", "approvals": "/approvals", "cases": "/cases", "runs": "/runs", "runs-problems": "/runs?f=problems",
		"fleets": "/fleets", "hosts": "/hosts", "autonomy": "/autonomy", "models": "/models", "models-spend": "/models?tab=spend&period=30d",
		"knowledge": "/knowledge", "history": "/history", "history-ledger": "/history?type=ledger", "brief": "/brief", "brief-archive": "/brief/archive",
		"account": "/account", "users": "/users", "examples": "/examples"}
	for _, r := range routines {
		pages["fleet-"+r.Fleet] = "/fleets/" + r.Fleet
		pages["fleet-"+r.Fleet+"-stats"] = "/fleets/" + r.Fleet + "?tab=stats"
		pages["routine-"+r.Fleet+"-"+r.Routine] = "/fleets/" + r.Fleet + "/" + r.Routine
	}
	cs, err := st.Cases(ctx, store.CaseFilter{Limit: 400})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, c := range cs {
		key := c.Kind + "-" + c.Status
		if !seen[key] {
			seen[key] = true
			pages["case-"+key] = "/cases/" + c.ID
		}
	}
	runs, err := st.Runs(ctx, "", 200)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range runs {
		key := "run-" + filepath.Base(r.Routine) + "-" + r.Status
		if _, ok := pages[key]; !ok {
			pages[key] = "/runs/" + r.ID
		}
	}
	hosts, err := st.Hosts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hosts {
		pages["host-"+h.Name] = "/hosts/" + h.Name
		pages["host-"+h.Name+"-exec"] = "/hosts/" + h.Name + "?tab=exec"
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, path := range pages {
		resp, body := f.do("GET", path, ck, nil, nil)
		if resp.StatusCode != 200 {
			t.Errorf("%s %s: %d", name, path, resp.StatusCode)
			continue
		}
		body = strings.NewReplacer(`href="/static/flotilla.css"`, `href="flotilla.css"`, `href="/static/favicon.svg"`, `href="favicon.svg"`,
			`src="/static/flotilla.js"`, `src="flotilla.js"`).Replace(body)
		if err := os.WriteFile(filepath.Join(dir, name+".html"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(dir, "fonts"), 0o700); err != nil {
		t.Fatal(err)
	}
	fonts, _ := Static.ReadDir("static/fonts")
	for _, e := range fonts {
		b, _ := Static.ReadFile("static/fonts/" + e.Name())
		_ = os.WriteFile(filepath.Join(dir, "fonts", e.Name()), b, 0o600)
	}
	for _, name := range []string{"favicon.svg", "flotilla.css", "flotilla.js"} {
		b, _ := Static.ReadFile("static/" + name)
		_ = os.WriteFile(filepath.Join(dir, name), b, 0o600)
	}
	t.Logf("%d pages written to %s", len(pages), dir)
}
