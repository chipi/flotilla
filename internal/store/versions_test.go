package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/chipi/flotilla/internal/cases"
	"github.com/chipi/flotilla/internal/spec"
)

const rt = "triage/cycle"

func lv(n int, h string) LoadedVersion {
	return LoadedVersion{Version: n, Hash: h, Manifest: map[string]string{"spec.yaml": h}}
}

func states(t *testing.T, s *Store, routine string) string {
	t.Helper()
	vs, err := s.Versions(context.Background(), routine)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, v := range vs {
		out = append(out, strings.Join([]string{"v", string(rune('0' + v.Version)), "=", v.State}, ""))
	}
	return strings.Join(out, " ")
}

func promote(s *Store, n int, hash string) PromoteArgs {
	return PromoteArgs{Routine: rt, Version: n, Hash: hash, Autonomy: "record", Current: "record", Actor: "user:ada", Via: "cli", Reason: "no pairs yet; read the diff"}
}

func lastVersionEvent(t *testing.T, s *Store, kind, subject string) *Event {
	t.Helper()
	e, err := s.LatestEvent(context.Background(), kind, subject)
	if err != nil || e == nil {
		t.Fatalf("no %s event on %s (%v)", kind, subject, err)
	}
	return e
}

func TestEnsureVersionsFirstLiveThenShadows(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	res, err := s.EnsureVersions(ctx, rt, []LoadedVersion{lv(2, "h2"), lv(1, "h1"), lv(3, "h3")}, "flotillad")
	if err != nil || res.Live != 1 || len(res.Shadows) != 2 {
		t.Fatalf("first deploy: %+v %v", res, err)
	}
	if got := states(t, s, rt); got != "v1=live v2=shadow v3=shadow" {
		t.Fatalf("states %s", got)
	}
	v1, ok, _ := s.LiveVersion(ctx, rt)
	if !ok || v1.Version != 1 || v1.Hash != "h1" || v1.LiveAt == "" || v1.StateBy != "flotillad" || v1.Manifest["spec.yaml"] != "h1" {
		t.Fatalf("live %+v", v1)
	}
	if e := lastVersionEvent(t, s, "version.deploy", rt+"@v1"); e.Actor != "flotillad" || !strings.HasPrefix(e.Detail, "live") {
		t.Fatalf("event %+v", e)
	}
	// again: nothing changes, nothing is written
	n0, _ := s.Events(ctx, 1000)
	res, err = s.EnsureVersions(ctx, rt, []LoadedVersion{lv(1, "h1"), lv(2, "h2"), lv(3, "h3")}, "flotillad")
	n1, _ := s.Events(ctx, 1000)
	if err != nil || res.Live != 0 || len(res.Shadows) != 0 || len(res.Updated) != 0 || len(n1) != len(n0) {
		t.Fatalf("a second start: %+v %v events %d -> %d", res, err, len(n0), len(n1))
	}
	// a new number arrives as a shadow, even when it is lower than the live one's successors; a shadow's
	// hash follows its folder; the live version's does not move here
	res, err = s.EnsureVersions(ctx, rt, []LoadedVersion{lv(1, "CHANGED"), lv(2, "h2b"), lv(3, "h3"), lv(4, "h4")}, "flotillad")
	if err != nil || len(res.Shadows) != 1 || res.Shadows[0] != 4 || len(res.Updated) != 1 || res.Updated[0] != 2 {
		t.Fatalf("a later deploy: %+v %v", res, err)
	}
	vs, _ := s.Versions(ctx, rt)
	if vs[0].Hash != "h1" || vs[1].Hash != "h2b" || vs[3].State != VersionShadow {
		t.Fatalf("versions %+v", vs)
	}
	if e := lastVersionEvent(t, s, "version.update", rt+"@v2"); !strings.Contains(e.Detail, "h2 -> h2b") {
		t.Fatalf("event %+v", e)
	}
}

func seed(t *testing.T, s *Store, n int) {
	t.Helper()
	var l []LoadedVersion
	for i := 1; i <= n; i++ {
		l = append(l, lv(i, "h"+string(rune('0'+i))))
	}
	if _, err := s.EnsureVersions(context.Background(), rt, l, "flotillad"); err != nil {
		t.Fatal(err)
	}
}

func TestPromote(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	seed(t, s, 3)
	if err := s.Promote(ctx, promote(s, 2, "h2")); err != nil {
		t.Fatal(err)
	}
	if got := states(t, s, rt); got != "v1=previous v2=live v3=shadow" {
		t.Fatalf("states %s", got)
	}
	v2, _, _ := s.LiveVersion(ctx, rt)
	if v2.Version != 2 || v2.StateBy != "user:ada" || v2.StateWhy != "no pairs yet; read the diff" || v2.AutonomyAtPromote != "record" || v2.LiveAt == "" {
		t.Fatalf("live %+v", v2)
	}
	if v, _ := s.Control(ctx, rt, "autonomy"); v != "record" {
		t.Fatalf("the autonomy control is set at promote, not inherited: %q", v)
	}
	if e := lastVersionEvent(t, s, "version.promote", rt+"@v2"); e.Actor != "user:ada" || e.Via != "cli" || !strings.Contains(e.Detail, "missing pairs") || !strings.Contains(e.Detail, "v1 is previous") {
		t.Fatalf("event %+v", e)
	}
	if e := lastVersionEvent(t, s, "control.autonomy", rt); e.Actor != "user:ada" || !strings.HasPrefix(e.Detail, "record: ") {
		t.Fatalf("autonomy event %+v", e)
	}
	// promote v3: v2 becomes previous, the old previous (v1) is retired
	a := promote(s, 3, "h3")
	a.Autonomy = "propose"
	if err := s.Promote(ctx, a); err != nil {
		t.Fatal(err)
	}
	if got := states(t, s, rt); got != "v1=retired v2=previous v3=live" {
		t.Fatalf("states %s", got)
	}
	vs, _ := s.Versions(ctx, rt)
	if vs[0].RetiredKind != RetiredPlain || !strings.Contains(vs[0].StateWhy, "superseded by v3") {
		t.Fatalf("v1 %+v", vs[0])
	}
	if e := lastVersionEvent(t, s, "version.retire", rt+"@v1"); !strings.Contains(e.Detail, "superseded by v3") {
		t.Fatalf("event %+v", e)
	}
}

func TestPromoteRefusals(t *testing.T) {
	ctx := context.Background()
	for name, c := range map[string]struct {
		edit func(a *PromoteArgs)
		want error
		msg  string
	}{
		"not a shadow":                  {func(a *PromoteArgs) { a.Version, a.Hash = 1, "h1" }, ErrVersionConflict, "is live, not a shadow"},
		"no such version":               {func(a *PromoteArgs) { a.Version = 9 }, ErrNoSuchVersion, "v9"},
		"a changed shadow":              {func(a *PromoteArgs) { a.Hash = "other" }, ErrVersionConflict, "changed since it was recorded"},
		"no reason without pairs":       {func(a *PromoteArgs) { a.Reason = " " }, ErrVersionInvalid, "lacks the evidence its acceptance names (pairs)"},
		"no autonomy":                   {func(a *PromoteArgs) { a.Autonomy = "" }, ErrVersionInvalid, "choose record, propose or auto"},
		"an unknown autonomy":           {func(a *PromoteArgs) { a.Autonomy = "shadow" }, ErrVersionInvalid, "choose record, propose or auto"},
		"no actor":                      {func(a *PromoteArgs) { a.Actor = "" }, ErrVersionInvalid, "who promotes"},
		"a raise needs its own reason":  {func(a *PromoteArgs) { a.Autonomy, a.Reason = "auto", "" }, ErrVersionInvalid, "needs a reason"},
		"a raise with a reason is fine": {func(a *PromoteArgs) { a.Autonomy = "auto" }, nil, ""},
	} {
		t.Run(name, func(t *testing.T) {
			s, _ := open(t)
			seed(t, s, 2)
			a := promote(s, 2, "h2")
			c.edit(&a)
			err := s.Promote(ctx, a)
			if c.want == nil {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if !errors.Is(err, c.want) || !strings.Contains(err.Error(), c.msg) {
				t.Fatalf("want %v with %q, got %v", c.want, c.msg, err)
			}
			if got := states(t, s, rt); got != "v1=live v2=shadow" {
				t.Fatalf("a refused promote changed states: %s", got)
			}
			if v, _ := s.Control(ctx, rt, "autonomy"); v != "" {
				t.Fatalf("a refused promote set the autonomy: %q", v)
			}
		})
	}
}

func addPlanAwaiting(t *testing.T, s *Store, run, key string) string {
	t.Helper()
	id := plannedOn(t, s, key)
	p := rec(id)
	p.RunID, p.Outcome, p.Autonomy = run, "propose", "propose"
	if _, err := s.ApplyPlan(context.Background(), p, cases.AwaitingApproval, "", "host:backup-01"+key, true); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestRollback(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	seed(t, s, 2)
	if err := s.Promote(ctx, promote(s, 2, "h2")); err != nil {
		t.Fatal(err)
	}
	// v2 has a run in flight and a plan awaiting approval; v1's plan stays approvable
	for _, r := range []Run{{ID: "run-v2", Routine: rt, Version: 2, SpecHash: "x", Trigger: "case", Actor: "flotillad", Autonomy: "propose", StartedAt: "2026-11-18T10:00:00Z", Status: StatusRunning},
		{ID: "run-v1", Routine: rt, Version: 1, SpecHash: "x", Trigger: "case", Actor: "flotillad", Autonomy: "propose", StartedAt: "2026-11-18T09:00:00Z", Status: StatusOK}} {
		if err := s.StartRun(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	c2 := addPlanAwaiting(t, s, "run-v2", "-a")
	c1 := addPlanAwaiting(t, s, "run-v1", "-b")

	a := RollbackArgs{Routine: rt, Hash: "h1", Autonomy: "record", Current: "record", Actor: "user:ada", Via: "ui", Reason: "v2 files too much"}
	bad := a
	bad.Hash = "moved"
	if _, err := s.Rollback(ctx, bad); !errors.Is(err, ErrVersionConflict) || !strings.Contains(err.Error(), "changed since it was live") {
		t.Fatalf("a changed previous: %v", err)
	}
	bad = a
	bad.Reason = ""
	if _, err := s.Rollback(ctx, bad); !errors.Is(err, ErrVersionInvalid) {
		t.Fatalf("no reason: %v", err)
	}
	for key, id := range map[string]string{"-a": c2, "-b": c1} {
		if h, _ := s.LockHolder(ctx, "host:backup-01"+key); h != id {
			t.Fatalf("before the rollback %s holds its target: %q", id, h)
		}
	}
	res, err := s.Rollback(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	if res.RolledBack != 2 || res.Restored != 1 || len(res.Cases) != 1 || res.Cases[0] != c2 || len(res.RunsInFlight) != 1 || res.RunsInFlight[0] != "run-v2" {
		t.Fatalf("result %+v (c1 %s c2 %s)", res, c1, c2)
	}
	if got := states(t, s, rt); got != "v1=live v2=retired" {
		t.Fatalf("states %s", got)
	}
	// the rolled-back plan's case went back to the acting routine: handed over, its target lock released, one
	// case.rolled_back event; the case of v1's plan is untouched
	cs2, _ := s.Case(ctx, c2)
	cs1, _ := s.Case(ctx, c1)
	if cs2.Status != cases.HandedOver || cs1.Status != cases.AwaitingApproval {
		t.Fatalf("cases: v2's is %s, v1's is %s", cs2.Status, cs1.Status)
	}
	evs, _ := s.CaseEvents(ctx, c2)
	if last := evs[len(evs)-1]; last.Kind != "case.rolled_back" || last.Actor != "user:ada" || last.By != cases.ByOperator ||
		last.From != cases.AwaitingApproval || last.To != cases.HandedOver || last.Version != 0 || !strings.Contains(last.Detail, "v2 was rolled back") {
		t.Fatalf("last event of the rolled-back case: %+v", last)
	}
	if h, _ := s.LockHolder(ctx, "host:backup-01-a"); h != "" {
		t.Fatalf("the rolled-back case still holds its target: %q", h)
	}
	if h, _ := s.LockHolder(ctx, "host:backup-01-b"); h != c1 {
		t.Fatalf("v1's case lost its target lock: %q", h)
	}
	vs, _ := s.Versions(ctx, rt)
	if vs[1].RetiredKind != RetiredRolledBack || vs[1].StateBy != "user:ada" || vs[1].StateWhy != "v2 files too much" || vs[0].LiveAt == "" || vs[0].Hash != "h1" {
		t.Fatalf("versions %+v", vs)
	}
	p2, _ := s.Plans(ctx, c2)
	p1, _ := s.Plans(ctx, c1)
	if p2[0].RolledBackAt == "" || p2[0].RolledBackBy != "user:ada" || p2[0].Version != 2 || p1[0].RolledBackAt != "" || p1[0].Version != 1 {
		t.Fatalf("plans: v2's %+v, v1's %+v", p2[0], p1[0])
	}
	if e := lastVersionEvent(t, s, "version.rollback", rt+"@v2"); e.Actor != "user:ada" || e.Via != "ui" || !strings.Contains(e.Detail, "v1 is live again") || !strings.Contains(e.Detail, "1 plan(s)") {
		t.Fatalf("event %+v", e)
	}
	// there is no previous until the next promote
	if _, err := s.Rollback(ctx, a); !errors.Is(err, ErrVersionConflict) || !strings.Contains(err.Error(), "no previous") {
		t.Fatalf("second rollback: %v", err)
	}
}

func TestRetire(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	seed(t, s, 3)
	if err := s.Promote(ctx, promote(s, 2, "h2")); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		n    int
		want error
		msg  string
	}{
		"live":     {2, ErrVersionConflict, "roll back or promote instead"},
		"previous": {1, ErrVersionConflict, "roll back or promote instead"},
		"unknown":  {7, ErrNoSuchVersion, "v7"},
	} {
		if err := s.Retire(ctx, rt, c.n, "user:ada", "cli", "x"); !errors.Is(err, c.want) || !strings.Contains(err.Error(), c.msg) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := s.Retire(ctx, rt, 3, "user:ada", "cli", ""); !errors.Is(err, ErrVersionInvalid) {
		t.Fatalf("no reason: %v", err)
	}
	if err := s.Retire(ctx, rt, 3, "user:ada", "cli", "not needed"); err != nil {
		t.Fatal(err)
	}
	if got := states(t, s, rt); got != "v1=previous v2=live v3=retired" {
		t.Fatalf("states %s", got)
	}
	if err := s.Retire(ctx, rt, 3, "user:ada", "cli", "again"); !errors.Is(err, ErrVersionConflict) || !strings.Contains(err.Error(), "already retired") {
		t.Fatalf("retire twice: %v", err)
	}
	vs, _ := s.Versions(ctx, rt)
	if vs[2].RetiredKind != RetiredPlain || vs[2].StateWhy != "not needed" {
		t.Fatalf("v3 %+v", vs[2])
	}
	if e := lastVersionEvent(t, s, "version.retire", rt+"@v3"); e.Detail != "not needed" || e.Actor != "user:ada" {
		t.Fatalf("event %+v", e)
	}
}

// One live and one previous version per routine is the schema's, not only the code's.
func TestOneLiveAndOnePreviousPerRoutine(t *testing.T) {
	s, _ := open(t)
	seed(t, s, 1)
	ins := func(n int, state string) error {
		_, err := s.db.Exec(`INSERT INTO routine_versions (routine, version, hash, state, deployed_at) VALUES (?, ?, 'h', ?, 't')`, rt, n, state)
		return err
	}
	if err := ins(2, "live"); err == nil || !strings.Contains(err.Error(), "UNIQUE") {
		t.Fatalf("a second live version: %v", err)
	}
	if err := ins(2, "previous"); err != nil {
		t.Fatal(err)
	}
	if err := ins(3, "previous"); err == nil || !strings.Contains(err.Error(), "UNIQUE") {
		t.Fatalf("a second previous version: %v", err)
	}
	if err := ins(3, "shadow"); err != nil {
		t.Fatalf("any number of shadows: %v", err)
	}
	if err := ins(4, "weird"); err == nil {
		t.Fatal("an unknown state")
	}
	if err := ins(5, "live"); err == nil {
		t.Fatal("a second live version")
	}
	other := func() error {
		_, err := s.db.Exec(`INSERT INTO routine_versions (routine, version, hash, state, deployed_at) VALUES ('ops/other', 1, 'h', 'live', 't')`)
		return err
	}
	if err := other(); err != nil {
		t.Fatalf("another routine has its own live version: %v", err)
	}
}

// Every state change is one transaction with its events row: when the last statement fails, nothing stays.
func TestStateChangesAreAtomic(t *testing.T) {
	ctx := context.Background()
	for name, c := range map[string]struct {
		kind string
		run  func(s *Store) error
	}{
		"promote": {"control.autonomy", func(s *Store) error { return s.Promote(ctx, promote(s, 2, "h2")) }},
		"retire":  {"version.retire", func(s *Store) error { return s.Retire(ctx, rt, 2, "user:ada", "cli", "x") }},
		"rollback": {"control.autonomy", func(s *Store) error {
			if err := s.Promote(ctx, promote(s, 2, "h2")); err != nil {
				return err
			}
			if _, err := s.db.Exec(`CREATE TRIGGER fail_rollback BEFORE INSERT ON events WHEN NEW.kind = 'control.autonomy' BEGIN SELECT RAISE(ABORT, 'injected'); END`); err != nil {
				return err
			}
			_, err := s.Rollback(ctx, RollbackArgs{Routine: rt, Hash: "h1", Autonomy: "record", Current: "record", Actor: "user:ada", Via: "cli", Reason: "r"})
			return err
		}},
	} {
		t.Run(name, func(t *testing.T) {
			s, _ := open(t)
			seed(t, s, 2)
			if name != "rollback" {
				if _, err := s.db.Exec(`CREATE TRIGGER fail_it BEFORE INSERT ON events WHEN NEW.kind = '` + c.kind + `' BEGIN SELECT RAISE(ABORT, 'injected'); END`); err != nil {
					t.Fatal(err)
				}
			}
			want := "v1=live v2=shadow"
			if name == "rollback" {
				want = "v1=previous v2=live"
			}
			if err := c.run(s); err == nil || !strings.Contains(err.Error(), "injected") {
				t.Fatalf("want the injected failure, got %v", err)
			}
			if got := states(t, s, rt); got != want {
				t.Fatalf("a failed %s left states %q, want %q", name, got, want)
			}
			if name == "promote" {
				if v, _ := s.Control(ctx, rt, "autonomy"); v != "" {
					t.Fatalf("the control was set: %q", v)
				}
			}
			vs, _ := s.Versions(ctx, rt)
			for _, v := range vs {
				if v.RetiredKind != "" {
					t.Fatalf("a failed %s left a retirement: %+v", name, v)
				}
			}
		})
	}
}

// The version of a run is on its calls, what it loaded, its plans and its ledger rows; a case event made by
// a routine carries that routine's live version, one made by a person or policy carries 0.
func TestVersionIsCarried(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	seed(t, s, 2)
	if err := s.Promote(ctx, promote(s, 2, "h2")); err != nil {
		t.Fatal(err)
	}
	if err := s.StartRun(ctx, Run{ID: "r2", Routine: rt, Version: 2, SpecHash: "x", Trigger: "schedule", Actor: "flotillad", Autonomy: "record", StartedAt: "2026-11-18T10:00:00Z", Status: StatusRunning}); err != nil {
		t.Fatal(err)
	}
	if err := s.StartRun(ctx, Run{ID: "r0", Routine: "ops/old", SpecHash: "x", Trigger: "schedule", Actor: "flotillad", Autonomy: "record", StartedAt: "2026-11-18T10:00:00Z", Status: StatusRunning}); err != nil {
		t.Fatal(err)
	}
	runs, _ := s.Runs(ctx, "", 10)
	got := map[string]int{}
	for _, r := range runs {
		got[r.ID] = r.Version
	}
	if got["r2"] != 2 || got["r0"] != 1 {
		t.Fatalf("run versions %v (an unset version reads as 1)", got)
	}
	if err := s.RecordCalls(ctx, []ModelCall{{RunID: "r2", Routine: rt, Alias: "a"}, {RunID: "nope", Routine: rt, Alias: "a"}}); err != nil {
		t.Fatal(err)
	}
	calls, _ := s.RunCalls(ctx, []string{"r2", "nope"})
	if calls["r2"][0].Version != 2 || calls["nope"][0].Version != 1 {
		t.Fatalf("calls %+v %+v", calls["r2"], calls["nope"])
	}
	if err := s.RecordLoaded(ctx, []Loaded{{RunID: "r2", Routine: rt, Kind: "playbook", Trust: TrustSource, Title: "t", Source: "s"}}); err != nil {
		t.Fatal(err)
	}
	ld, _ := s.RunLoaded(ctx, []string{"r2"})
	if ld["r2"][0].Version != 2 {
		t.Fatalf("loaded %+v", ld)
	}
	if _, err := s.WriteFiling(ctx, "triage/cycle", map[string]string{"fingerprint": "fp1"}, "r2"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendEntry(ctx, "triage/cycle", "decision", map[string]string{"fingerprint": "fp1", "occurrence_id": "o1"}, "r2"); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"ledger_filings", "ledger_entries"} {
		var v int
		if err := s.db.QueryRow(`SELECT version FROM ` + table + ` WHERE run_id = 'r2'`).Scan(&v); err != nil || v != 2 {
			t.Fatalf("%s version %d %v", table, v, err)
		}
	}
	id := plannedOn(t, s, "-v")
	p := rec(id)
	p.RunID = "r2"
	if _, err := s.ApplyPlan(ctx, p, cases.Executing, "", "host:backup-01-v", true); err != nil {
		t.Fatal(err)
	}
	plans, _ := s.Plans(ctx, id)
	if plans[0].Version != 2 {
		t.Fatalf("plan %+v", plans[0])
	}
	evs, _ := s.CaseEvents(ctx, id)
	by := map[string]int{}
	for _, e := range evs {
		by[e.Actor] = e.Version
	}
	// plannedOn's detector "m" and the policy "flotillad" have no slash: 0. The acting routine "triage/cycle"
	// moved the case to planned: its live version, 2
	if by["m"] != 0 || by["flotillad"] != 0 || by["triage/cycle"] != 2 {
		t.Fatalf("case event versions by actor: %v", by)
	}
}

func TestMigrateFromSchema25AddsAndRenames(t *testing.T) {
	if len(schema) < 28 {
		t.Fatal("steps 26 to 28 are the routine versions' and the rename's")
	}
	p := filepath.Join(t.TempDir(), "s25.db")
	makeSchema25(t, p)
	before := tableCounts(t, p)
	s, err := Open(p)
	if err != nil {
		t.Fatalf("migrating a schema-25 store: %v", err)
	}
	s.Close()
	db, err := sql.Open("sqlite", p)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	q := func(query string) int {
		var n int
		if err := db.QueryRow(query).Scan(&n); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		return n
	}
	want := func(query string, n int) {
		t.Helper()
		if got := q(query); got != n {
			t.Errorf("%s = %d, want %d", query, got, n)
		}
	}
	want(`SELECT MAX(n) FROM schema_version`, len(schema))
	// no row lost or added by the migration, in any table that existed
	after := tableCounts(t, p)
	for table, n := range before {
		if after[table] != n {
			t.Errorf("%s: %d rows before, %d after", table, n, after[table])
		}
	}
	want(`SELECT COUNT(*) FROM routine_versions`, 0) // filled at a normal start, not by the migration
	// the rename: every shadow is now record, in exactly the places listed, and the counts carry over
	want(`SELECT COUNT(*) FROM controls WHERE key = 'autonomy' AND value = 'shadow'`, 0)
	want(`SELECT COUNT(*) FROM controls WHERE key = 'autonomy' AND value = 'record'`, 2)
	want(`SELECT COUNT(*) FROM controls WHERE key = 'autonomy' AND value = 'propose'`, 1)
	want(`SELECT COUNT(*) FROM controls WHERE key = 'autonomy' AND value = 'auto'`, 1)
	want(`SELECT COUNT(*) FROM controls WHERE key = 'note' AND value = 'shadow'`, 1) // not an autonomy: untouched
	want(`SELECT COUNT(*) FROM runs WHERE autonomy = 'shadow'`, 0)
	want(`SELECT COUNT(*) FROM runs WHERE autonomy = 'record'`, 3)
	want(`SELECT COUNT(*) FROM runs WHERE autonomy = 'propose'`, 2)
	want(`SELECT COUNT(*) FROM runs WHERE autonomy = 'auto'`, 1)
	want(`SELECT COUNT(*) FROM plans WHERE autonomy = 'shadow' OR outcome = 'shadow'`, 0)
	want(`SELECT COUNT(*) FROM plans WHERE autonomy = 'record' AND outcome = 'record' AND would = 'propose'`, 2)
	want(`SELECT COUNT(*) FROM plans WHERE autonomy = 'propose' AND outcome = 'propose'`, 1)
	want(`SELECT COUNT(*) FROM cases WHERE resolution = 'shadow_plan_recorded'`, 0)
	want(`SELECT COUNT(*) FROM cases WHERE resolution = 'plan_recorded'`, 2)
	want(`SELECT COUNT(*) FROM cases WHERE resolution = 'verified'`, 1)
	want(`SELECT COUNT(*) FROM hosts WHERE ceiling = 'shadow'`, 0)
	want(`SELECT COUNT(*) FROM hosts WHERE ceiling = 'record'`, 1)
	want(`SELECT COUNT(*) FROM hosts WHERE ceiling = 'propose'`, 1)
	// history is not rewritten
	want(`SELECT COUNT(*) FROM events WHERE detail LIKE '%shadow%'`, 2)
	want(`SELECT COUNT(*) FROM case_events WHERE detail LIKE '%shadow%'`, 2)
	// the version columns: 1 for what came before versions, 0 for an event no routine made
	for _, table := range []string{"runs", "run_calls", "run_loaded", "plans", "ledger_filings", "ledger_entries"} {
		want(`SELECT COUNT(*) FROM `+table+` WHERE version = 1`, before[table])
	}
	want(`SELECT COUNT(*) FROM case_events WHERE actor LIKE '%/%' AND version = 1`, 2)
	want(`SELECT COUNT(*) FROM case_events WHERE actor NOT LIKE '%/%' AND version = 0`, 3)
	want(`SELECT COUNT(*) FROM plans WHERE rolled_back_at IS NOT NULL OR rolled_back_by IS NOT NULL`, 0)
	want(`SELECT COUNT(*) FROM pragma_index_list('routine_versions') WHERE "unique" = 1 AND partial = 1`, 2)
	var integrity string
	if err := db.QueryRow(`PRAGMA integrity_check`).Scan(&integrity); err != nil || integrity != "ok" {
		t.Errorf("integrity_check: %q %v", integrity, err)
	}
	// and a second start changes nothing
	s2, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	s2.Close()
	if again := tableCounts(t, p); len(again) != len(after) {
		t.Fatal("a second open added tables")
	} else {
		for k, v := range after {
			if again[k] != v {
				t.Errorf("a second open changed %s: %d -> %d", k, v, again[k])
			}
		}
	}
}

// makeSchema25 builds a store as the last released flotillad left it: steps 1 to 25, with rows holding the
// word shadow in every place the rename moves, and in places it must not touch.
func makeSchema25(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE schema_version (n INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 25; i++ {
		if _, err := db.Exec(schema[i]); err != nil {
			t.Fatalf("step %d: %v", i+1, err)
		}
		if _, err := db.Exec(`INSERT INTO schema_version (n) VALUES (?)`, i+1); err != nil {
			t.Fatal(err)
		}
	}
	const T = "2026-11-17T00:00:00Z"
	for _, q := range []string{
		`INSERT INTO controls (subject, key, value, actor, at) VALUES
		  ('triage/cycle', 'autonomy', 'shadow', 'operator', '` + T + `'),
		  ('action:backup.rerun', 'autonomy', 'shadow', 'operator', '` + T + `'),
		  ('ops/backup-monitor', 'autonomy', 'propose', 'operator', '` + T + `'),
		  ('platform/heartbeat', 'autonomy', 'auto', 'operator', '` + T + `'),
		  ('triage', 'note', 'shadow', 'operator', '` + T + `')`,
		`INSERT INTO runs (id, routine, spec_hash, trigger, actor, autonomy, started_at, status, detail) VALUES
		  ('r1', 'triage/cycle', 'h', 'schedule', 'flotillad', 'shadow', '` + T + `', 'ok', 'ran at shadow'),
		  ('r2', 'triage/cycle', 'h', 'schedule', 'flotillad', 'shadow', '` + T + `', 'ok', ''),
		  ('r3', 'ops/backup-monitor', 'h', 'schedule', 'flotillad', 'shadow', '` + T + `', 'ok', ''),
		  ('r4', 'ops/rerun-backup', 'h', 'case', 'flotillad', 'propose', '` + T + `', 'ok', ''),
		  ('r5', 'ops/rerun-backup', 'h', 'case', 'flotillad', 'propose', '` + T + `', 'ok', ''),
		  ('r6', 'platform/heartbeat', 'h', 'schedule', 'flotillad', 'auto', '` + T + `', 'ok', '')`,
		`INSERT INTO cases (id, kind, target, canonical_key, status, resolution, opened_at, status_at) VALUES
		  ('c1', 'failed_backup', 'host:a', 'k1', 'closed', 'shadow_plan_recorded', '` + T + `', '` + T + `'),
		  ('c2', 'failed_backup', 'host:b', 'k2', 'closed', 'shadow_plan_recorded', '` + T + `', '` + T + `'),
		  ('c3', 'failed_backup', 'host:c', 'k3', 'closed', 'verified', '` + T + `', '` + T + `')`,
		`INSERT INTO plans (case_id, run_id, action, params, plan_hash, outcome, autonomy, would, reasons, decided_at) VALUES
		  ('c1', 'r4', 'backup.rerun', '{}', 'h', 'shadow', 'shadow', 'propose', '[]', '` + T + `'),
		  ('c2', 'r4', 'backup.rerun', '{}', 'h', 'shadow', 'shadow', 'propose', '[]', '` + T + `'),
		  ('c3', 'r5', 'backup.rerun', '{}', 'h', 'propose', 'propose', '', '[]', '` + T + `')`,
		`INSERT INTO case_events (case_id, at, kind, actor, detail) VALUES
		  ('c1', '` + T + `', 'case.detected', 'ops/backup-monitor', 'found'),
		  ('c1', '` + T + `', 'case.planned', 'ops/rerun-backup', 'planned'),
		  ('c1', '` + T + `', 'case.closed', 'flotillad', 'plan 1 backup.rerun: shadow at shadow, would propose'),
		  ('c2', '` + T + `', 'case.closed', 'flotillad', 'plan 2 backup.rerun: shadow at shadow, would propose'),
		  ('c3', '` + T + `', 'case.verified', 'ada', 'looks right')`,
		`INSERT INTO events (at, actor, via, kind, subject, detail) VALUES
		  ('` + T + `', 'operator', 'cli', 'control.autonomy', 'triage/cycle', 'shadow'),
		  ('` + T + `', 'operator', 'cli', 'control.autonomy', 'action:backup.rerun', 'shadow: the new action starts at shadow'),
		  ('` + T + `', 'operator', 'cli', 'control.stop', 'triage', '1')`,
		`INSERT INTO hosts (name, created_at, ceiling) VALUES ('host-a', '` + T + `', 'shadow'), ('host-b', '` + T + `', 'propose')`,
		`INSERT INTO run_calls (run_id, routine, at, alias) VALUES ('r1', 'triage/cycle', '` + T + `', 'a'), ('r2', 'triage/cycle', '` + T + `', 'a')`,
		`INSERT INTO run_loaded (run_id, routine, at, kind, trust, title, source) VALUES ('r1', 'triage/cycle', '` + T + `', 'playbook', 'source', 't', 's')`,
		`INSERT INTO ledger_filings (namespace, fingerprint, row, run_id, written_at) VALUES ('triage/cycle', 'fp', '{}', 'r1', '` + T + `')`,
		`INSERT INTO ledger_entries (namespace, kind, row, run_id, written_at) VALUES ('triage/cycle', 'decision', '{}', 'r1', '` + T + `'), ('triage/cycle', 'decision', '{}', 'r2', '` + T + `')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q[:40], err)
		}
	}
}

func tableCounts(t *testing.T, path string) map[string]int {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' AND name NOT IN ('routine_versions', 'schema_version')`)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		names = append(names, n)
	}
	rows.Close()
	out := map[string]int{}
	for _, n := range names {
		var c int
		if err := db.QueryRow(`SELECT COUNT(*) FROM ` + n).Scan(&c); err != nil {
			t.Fatal(err)
		}
		out[n] = c
	}
	return out
}

func TestPromoteAcceptanceAndEvidence(t *testing.T) {
	ctx := context.Background()
	pass := Gate{Name: "replay-corpus", Result: "pass", Ref: "ci/1234"}
	fail := Gate{Name: "lint", Result: "fail"}
	for name, c := range map[string]struct {
		edit func(a *PromoteArgs)
		msg  string // "" promotes
	}{
		"pairs by default, none in step 1": {func(a *PromoteArgs) { a.Reason = "" }, "lacks the evidence its acceptance names (pairs)"},
		"replay declared, no gate":         {func(a *PromoteArgs) { a.Acceptance, a.Reason = []string{"replay"}, "" }, "(replay)"},
		"replay declared, a passing gate":  {func(a *PromoteArgs) { a.Acceptance, a.Reason, a.Evidence.Gates = []string{"replay"}, "", []Gate{pass} }, ""},
		"scenario_eval declared, no eval":  {func(a *PromoteArgs) { a.Acceptance, a.Reason = []string{"scenario_eval"}, "" }, "(scenario_eval)"},
		"scenario_eval, a passing eval": {func(a *PromoteArgs) {
			a.Acceptance, a.Reason, a.Evidence.Evals = []string{"scenario_eval"}, "", []EvalRun{{"run-7", "pass"}}
		}, ""},
		"every declared kind is required": {func(a *PromoteArgs) {
			a.Acceptance, a.Reason, a.Evidence.Gates = []string{"replay", "scenario_eval"}, "", []Gate{pass}
		}, "(scenario_eval)"},
		"a failed gate, declared or not": {func(a *PromoteArgs) {
			a.Acceptance, a.Reason, a.Evidence.Gates = []string{"replay"}, "", []Gate{pass, fail}
		}, "failed result (gate lint)"},
		"a failed eval": {func(a *PromoteArgs) {
			a.Acceptance, a.Reason, a.Evidence.Evals = []string{"scenario_eval"}, "", []EvalRun{{"run-8", "fail"}}
		}, "failed result (eval run-8)"},
		"a failed gate with a reason goes on": {func(a *PromoteArgs) { a.Acceptance, a.Evidence.Gates = []string{"replay"}, []Gate{fail} }, ""},
		"a gate is pass or fail":              {func(a *PromoteArgs) { a.Evidence.Gates = []Gate{{Name: "x", Result: "maybe"}} }, "name=pass|fail"},
		"an eval is pass or fail":             {func(a *PromoteArgs) { a.Evidence.Evals = []EvalRun{{RunID: "r", Verdict: ""}} }, "run_id=pass|fail"},
		"an unknown kind":                     {func(a *PromoteArgs) { a.Acceptance = []string{"vibes"} }, "one of pairs, replay, scenario_eval"},
	} {
		t.Run(name, func(t *testing.T) {
			s, _ := open(t)
			seed(t, s, 2)
			a := promote(s, 2, "h2")
			c.edit(&a)
			err := s.Promote(ctx, a)
			if c.msg != "" {
				if !errors.Is(err, ErrVersionInvalid) || !strings.Contains(err.Error(), c.msg) {
					t.Fatalf("want %q, got %v", c.msg, err)
				}
				if got := states(t, s, rt); got != "v1=live v2=shadow" {
					t.Fatalf("a refused promote changed states: %s", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
		})
	}

	// what was recorded: on the row, as JSON, and in the event
	s, _ := open(t)
	seed(t, s, 2)
	a := promote(s, 2, "h2")
	a.Acceptance, a.Reason, a.WithoutPairs = []string{"pairs", "replay"}, "read the diff; the corpus passes", true
	a.Evidence = Evidence{Gates: []Gate{pass}, Evals: []EvalRun{{"run-9", "pass"}}}
	if err := s.Promote(ctx, a); err != nil {
		t.Fatal(err)
	}
	v2, _, _ := s.LiveVersion(ctx, rt)
	p := v2.Promotion
	if p == nil || strings.Join(p.Acceptance, ",") != "pairs,replay" || p.Pairs != 0 || len(p.Gates) != 1 || p.Gates[0] != pass ||
		len(p.Evals) != 1 || p.Evals[0].Verdict != "pass" || strings.Join(p.Missing, ",") != "pairs" || len(p.Failed) != 0 || !p.WithoutPairs {
		t.Fatalf("recorded %+v", p)
	}
	e := lastVersionEvent(t, s, "version.promote", rt+"@v2")
	for _, want := range []string{"acceptance pairs,replay", "gate replay-corpus=pass", "eval run-9=pass", "missing pairs", "without pairs, acknowledged", `"run_id":"run-9"`, ": read the diff"} {
		if !strings.Contains(e.Detail, want) {
			t.Fatalf("event detail lacks %q: %s", want, e.Detail)
		}
	}
	// a version that was never promoted records none; the first, live at deploy, neither
	vs, _ := s.Versions(ctx, rt)
	if vs[0].Promotion != nil {
		t.Fatalf("v1 %+v", vs[0])
	}
}

// A store that has routine_versions but not yet promote_evidence (schema 26 to 28, as a deploy's -check
// finds it before the new image migrates it) reads without that column.
func TestReadVersionsBeforeEvidenceColumn(t *testing.T) {
	s, p := open(t)
	seed(t, s, 2)
	s.Close()
	db, err := sql.Open("sqlite", p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`ALTER TABLE routine_versions DROP COLUMN promote_evidence; DELETE FROM schema_version WHERE n >= 29`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	read, err := ReadVersions(p)
	if err != nil || read.Schema != 28 || len(read.Rows) != 2 || read.Rows[0].Promotion != nil {
		t.Fatalf("%+v %v", read, err)
	}
}

// A flat spec's live v1 is not frozen: EnsureVersions records its new hash and manifest, once, with an event. A live
// folder version (manifest of files) never follows its folder here.
func TestEnsureVersionsFollowsALegacyLiveV1(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	flat := func(h string) LoadedVersion {
		return LoadedVersion{Version: 1, Hash: h, Legacy: true, Manifest: map[string]string{"run": "r1", "prompt-file:playbooks/triage.md": h}}
	}
	if _, err := s.EnsureVersions(ctx, rt, []LoadedVersion{flat("h1")}, "flotillad"); err != nil {
		t.Fatal(err)
	}
	res, err := s.EnsureVersions(ctx, rt, []LoadedVersion{flat("h2")}, "flotillad")
	if err != nil || len(res.Rerecorded) != 1 || res.Rerecorded[0] != 1 {
		t.Fatalf("re-record: %+v %v", res, err)
	}
	v, _, _ := s.LiveVersion(ctx, rt)
	if v.Hash != "h2" || v.State != VersionLive || v.Manifest["prompt-file:playbooks/triage.md"] != "h2" {
		t.Fatalf("row %+v", v)
	}
	e := lastVersionEvent(t, s, "version.update", rt+"@v1")
	if e.Actor != "flotillad" || !strings.Contains(e.Detail, "h1 -> h2") || !strings.Contains(e.Detail, "prompt-file:playbooks/triage.md (changed)") {
		t.Fatalf("event %+v", e)
	}
	n0, _ := s.Events(ctx, 1000)
	if res, err = s.EnsureVersions(ctx, rt, []LoadedVersion{flat("h2")}, "flotillad"); err != nil || len(res.Rerecorded) != 0 {
		t.Fatalf("again: %+v %v", res, err)
	}
	if n1, _ := s.Events(ctx, 1000); len(n1) != len(n0) {
		t.Fatal("an unchanged v1 wrote an event")
	}

	// a folder version, even if the caller marks it Legacy, is not followed: its row names files, not keys
	s2, _ := open(t)
	if _, err := s2.EnsureVersions(ctx, rt, []LoadedVersion{lv(1, "f1")}, "flotillad"); err != nil {
		t.Fatal(err)
	}
	moved := lv(1, "f2")
	moved.Legacy = true
	if res, _ = s2.EnsureVersions(ctx, rt, []LoadedVersion{moved}, "flotillad"); len(res.Rerecorded) != 0 {
		t.Fatalf("a folder version followed: %+v", res)
	}
	if v, _, _ := s2.LiveVersion(ctx, rt); v.Hash != "f1" {
		t.Fatalf("a folder version's hash moved: %+v", v)
	}
}

// Rerecord: live and previous only, a reason, a different hash; one event with the old and new hash and the diff.
func TestRerecord(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	if _, err := s.EnsureVersions(ctx, rt, []LoadedVersion{lv(1, "h1"), lv(2, "h2"), lv(3, "h3")}, "flotillad"); err != nil {
		t.Fatal(err)
	}
	if err := s.Promote(ctx, promote(s, 2, "h2")); err != nil {
		t.Fatal(err)
	}
	re := func(n int, h, why string) (RerecordResult, error) {
		return s.Rerecord(ctx, RerecordArgs{Routine: rt, Version: n, Hash: h, Manifest: map[string]string{"spec.yaml": h, "new.md": "x"}, Actor: "user:ada", Via: "cli", Reason: why})
	}
	for name, c := range map[string]struct {
		n      int
		h, why string
		want   error
	}{
		"no reason": {2, "hx", " ", ErrVersionInvalid},
		"no hash":   {2, "", "r", ErrVersionInvalid},
		"same hash": {2, "h2", "r", ErrVersionConflict},
		"a shadow":  {3, "hx", "r", ErrVersionConflict},
		"no such":   {9, "hx", "r", ErrNoSuchVersion},
	} {
		if _, err := re(c.n, c.h, c.why); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", name, err, c.want)
		}
	}
	if err := s.Retire(ctx, rt, 3, "user:ada", "cli", "x"); err != nil {
		t.Fatal(err)
	}
	if _, err := re(3, "hx", "r"); !errors.Is(err, ErrVersionConflict) || !strings.Contains(err.Error(), "retired") {
		t.Fatalf("a retired version: %v", err)
	}
	if e, _ := s.LatestEvent(ctx, "version.rerecord", rt+"@v2"); e != nil {
		t.Fatalf("a refused re-record wrote an event: %+v", e)
	}
	for n, h := range map[int]string{2: "h2b", 1: "h1b"} { // live, then previous
		res, err := re(n, h, "deliberate")
		if err != nil || res.NewHash != h || res.OldHash != "h"+string(rune('0'+n)) || strings.Join(res.Changed, ",") != "new.md (added),spec.yaml (changed)" {
			t.Fatalf("v%d: %v %+v", n, err, res)
		}
	}
	if got := states(t, s, rt); got != "v1=previous v2=live v3=retired" {
		t.Fatalf("a re-record changes no state: %s", got)
	}
	live, _, _ := s.LiveVersion(ctx, rt)
	if live.Hash != "h2b" || live.Manifest["new.md"] != "x" {
		t.Fatalf("live row %+v", live)
	}
	e := lastVersionEvent(t, s, "version.rerecord", rt+"@v2")
	if e.Actor != "user:ada" || e.Via != "cli" || !strings.Contains(e.Detail, "h2 -> h2b") || !strings.Contains(e.Detail, "new.md (added), spec.yaml (changed)") || !strings.HasSuffix(e.Detail, "deliberate") {
		t.Fatalf("event %+v", e)
	}
}

// The run of a version that was rolled back is found through its row: a plan it files after the rollback is
// withdrawn by the same fact.
func TestRunVersionRolledBack(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	if _, err := s.EnsureVersions(ctx, rt, []LoadedVersion{lv(1, "h1"), lv(2, "h2")}, "flotillad"); err != nil {
		t.Fatal(err)
	}
	if err := s.Promote(ctx, promote(s, 2, "h2")); err != nil {
		t.Fatal(err)
	}
	for _, r := range []Run{{ID: "r1", Version: 2}, {ID: "r0", Version: 1}} {
		r.Routine, r.SpecHash, r.Trigger, r.Actor, r.Autonomy, r.StartedAt, r.Status = rt, "x", "schedule", "flotillad", "record", "2026-11-18T10:00:00Z", StatusRunning
		if err := s.StartRun(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, rb, err := s.RunVersionRolledBack(ctx, "r1"); err != nil || rb {
		t.Fatalf("v2 is live: %v %v", rb, err)
	}
	if _, err := s.Rollback(ctx, RollbackArgs{Routine: rt, Hash: "h1", Autonomy: "record", Current: "record", Actor: "user:ada", Via: "cli", Reason: "bad"}); err != nil {
		t.Fatal(err)
	}
	at, by, rb, err := s.RunVersionRolledBack(ctx, "r1")
	if err != nil || !rb || by != "user:ada" || at == "" {
		t.Fatalf("v2 was rolled back: %q %q %v %v", at, by, rb, err)
	}
	if _, _, rb, _ = s.RunVersionRolledBack(ctx, "r0"); rb {
		t.Fatal("v1 is live again, not rolled back")
	}
	if _, _, rb, _ = s.RunVersionRolledBack(ctx, "nope"); rb {
		t.Fatal("no such run")
	}
}

func startRun(t *testing.T, s *Store, id, routine string, version int, status string) {
	t.Helper()
	if err := s.StartRun(context.Background(), Run{ID: id, Routine: routine, Version: version, SpecHash: "x", Trigger: "case", Actor: "flotillad", Autonomy: "propose",
		StartedAt: "2026-11-18T10:00:00Z", Status: status}); err != nil {
		t.Fatal(err)
	}
}

// What a rollback must leave alone: a case whose newest plan is the live version's (though an older plan of the
// rolled-back version is on it), a case already executing, a plan of another routine's version with the same
// number, and a plan whose version was never the rolled-back one. Only the newest awaiting plan of the
// rolled-back version is withdrawn.
func TestRollbackLeavesWhatIsNotTheRolledBackVersionsPlanAlone(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	seed(t, s, 2)
	if err := s.Promote(ctx, promote(s, 2, "h2")); err != nil {
		t.Fatal(err)
	}
	startRun(t, s, "run-v2", rt, 2, StatusOK)
	startRun(t, s, "run-v1", rt, 1, StatusOK)
	startRun(t, s, "run-other-v2", "ops/other", 2, StatusOK)

	older := addPlanAwaiting(t, s, "run-v2", "-old") // a v2 plan, then v1 plans again: the newest plan is v1's
	for _, to := range []struct{ status, by string }{{cases.Escalated, cases.ByOperator}, {cases.HandedOver, cases.ByOperator}, {cases.Planned, cases.ByAct}} {
		if err := s.MoveCase(ctx, older, to.status, to.by, "", "user:ada", ""); err != nil { // rejected, reassigned, planned again
			t.Fatal(err)
		}
	}
	p := rec(older)
	p.RunID, p.Outcome, p.Autonomy, p.PlanHash = "run-v1", "propose", "propose", "h-newer"
	if _, err := s.ApplyPlan(ctx, p, cases.AwaitingApproval, "", "host:backup-01-old", true); err != nil {
		t.Fatal(err)
	}
	executing := addPlanAwaiting(t, s, "run-v2", "-exec") // a v2 plan already approved
	if err := s.MoveCase(ctx, executing, cases.Executing, cases.ByOperator, "", "user:ada", "approved"); err != nil {
		t.Fatal(err)
	}
	other := addPlanAwaiting(t, s, "run-other-v2", "-other") // another routine's v2
	target := addPlanAwaiting(t, s, "run-v2", "-target")     // the one that is withdrawn

	res, err := s.Rollback(ctx, RollbackArgs{Routine: rt, Hash: "h1", Autonomy: "record", Current: "record", Actor: "user:ada", Via: "api", Reason: "bad"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Cases) != 1 || res.Cases[0] != target {
		t.Fatalf("only %s is withdrawn, got %v", target, res.Cases)
	}
	for name, c := range map[string]struct{ id, want string }{
		"older v2 plan under a newer v1 plan": {older, cases.AwaitingApproval},
		"an executing case":                   {executing, cases.Executing},
		"another routine's v2":                {other, cases.AwaitingApproval},
		"the withdrawn case":                  {target, cases.HandedOver},
	} {
		if cs, _ := s.Case(ctx, c.id); cs.Status != c.want {
			t.Errorf("%s: %s, want %s", name, cs.Status, c.want)
		}
	}
	for _, id := range []string{older, executing, other} {
		for _, pl := range mustPlans(t, s, id) {
			if pl.RolledBackAt != "" {
				t.Errorf("a plan on %s was marked rolled back: %+v", id, pl)
			}
		}
	}
	if h, _ := s.LockHolder(ctx, "host:backup-01-old"); h != older {
		t.Errorf("the older case lost its target lock: %q", h)
	}
}

func mustPlans(t *testing.T, s *Store, id string) []PlanRecord {
	t.Helper()
	p, err := s.Plans(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// makeSchemaUpTo is an empty store as an older binary left it: steps 1 to n applied and recorded.
func makeSchemaUpTo(t *testing.T, path string, n int) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE schema_version (n INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		if _, err := db.Exec(schema[i]); err != nil {
			t.Fatalf("step %d: %v", i+1, err)
		}
		if _, err := db.Exec(`INSERT INTO schema_version (n) VALUES (?)`, i+1); err != nil {
			t.Fatal(err)
		}
	}
}

// Step 29 on its own: a store at schema 28 with recorded versions gains promote_evidence, empty on the rows it had,
// and every row is as it was (the later steps run too, and the final schema is the newest).
func TestMigrateStep29Alone(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s28.db")
	makeSchemaUpTo(t, p, EvidenceSchema-1)
	db, err := sql.Open("sqlite", p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO routine_versions (routine, version, hash, manifest, state, deployed_at, live_at, state_by, state_at, state_why)
		VALUES ('triage/cycle', 1, 'h1', '{"run":"r"}', 'live', 't', 't', 'flotillad', 't', 'first'), ('triage/cycle', 2, 'h2', '{}', 'shadow', 't', '', 'flotillad', 't', '')`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if err := CheckSchema(p); err != nil {
		t.Fatalf("a store one step behind passes the read-only check: %v", err)
	}
	if got, err := ReadVersions(p); err != nil || got.Schema != EvidenceSchema-1 || len(got.Rows) != 2 {
		t.Fatalf("read before: %+v %v", got, err) // the column-less read path
	}
	s, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	vs, err := s.Versions(context.Background(), rt)
	if err != nil || len(vs) != 2 || vs[0].Hash != "h1" || vs[0].Manifest["run"] != "r" || vs[0].Promotion != nil || vs[1].State != VersionShadow {
		t.Fatalf("rows after: %+v %v", vs, err)
	}
	var n int
	if err := s.db.QueryRow(`SELECT MAX(n) FROM schema_version`).Scan(&n); err != nil || n != len(schema) {
		t.Fatalf("schema %d %v", n, err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM routine_versions WHERE promote_evidence = ''`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("the column is empty on the rows it had: %d %v", n, err)
	}
}

// A step that fails part-way changes nothing: the schema version stays and the statements before the failure are
// undone, so the next start tries the same step on the same store.
func TestAFailedMigrationStepLeavesTheSchemaAlone(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.db")
	s, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	was := schema
	t.Cleanup(func() { schema = was })
	schema = append(append([]string{}, was...), `ALTER TABLE runs ADD COLUMN injected_by_test INTEGER; SELECT * FROM no_such_table;`)
	if s, err = Open(p); err == nil {
		s.Close()
		t.Fatal("a failing step must fail Open")
	} else if !strings.Contains(err.Error(), "schema step "+strconv.Itoa(len(was)+1)) {
		t.Fatalf("the error names the step: %v", err)
	}
	schema = was
	db, err := sql.Open("sqlite", p)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT MAX(n) FROM schema_version`).Scan(&n); err != nil || n != len(was) {
		t.Fatalf("schema version %d, want %d (%v)", n, len(was), err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('runs') WHERE name = 'injected_by_test'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("the statement before the failure stayed: %d %v", n, err)
	}
	if s, err = Open(p); err != nil { // and the store still opens
		t.Fatal(err)
	}
	s.Close()
}

// The version a case event carries is the acting routine's: step 27 gives 1 to a routine's events (an actor with a
// "/") and 0 to anyone else's: an operator, a user, a host, flotillad.
func TestMigrationBackfillsCaseEventVersions(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.db")
	makeSchemaUpTo(t, p, 25)
	db, err := sql.Open("sqlite", p)
	if err != nil {
		t.Fatal(err)
	}
	const T = "2026-11-17T00:00:00Z"
	if _, err := db.Exec(`INSERT INTO cases (id, kind, target, canonical_key, status, opened_at, status_at) VALUES ('c1', 'failed_backup', 'host:a', 'k', 'closed', '` + T + `', '` + T + `')`); err != nil {
		t.Fatal(err)
	}
	for i, actor := range []string{"triage/cycle", "ops/rerun-backup", "host:a", "host:b", "user:x", "user:y", "operator", "flotillad"} {
		if _, err := db.Exec(`INSERT INTO case_events (case_id, at, kind, actor, by_role, from_status, to_status, detail) VALUES ('c1', ?, 'case.moved', ?, 'act', 'a', 'b', '')`, T+strconv.Itoa(i), actor); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	s, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	evs, err := s.CaseEvents(context.Background(), "c1")
	if err != nil || len(evs) != 8 {
		t.Fatalf("%d events %v", len(evs), err)
	}
	for _, e := range evs {
		want := 0
		if strings.Contains(e.Actor, "/") {
			want = 1
		}
		if e.Version != want {
			t.Errorf("%s: version %d, want %d", e.Actor, e.Version, want)
		}
	}
}

// The first start on a migrated store: every routine's v1 is recorded live over the history the migration left, and
// a later promote and rollback leave that history on version 1 (the old runs, plans and their approvals).
func TestFirstStartOnAMigratedStoreKeepsHistoryOnV1(t *testing.T) {
	ctx := context.Background()
	p := filepath.Join(t.TempDir(), "s25.db")
	makeSchema25(t, p)
	s, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	rs, err := spec.LoadVersions(filepath.Join("..", "..", "fleets"))
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range rs {
		if _, err := s.EnsureVersions(ctx, j.ID(), []LoadedVersion{{Version: 1, Hash: j.VersionHash, Manifest: j.Versions[0].Manifest, Legacy: true}}, "flotillad"); err != nil {
			t.Fatal(err)
		}
	}
	// a second version of triage/cycle arrives, goes live, and is rolled back
	if _, err := s.EnsureVersions(ctx, rt, []LoadedVersion{lv(1, "unused-already-recorded"), lv(2, "h2")}, "flotillad"); err != nil {
		t.Fatal(err)
	}
	if err := s.Promote(ctx, promote(s, 2, "h2")); err != nil {
		t.Fatal(err)
	}
	v1, _, _ := s.stateOf(t, rt, 1)
	if _, err := s.Rollback(ctx, RollbackArgs{Routine: rt, Hash: v1.Hash, Autonomy: "record", Current: "record", Actor: "user:ada", Via: "cli", Reason: "bad"}); err != nil {
		t.Fatal(err)
	}
	if got := states(t, s, rt); got != "v1=live v2=retired" {
		t.Fatalf("states %s", got)
	}
	runs, _ := s.Runs(ctx, rt, 100)
	if len(runs) != 2 {
		t.Fatalf("%d triage/cycle runs", len(runs))
	}
	for _, r := range runs {
		if r.Version != 1 {
			t.Errorf("run %s moved to version %d", r.ID, r.Version)
		}
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM plans WHERE version != 1 OR rolled_back_at IS NOT NULL`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("plans changed version or were withdrawn: %d %v", n, err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM runs WHERE version != 1`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("a run left version 1: %d %v", n, err)
	}
}

func (s *Store) stateOf(t *testing.T, routine string, n int) (RoutineVersion, bool, error) {
	t.Helper()
	vs, err := s.Versions(context.Background(), routine)
	for _, v := range vs {
		if v.Version == n {
			return v, true, err
		}
	}
	return RoutineVersion{}, false, err
}
