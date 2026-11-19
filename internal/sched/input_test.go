package sched

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/chipi/flotilla/internal/spec"
	"github.com/chipi/flotilla/internal/store"
	"github.com/chipi/flotilla/internal/worker"
)

// A live run's input is frozen when it starts: what it was handed, the routine's ledger marks and the open case keys of what it
// detects, recorded under the run's id before the run can write.
func TestARunsInputIsFrozenAtItsStart(t *testing.T) {
	s, fr, _ := setup(t, 2)
	ctx := context.Background()
	j := s.Routine("triage/cycle")
	j.Detects = []string{"backup"}
	j.Version = 3
	ns := "triage/cycle"
	s.Store.WriteFiling(ctx, ns, map[string]string{"fingerprint": "a", "issue": "1"}, "old")
	s.Store.AppendEntry(ctx, ns, "decision", map[string]string{"fingerprint": "a"}, "old")
	mkCase := func(kind, key string) {
		if _, _, err := s.Store.OpenCase(ctx, store.NewCase{Kind: kind, Target: "t", Keys: []store.Key{{Kind: "alert", Value: key}}, Canonical: store.Key{Kind: "alert", Value: key}}, "ops/m", "d"); err != nil {
			t.Fatal(err)
		}
	}
	mkCase("backup", "b-1")
	mkCase("other", "o-1") // not a kind this routine detects
	wantF, wantE, _ := s.Store.LedgerMark(ctx, ns)

	// the run writes to the ledger as it runs: after the mark
	fr.res = &worker.Result{Status: "ok", ExitCode: new(int)}
	r, err := s.RunNow(ctx, ns, "manual", "operator")
	if err != nil {
		t.Fatal(err)
	}
	s.Store.WriteFiling(ctx, ns, map[string]string{"fingerprint": "b", "issue": "2"}, r.ID)

	in, err := s.Store.Input(ctx, r.ID)
	if err != nil {
		t.Fatalf("the input is recorded under the run's id: %v", err)
	}
	if in.Routine != ns || in.Kind != store.InputTick || in.Version != 3 || in.Trigger != "manual" || in.Actor != "operator" || in.StartedAt != r.StartedAt {
		t.Fatalf("%+v", in)
	}
	if in.MarkFilings != wantF || in.MarkEntries != wantE {
		t.Fatalf("the marks are as of the start: %d %d, want %d %d", in.MarkFilings, in.MarkEntries, wantF, wantE)
	}
	if len(in.LiveCaseKeys) != 1 || in.LiveCaseKeys[0] != "alert=b-1" {
		t.Fatalf("the open cases of the kinds it detects: %v", in.LiveCaseKeys)
	}
	if string(in.Handed) != "{}" || in.SnapshotState != store.SnapshotNone {
		t.Fatalf("a tick hands nothing: %s %s", in.Handed, in.SnapshotState)
	}
}

func TestACaseRunsInputHoldsWhatItWasHanded(t *testing.T) {
	s, _, _ := setup(t, 2)
	ctx := context.Background()
	env := map[string]string{"FLOTILLA_CASE": `{"id":"c1","kind":"backup"}`, "FLOTILLA_RESUME": `{"run_id":"r0","status":"interrupted"}`, "FLEETD_X": "no"}
	for trigger, want := range map[string]string{"case": store.InputCase, "check": store.InputCheck, "execute": store.InputExecute} {
		r, _, err := s.RunWith(ctx, "triage/cycle", trigger, "flotillad", env)
		if err != nil {
			t.Fatal(err)
		}
		in, err := s.Store.Input(ctx, r.ID)
		if err != nil || in.Kind != want {
			t.Fatalf("%s: %+v %v", trigger, in, err)
		}
		var handed map[string]map[string]any
		if err := json.Unmarshal(in.Handed, &handed); err != nil || handed["case"]["id"] != "c1" || handed["resume"]["run_id"] != "r0" || len(handed) != 2 {
			t.Fatalf("%s: handed %s %v", trigger, in.Handed, err)
		}
	}
}

// A skipped run has no input, and a routine that is not a monitor has no live case keys to freeze.
func TestASkippedRunHasNoInput(t *testing.T) {
	s, fr, _ := setup(t, 2)
	ctx := context.Background()
	if err := s.Store.SetControl(ctx, "triage/cycle", KeyPaused, "1", "operator", "test"); err != nil {
		t.Fatal(err)
	}
	r, err := s.RunNow(ctx, "triage/cycle", "manual", "operator")
	if err != nil || r.Status != store.StatusSkipped {
		t.Fatalf("%+v %v", r, err)
	}
	if _, err := s.Store.Input(ctx, r.ID); err == nil {
		t.Fatal("an input for a run that never started")
	}
	if len(fr.got) != 0 {
		t.Fatal("a skipped run was sent")
	}
}

// The live fleet does not stop for its shadows: a failure to record the input is logged and counted, and the run goes on.
func TestARunGoesOnWhenItsInputCannotBeRecorded(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.db")
	st, err := store.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	raw, err := sql.Open("sqlite", p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`DROP TABLE run_inputs`); err != nil {
		t.Fatal(err)
	}
	raw.Close()
	zero := 0
	fr := &fakeRunner{res: &worker.Result{Status: "ok", ExitCode: &zero, DurationS: 1}}
	fm := &errMetrics{}
	j := &spec.Routine{Fleet: "triage", Routine: "cycle", Description: "d", Enabled: true, Worker: "triage", Run: spec.Run{Command: "true"},
		Autonomy: spec.AutonomyRecord, Every: 10 * time.Minute, Limit: time.Minute, Fresh: time.Hour, Hash: "h"}
	s := &Scheduler{Store: st, Workers: map[string]Runner{"triage": fr}, Metrics: fm}
	s.SetRoutines([]*spec.Routine{j})
	r, err := s.RunNow(context.Background(), "triage/cycle", "manual", "operator")
	if err != nil || r.Status != store.StatusOK {
		t.Fatalf("the run is the live fleet's: %+v %v", r, err)
	}
	if len(fr.got) != 1 {
		t.Fatalf("it ran: %d requests", len(fr.got))
	}
	if len(fm.storeErrors) != 1 || fm.storeErrors[0] != "run_input" {
		t.Fatalf("the failure is counted: %v", fm.storeErrors)
	}
}

type errMetrics struct {
	fakeMetrics
	storeErrors []string
}

func (m *errMetrics) StoreError(op string) { m.storeErrors = append(m.storeErrors, op) }
