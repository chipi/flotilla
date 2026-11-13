package store

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/chipi/flotilla/internal/cases"
)

func backupCase() NewCase {
	k := Key{Kind: "target", Value: "failed_backup/host:mini"}
	return NewCase{Kind: "failed_backup", Target: "host:mini", Project: "homelab", Keys: []Key{k}, Canonical: k}
}

// v1's path (RFC-0004 §10): detected, handed over by the monitor, planned by the acting job, closed by
// policy as a recorded shadow plan; every step on the timeline with who made it.
func TestCaseShadowPath(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	id, opened, err := s.OpenCase(ctx, backupCase(), "ops/backup-monitor", "age 20.4h")
	if err != nil || !opened {
		t.Fatalf("open: %v %v", opened, err)
	}
	steps := []struct{ to, by, res string }{
		{cases.HandedOver, cases.ByDetector, ""},
		{cases.Planned, cases.ByAct, ""},
		{cases.Closed, cases.ByPolicy, cases.ShadowPlan},
	}
	for _, x := range steps {
		if err := s.MoveCase(ctx, id, x.to, x.by, x.res, "test", ""); err != nil {
			t.Fatalf("to %s: %v", x.to, err)
		}
	}
	c, _ := s.Case(ctx, id)
	if c.Status != cases.Closed || c.Resolution != cases.ShadowPlan || c.ClosedAt == nil || len(c.Keys) != 1 {
		t.Fatalf("closed case: %+v", c)
	}
	ev, _ := s.CaseEvents(ctx, id)
	var kinds []string
	for _, e := range ev {
		kinds = append(kinds, e.Kind+"/"+e.By)
	}
	want := []string{"case.detected/", "case.handed_over/detector", "case.planned/act", "case.closed/policy"}
	if len(kinds) != len(want) {
		t.Fatalf("timeline %v", kinds)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("timeline %v, want %v", kinds, want)
		}
	}
}

// A move the table refuses changes nothing, says why, and is on the timeline.
func TestRefusedMoveIsRecorded(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	id, _, _ := s.OpenCase(ctx, backupCase(), "m", "")
	err := s.MoveCase(ctx, id, cases.Executing, cases.ByAct, "", "ops-act-host", "")
	if !errors.Is(err, ErrBadTransition) {
		t.Fatalf("detected -> executing: %v", err)
	}
	c, _ := s.Case(ctx, id)
	ev, _ := s.CaseEvents(ctx, id)
	if c.Status != cases.Detected || ev[len(ev)-1].Kind != "transition.refused" || ev[len(ev)-1].Actor != "ops-act-host" {
		t.Fatalf("after a refused move: %s, %+v", c.Status, ev[len(ev)-1])
	}
	if err := s.MoveCase(ctx, "case-nope", cases.HandedOver, cases.ByDetector, "", "m", ""); !errors.Is(err, ErrNoSuchCase) {
		t.Fatalf("no such case: %v", err)
	}
}

// A storm is one case; once closed, the same key opens a new one; a closed case takes events but no
// status change.
func TestOneOpenCasePerKey(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	a, opened1, _ := s.OpenCase(ctx, backupCase(), "m", "first")
	b, opened2, _ := s.OpenCase(ctx, backupCase(), "m", "second")
	if a != b || !opened1 || opened2 {
		t.Fatalf("two detections of one key: %s %v, %s %v", a, opened1, b, opened2)
	}
	if ev, _ := s.CaseEvents(ctx, a); ev[len(ev)-1].Kind != "signal.attached" || ev[len(ev)-1].Detail != "second" {
		t.Fatalf("the second detection is not on the case: %+v", ev)
	}
	_ = s.MoveCase(ctx, a, cases.HandedOver, cases.ByDetector, "", "m", "")
	_ = s.MoveCase(ctx, a, cases.Closed, cases.ByAct, cases.ResolvedItself, "a", "")
	if err := s.AddCaseEvent(ctx, a, "outcome.checked", "check", "still fine after 3 days"); err != nil {
		t.Fatalf("an event on a closed case: %v", err)
	}
	if err := s.MoveCase(ctx, a, cases.HandedOver, cases.ByOperator, "", "user:ada", ""); !errors.Is(err, ErrBadTransition) {
		t.Fatalf("a closed case moved: %v", err)
	}
	c, opened3, _ := s.OpenCase(ctx, backupCase(), "m", "next day")
	if c == a || !opened3 {
		t.Fatal("after the close, the same key did not open a new case")
	}
	if err := s.AddCaseEvent(ctx, a, "case.closed", "x", ""); err == nil {
		t.Fatal("a caller wrote one of the store's own event kinds")
	}
}

func TestConcurrentDetectionsOpenOneCase(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	var wg sync.WaitGroup
	var mu sync.Mutex
	ids, opened := map[string]bool{}, 0
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, o, err := s.OpenCase(ctx, backupCase(), "m", "")
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				t.Error(err)
			}
			ids[id] = true
			if o {
				opened++
			}
		}()
	}
	wg.Wait()
	if len(ids) != 1 || opened != 1 {
		t.Fatalf("20 detections at once: %d cases, %d opened", len(ids), opened)
	}
}

// The database itself refuses a second open case for a key, whatever the code above it does.
func TestOneOpenCaseIsTheDatabasesRule(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	_, _, _ = s.OpenCase(ctx, backupCase(), "m", "")
	_, err := s.db.ExecContext(ctx, `INSERT INTO cases (id, kind, target, canonical_key, status, opened_at, status_at)
		VALUES ('case-x', 'failed_backup', 'host:mini', 'target=failed_backup/host:mini', 'handed_over', 'x', 'x')`)
	if err == nil {
		t.Fatal("a second open case for one key was stored")
	}
}

func TestNewCaseNeedsItsCanonicalKey(t *testing.T) {
	n := backupCase()
	n.Canonical = Key{Kind: "target", Value: "other"}
	if _, _, err := openTest(t).OpenCase(context.Background(), n, "m", ""); err == nil {
		t.Fatal("a canonical key that is not one of the case's keys")
	}
}

func TestCasesFilter(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	a, _, _ := s.OpenCase(ctx, backupCase(), "m", "")
	k := Key{Kind: "target", Value: "memory_growth/container:x"}
	b, _, _ := s.OpenCase(ctx, NewCase{Kind: "memory_growth", Target: "container:x", Keys: []Key{k}, Canonical: k}, "m", "")
	_ = s.MoveCase(ctx, a, cases.HandedOver, cases.ByDetector, "", "m", "")
	_ = s.MoveCase(ctx, a, cases.Closed, cases.ByAct, cases.ResolvedItself, "a", "")
	open, _ := s.Cases(ctx, CaseFilter{Open: true})
	if len(open) != 1 || open[0].ID != b {
		t.Fatalf("open: %+v", open)
	}
	all, _ := s.Cases(ctx, CaseFilter{Kind: "failed_backup"})
	if len(all) != 1 || all[0].ID != a {
		t.Fatalf("by kind: %+v", all)
	}
}
