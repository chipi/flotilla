package main

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chipi/flotilla/internal/api"
	"github.com/chipi/flotilla/internal/cases"
	"github.com/chipi/flotilla/internal/policy"
	"github.com/chipi/flotilla/internal/registry"
	"github.com/chipi/flotilla/internal/sched"
	"github.com/chipi/flotilla/internal/spec"
	"github.com/chipi/flotilla/internal/store"
	"github.com/chipi/flotilla/internal/versions"
	"github.com/chipi/flotilla/internal/worker"
)

// holdRunner is a worker whose run lasts until it is cancelled.
type holdRunner struct {
	started chan string
	stop    chan struct{}
	once    sync.Once
}

func (h *holdRunner) Run(ctx context.Context, r worker.Request) (*worker.Result, error) {
	h.started <- r.RunID
	<-h.stop
	z := 0
	return &worker.Result{Status: "cancelled", ExitCode: &z, OutputTail: "terminated"}, nil
}

func (h *holdRunner) Cancel(ctx context.Context, runID string) error {
	h.once.Do(func() { close(h.stop) })
	return nil
}

// versionedServer is the real API over a routine folder (v1 live, v2 and v3 shadows) and a real store, for the CLI
// to talk to: the operator's token is the one the test helper sends ("op-token").
func versionedServer(t *testing.T) (*httptest.Server, *api.Server, *holdRunner, string) {
	t.Helper()
	d := t.TempDir()
	put := func(rel, body string) {
		p := filepath.Join(d, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	put("triage/cycle/routine.yaml", "fleet: triage\nroutine: cycle\ndescription: one pass\nenabled: true\nworker: triage\ntrigger: {every: 10m}\nautonomy: record\nbudget: {day_usd: 2}\nfreshness: 35m\n")
	for _, v := range []string{"v1", "v2", "v3"} {
		put("triage/cycle/"+v+"/spec.yaml", "run: {command: \"echo "+v+"\", timeout: 20m}\n")
	}
	rs, err := spec.LoadVersions(d)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "f.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	bound, _, err := versions.Reconcile(context.Background(), st, rs)
	if err != nil {
		t.Fatal(err)
	}
	hold := &holdRunner{started: make(chan string, 2), stop: make(chan struct{})}
	s := &sched.Scheduler{Store: st, Workers: map[string]sched.Runner{"triage": hold}}
	s.SetRoutines(bound)
	reg, err := registry.Load(filepath.Join("..", "..", "host", "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	srv := &api.Server{Sched: s, Store: st, Workers: map[string]*worker.Client{}, Version: "test", Registry: reg, SpecsDir: d,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Callers: []api.Caller{{Name: "operator", Role: api.RoleOperator, TokenSHA256: api.HashToken("op-token")}}}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, srv, hold, d
}

// The CLI against the real API and store: exit codes and the API's own words for each refusal, the promote's answer,
// what a rollback prints (the run it cancelled and the cases it withdrew) and a re-record of a folder that changed.
func TestCLIAgainstTheRealAPI(t *testing.T) {
	ctx := context.Background()
	ts, srv, hold, dir := versionedServer(t)

	for _, c := range []struct {
		args []string
		code int
		want string // in stderr when the exit is not 0
	}{
		{[]string{"promote", "triage/cycle", "v2", "--autonomy", "propose"}, 1, "HTTP 400"},
		{[]string{"promote", "triage/cycle", "v2", "--autonomy", "yolo", "--reason", "x"}, 1, "needs the routine's autonomy"},
		{[]string{"promote", "triage/cycle", "v9", "--autonomy", "record", "--reason", "x"}, 1, "HTTP 404"},
		{[]string{"promote", "triage/cycle", "v1", "--autonomy", "record", "--reason", "x"}, 1, "is live, not a shadow"},
		{[]string{"promote", "triage/cycle", "two"}, 2, "the version is vN"},
		{[]string{"retire", "triage/cycle", "v1", "--reason", "x"}, 1, "roll back or promote instead"},
		{[]string{"retire", "triage/cycle", "v3"}, 1, "a retire needs who and a reason"},
		{[]string{"rollback", "triage/cycle", "--autonomy", "record", "--reason", "x"}, 1, "no previous version to roll back to"},
		{[]string{"versions", "triage/nothing"}, 1, "HTTP 404"},
		{[]string{"versions", "triage/nothing", "-json"}, 1, "HTTP 404"},
		{[]string{"versions", "rerecord", "triage/cycle", "v1", "--reason", "x"}, 1, "nothing to re-record"},
		{[]string{"versions", "rerecord", "triage/cycle", "v2", "--reason", "x"}, 1, "is a shadow"},
		{[]string{"versions", "rerecord", "triage/cycle", "v1"}, 1, "needs a reason"},
	} {
		out, errs, code := cli(t, ts.URL, c.args...)
		if code != c.code || !strings.Contains(errs, c.want) || strings.Contains(out, `"state"`) {
			t.Errorf("%v: exit %d, stderr %q, stdout %q; want exit %d with %q", c.args, code, errs, out, c.code, c.want)
		}
	}
	if rows, _ := srv.Store.Versions(ctx, "triage/cycle"); len(rows) != 3 || rows[0].State != store.VersionLive || rows[1].State != store.VersionShadow || rows[2].State != store.VersionShadow {
		t.Fatalf("a refused command changed the states: %+v", rows)
	}

	out, errs, code := cli(t, ts.URL, "promote", "triage/cycle", "v2", "--autonomy", "propose", "--reason", "no pairs yet; the diff was read")
	if code != 0 || !strings.Contains(out, `"state":"live"`) || !strings.Contains(out, `"version":"2"`) || !strings.Contains(out, `"autonomy":"propose"`) || !strings.Contains(out, `"by":"operator"`) {
		t.Fatalf("promote: exit %d (%s) %q", code, errs, out)
	}

	// a v2 run in flight and a v2 plan awaiting approval, then the rollback prints both
	done := make(chan error, 1)
	go func() { _, _, err := srv.Sched.RunWith(ctx, "triage/cycle", "manual", "operator", nil); done <- err }()
	var runID string
	select {
	case runID = <-hold.started:
	case <-time.After(10 * time.Second):
		t.Fatal("the run never reached the worker")
	}
	k := store.Key{Kind: "target", Value: "failed_backup/host:backup-01cli"}
	id, _, err := srv.Store.OpenCase(ctx, store.NewCase{Kind: "failed_backup", Target: "host:backup-01", Keys: []store.Key{k}, Canonical: k}, "ops/backup-monitor", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, to := range []struct{ status, by string }{{cases.HandedOver, cases.ByDetector}, {cases.Planned, cases.ByAct}} {
		if err := srv.Store.MoveCase(ctx, id, to.status, to.by, "", "triage/cycle", ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := srv.Store.ApplyPlan(ctx, store.PlanRecord{CaseID: id, RunID: runID, Action: "backup.rerun", Params: map[string]string{"host": "backup-01"},
		PlanHash: "hash-cli-0123456789", Outcome: policy.Propose, Autonomy: policy.Propose}, cases.AwaitingApproval, "", "host:backup-01", true); err != nil {
		t.Fatal(err)
	}
	out, errs, code = cli(t, ts.URL, "rollback", "triage/cycle", "--autonomy", "record", "--reason", "v2 files too much")
	if code != 0 || !strings.Contains(out, `"rolled_back":2`) || !strings.Contains(out, `"live":1`) || !strings.Contains(out, `"cases":["`+id+`"]`) || !strings.Contains(out, `"cancelled":["`+runID+`"]`) {
		t.Fatalf("rollback: exit %d (%s) %q", code, errs, out)
	}
	<-done

	// a folder that changed on disk is re-recorded by the operator's word, with the old and new hash printed
	if err := os.WriteFile(filepath.Join(dir, "triage/cycle/v1/spec.yaml"), []byte("run: {command: \"echo v1 edited\", timeout: 20m}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, errs, code = cli(t, ts.URL, "versions", "rerecord", "triage/cycle", "v1", "--reason", "a deliberate change")
	if code != 0 || !strings.Contains(out, `"old_hash"`) || !strings.Contains(out, `"new_hash"`) || !strings.Contains(out, `"changed":["spec.yaml (changed)"]`) {
		t.Fatalf("rerecord: exit %d (%s) %q", code, errs, out)
	}
	out, _, code = cli(t, ts.URL, "versions", "triage/cycle", "-json")
	if code != 0 || !strings.Contains(out, `"live":1`) {
		t.Fatalf("versions -json: %d %q", code, out)
	}
}
