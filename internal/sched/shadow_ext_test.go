package sched_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chipi/flotilla/internal/dispatch"
	"github.com/chipi/flotilla/internal/runtoken"
	"github.com/chipi/flotilla/internal/sched"
	"github.com/chipi/flotilla/internal/spec"
	"github.com/chipi/flotilla/internal/store"
	"github.com/chipi/flotilla/internal/versions"
	"github.com/chipi/flotilla/internal/worker"
)

// ---- the world: a routine folder with v1 live and v2 shadow, a live worker and a shadow worker as fakes

// liveWorker is the live worker: it runs the live command, takes and deletes snapshots and resolves a commit.
type liveWorker struct {
	mu        sync.Mutex
	order     []string // "snapshot:<run>", "run:<run>" in the order they happened
	reqs      []worker.Request
	res       *worker.Result
	snapErr   error
	commit    string
	commitErr error
	deleted   []string
	hold      chan struct{} // when set, a run waits for it
}

func (w *liveWorker) Run(ctx context.Context, r worker.Request) (*worker.Result, error) {
	w.mu.Lock()
	w.order = append(w.order, "run:"+r.RunID)
	w.reqs = append(w.reqs, r)
	w.mu.Unlock()
	if w.hold != nil {
		<-w.hold
	}
	return w.res, nil
}

func (w *liveWorker) TakeSnapshot(ctx context.Context, runID string, paths []string) (*worker.Snapshot, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.order = append(w.order, "snapshot:"+runID)
	if w.snapErr != nil {
		return nil, w.snapErr
	}
	return &worker.Snapshot{RunID: runID, Bytes: 10, Files: 1}, nil
}

func (w *liveWorker) DeleteSnapshot(ctx context.Context, runID string) (bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.deleted = append(w.deleted, runID)
	return true, nil
}

func (w *liveWorker) ResolveCommit(ctx context.Context, repo, branch string) (string, error) {
	return w.commit, w.commitErr
}

func (w *liveWorker) snapshotOrder() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.order...)
}

// shadowWorker is the shadow worker: it says its mode and runs a shadow run, taking its time from hold.
type shadowWorker struct {
	mu      sync.Mutex
	reqs    []worker.Request
	mode    string
	res     *worker.Result
	err     error
	hold    chan struct{}
	started chan string
	cancels []string
	stopped bool
	reg     *runtoken.Registry
	during  []runtoken.Run
}

func (w *shadowWorker) Run(ctx context.Context, r worker.Request) (*worker.Result, error) {
	w.mu.Lock()
	w.reqs = append(w.reqs, r)
	if w.reg != nil {
		run, _ := w.reg.Resolve(r.APIToken)
		w.during = append(w.during, run)
	}
	w.mu.Unlock()
	if w.started != nil {
		w.started <- r.RunID
	}
	if w.hold != nil {
		select {
		case <-w.hold:
		case <-ctx.Done():
		}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stopped {
		return &worker.Result{Status: "cancelled", ExitCode: new(int), OutputTail: "terminated"}, nil
	}
	return w.res, w.err
}

func (w *shadowWorker) Health(ctx context.Context) (*worker.Health, error) {
	return &worker.Health{OK: true, Mode: w.mode}, nil
}

func (w *shadowWorker) Cancel(ctx context.Context, runID string) error {
	w.mu.Lock()
	w.cancels = append(w.cancels, runID)
	w.stopped = true
	w.mu.Unlock()
	if w.hold != nil {
		defer func() { recover() }()
		close(w.hold)
	}
	return nil
}

func (w *shadowWorker) requests() []worker.Request {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]worker.Request(nil), w.reqs...)
}

type metricsSink struct {
	mu                     sync.Mutex
	live, shadow           []string
	lastSuccess, freshness int
	shadowSeries           []string
}

func (m *metricsSink) Run(routine, status, autonomy string, d, s float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.live = append(m.live, routine+":"+status)
}
func (m *metricsSink) LastSuccess(routine string, at time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastSuccess++
}
func (m *metricsSink) ShadowRun(key string, version int, status, autonomy string, d, s float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.shadow = append(m.shadow, fmt.Sprintf("%s:v%d:%s", key, version, status))
}

type world struct {
	t       *testing.T
	st      *store.Store
	path    string
	s       *sched.Scheduler
	live    *liveWorker
	shadow  *shadowWorker
	metrics *metricsSink
	logs    *bytes.Buffer
	reg     *runtoken.Registry
}

func put(t *testing.T, d, rel, body string) {
	t.Helper()
	p := filepath.Join(d, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// newWorld loads fleets/ops/rerun with v1 live and v2 shadow; routineExtra is added to routine.yaml (a repo, state, detects, a trigger).
func newWorld(t *testing.T, routineExtra string) *world {
	t.Helper()
	return newWorldT(t, "{every: 10m}", routineExtra)
}

func newWorldT(t *testing.T, trigger, routineExtra string) *world {
	t.Helper()
	d := t.TempDir()
	put(t, d, "ops/rerun/routine.yaml", "fleet: ops\nroutine: rerun\ndescription: reruns\nenabled: true\nworker: ops\ntrigger: "+trigger+"\nautonomy: record\nbudget: {day_usd: 1}\nfreshness: 35m\n"+routineExtra)
	put(t, d, "ops/rerun/v1/spec.yaml", "run: {command: \"echo v1\", timeout: 5m}\n")
	put(t, d, "ops/rerun/v2/spec.yaml", "run: {command: \"echo v2\", timeout: 5m}\n")
	rs, err := spec.LoadVersions(d)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "f.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	bound, _, err := versions.Reconcile(context.Background(), st, rs)
	if err != nil {
		t.Fatal(err)
	}
	zero := 0
	w := &world{t: t, st: st, path: path, metrics: &metricsSink{}, logs: &bytes.Buffer{}, reg: runtoken.New()}
	w.live = &liveWorker{res: &worker.Result{Status: "ok", ExitCode: &zero, DurationS: 1}, commit: "0123456789abcdef0123456789abcdef01234567"}
	w.shadow = &shadowWorker{mode: "shadow", res: &worker.Result{Status: "ok", ExitCode: &zero, DurationS: 2, SpendUSD: 0.05}, reg: w.reg}
	w.s = &sched.Scheduler{Store: st, Workers: map[string]sched.Runner{"ops": w.live, "ops-shadow": w.shadow}, Metrics: w.metrics,
		Log: slog.New(slog.NewJSONHandler(w.logs, nil)), RunTokens: w.reg, Recorder: dispatch.ShadowRecords{Store: st},
		Shadows: sched.ShadowConfig{Enabled: true}}
	w.s.SetRoutines(bound)
	return w
}

func (w *world) runLive(trigger string, env map[string]string) *store.Run {
	w.t.Helper()
	r, _, err := w.s.RunWith(context.Background(), "ops/rerun", trigger, "flotillad", env)
	if err != nil {
		w.t.Fatal(err)
	}
	return r
}

func (w *world) shadowRuns() []store.ShadowRun {
	rows, err := w.st.ShadowRuns(context.Background(), store.ShadowRunFilter{})
	if err != nil {
		w.t.Fatal(err)
	}
	return rows
}

func (w *world) count(q string, args ...any) int {
	db, err := sql.Open("sqlite", "file:"+w.path+"?mode=ro")
	if err != nil {
		w.t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(q, args...).Scan(&n); err != nil {
		w.t.Fatal(err)
	}
	return n
}

// ---- tests

func TestALiveRunStartsItsShadowOnTheShadowWorker(t *testing.T) {
	w := newWorld(t, "state: [signal-fleet/results]\n")
	ctx := context.Background()
	r := w.runLive("schedule", nil)
	w.s.ShadowWait()
	if r.Status != store.StatusOK {
		t.Fatalf("%+v", r)
	}
	// the snapshot is taken before the live run starts, and deleted when its shadows have ended
	if got := w.live.snapshotOrder(); len(got) != 2 || got[0] != "snapshot:"+r.ID || got[1] != "run:"+r.ID {
		t.Fatalf("the live worker snapshots the state, then runs: %v", got)
	}
	if len(w.live.deleted) != 1 || w.live.deleted[0] != r.ID {
		t.Fatalf("the snapshot is deleted when the shadows have ended: %v", w.live.deleted)
	}
	in, _ := w.st.Input(ctx, r.ID)
	if in.SnapshotState != store.SnapshotDeleted || len(in.SnapshotPaths) != 1 {
		t.Fatalf("%+v", in)
	}
	reqs := w.shadow.requests()
	if len(reqs) != 1 {
		t.Fatalf("one shadow run: %d", len(reqs))
	}
	q := reqs[0]
	if !q.Shadow || q.InputKey != r.ID || q.SnapshotOf != r.ID || len(q.State) != 1 || q.State[0] != "signal-fleet/results" || q.APIToken == "" ||
		q.Command != "echo v2" || q.Routine != "ops/rerun@v2" {
		t.Fatalf("the shadow's request: %+v", q)
	}
	for k, v := range map[string]string{"FLOTILLA_SHADOW": "1", "FLEETD_STAGE": "shadow", "FLOTILLA_INPUT_KEY": r.ID, "FLOTILLA_LIVE_RUN_ID": r.ID,
		"FLOTILLA_ROUTINE": "ops/rerun@v2", "FLOTILLA_ROUTINE_VERSION": "2"} {
		if q.Env[k] != v {
			t.Errorf("env %s = %q, want %q", k, q.Env[k], v)
		}
	}
	// its token was its own, a shadow's, for the shadow's key, and is revoked now
	if len(w.shadow.during) != 1 || !w.shadow.during[0].Shadow || w.shadow.during[0].Key() != "ops/rerun@v2" || w.shadow.during[0].InputKey != r.ID {
		t.Fatalf("the token during the run: %+v", w.shadow.during)
	}
	if _, ok := w.reg.Resolve(q.APIToken); ok {
		t.Fatal("a shadow run's token outlives it")
	}
	// the shadow's run row is its own: keyed @v2, flagged shadow, with its input, and never among the live runs
	runs, _ := w.st.Runs(ctx, "ops/rerun@v2", 10)
	if len(runs) != 1 || !runs[0].Shadow || runs[0].InputKey != r.ID || runs[0].Version != 2 || runs[0].Status != store.StatusOK || runs[0].Trigger != "shadow" {
		t.Fatalf("%+v", runs)
	}
	if live, _ := w.st.Runs(ctx, "ops/rerun", 10); len(live) != 1 || live[0].ID != r.ID {
		t.Fatalf("the live routine's runs are its own: %+v", live)
	}
	// its spend is under its own key, not the routine's
	day := time.Now().In(time.Local).Format("2006-01-02")
	if v, _ := w.st.Spend(ctx, day, "ops/rerun@v2"); v != 0.05 {
		t.Fatalf("the shadow's spend: %v", v)
	}
	if v, _ := w.st.Spend(ctx, day, "ops/rerun"); v != 0 {
		t.Fatalf("the live day's spend took the shadow's: %v", v)
	}
	// its queue row, its records, its metrics
	rows := w.shadowRuns()
	if len(rows) != 1 || rows[0].Status != store.ShadowDone || rows[0].RunID != runs[0].ID || rows[0].LiveStartedAt != in.StartedAt || rows[0].VersionHash == "" {
		t.Fatalf("%+v", rows)
	}
	recs, _ := w.st.ShadowRecords(ctx, store.ShadowRecordFilter{InputKey: r.ID})
	kinds := []string{}
	for _, x := range recs {
		kinds = append(kinds, x.Kind)
	}
	if strings.Join(kinds, " ") != "run.started run.ended" {
		t.Fatalf("a tick that reports nothing records its start and its end: %v", kinds)
	}
	if len(w.metrics.shadow) != 1 || w.metrics.shadow[0] != "ops/rerun@v2:v2:ok" || len(w.metrics.live) != 1 || w.metrics.live[0] != "ops/rerun:ok" || w.metrics.lastSuccess != 1 {
		t.Fatalf("a shadow's sample is its own series and never a live success: live %v shadow %v lastSuccess %d", w.metrics.live, w.metrics.shadow, w.metrics.lastSuccess)
	}
}

// With shadows off (the default) nothing is queued, snapshotted or sent.
func TestShadowsOffDoNothing(t *testing.T) {
	w := newWorld(t, "state: [signal-fleet/results]\n")
	w.s.Shadows.Enabled = false
	r := w.runLive("schedule", nil)
	w.s.ShadowWait()
	if got := w.live.snapshotOrder(); len(got) != 1 || got[0] != "run:"+r.ID {
		t.Fatalf("no snapshot: %v", got)
	}
	if len(w.shadow.requests()) != 0 || len(w.shadowRuns()) != 0 {
		t.Fatal("a shadow ran or was queued")
	}
	if _, err := w.st.Input(context.Background(), r.ID); err != nil {
		t.Fatalf("the live run's input is recorded all the same: %v", err)
	}
}

// S2: every log line of a shadow run's path carries shadow=true, and a shadow run writes no event into the live log.
func TestEveryShadowLogLineIsMarkedAndNoLiveEventIsWritten(t *testing.T) {
	w := newWorld(t, "")
	w.shadow.res = &worker.Result{Status: "error", OutputTail: "boom", Calls: []worker.Call{{Alias: "a", At: time.Now().UTC().Format(time.RFC3339Nano)}}}
	events := w.count(`SELECT COUNT(*) FROM events`) // the deploy's own, from loading the versions
	w.runLive("schedule", nil)
	w.s.ShadowWait()
	var shadowLines, markedLines int
	for _, ln := range strings.Split(strings.TrimSpace(w.logs.String()), "\n") {
		var rec map[string]any
		if err := json.Unmarshal([]byte(ln), &rec); err != nil {
			t.Fatalf("%q: %v", ln, err)
		}
		msg, _ := rec["msg"].(string)
		isShadow := strings.Contains(msg, "shadow") || strings.Contains(fmt.Sprint(rec["routine"]), "@v")
		if rec["shadow"] == true {
			markedLines++
			if rec["routine_version"] != float64(2) {
				t.Errorf("a marked line says its routine version: %s", ln)
			}
		}
		if isShadow {
			shadowLines++
			if rec["shadow"] != true {
				t.Errorf("a line of the shadow path is not marked shadow=true: %s", ln)
			}
		}
	}
	if shadowLines == 0 || markedLines < 2 {
		t.Fatalf("the shadow run logged (start, end): %d shadow lines, %d marked", shadowLines, markedLines)
	}
	// a failed shadow run logs at the level a failed live run does (WARN), never lower
	if !strings.Contains(w.logs.String(), `"level":"WARN","msg":"shadow run end"`) {
		t.Fatalf("a failing shadow logs its end as WARN: %s", w.logs.String())
	}
	// no row of the live event log came from the shadow path (the only events of a world with no operator are flotillad's own)
	if n := w.count(`SELECT COUNT(*) FROM events`); n != events {
		db, _ := sql.Open("sqlite", "file:"+w.path+"?mode=ro")
		rows, _ := db.Query(`SELECT kind, subject FROM events`)
		for rows.Next() {
			var k, s string
			rows.Scan(&k, &s)
			t.Errorf("event %s %s", k, s)
		}
		t.Fatalf("%d events written", n)
	}
}

// What a shadow run changes outside its own rows: nothing (cases, plans, holds, host executions, briefs, controls, ledger, spend of the live key).
func TestAShadowRunChangesNoLiveTableButItsOwn(t *testing.T) {
	w := newWorld(t, "detects: [backup]\nbrief_section: Backups\n")
	w.live.res = &worker.Result{Status: "ok", Plan: json.RawMessage(`{"outcome":"detections","summary":["x"],"detections":[]}`), ExitCode: new(int)}
	w.shadow.res = &worker.Result{Status: "ok", ExitCode: new(int), SpendUSD: 0.1,
		Plan: json.RawMessage(`{"outcome":"detections","summary":["a line"],"detections":[{"kind":"backup","target":"host:b","detail":"old"}]}`)}
	// an acting routine's would-be action must not arrive anywhere: no case, no plan, no host execution
	before := map[string]int{}
	tables := []string{"cases", "case_events", "plans", "target_locks", "holds", "host_executions", "events", "controls", "ledger_filings", "ledger_entries", "spend"}
	w.runLive("schedule", nil)
	w.s.ShadowWait()
	for _, tb := range tables {
		before[tb] = w.count("SELECT COUNT(*) FROM " + tb)
	}
	// a second live tick plus its shadow: live rows change (the live run's own), the shadow's never touch the others
	w.live.res = &worker.Result{Status: "ok", Plan: json.RawMessage(`{"outcome":"detections","detections":[]}`), ExitCode: new(int)}
	w.runLive("schedule", nil)
	w.s.ShadowWait()
	for _, tb := range []string{"cases", "plans", "target_locks", "holds", "host_executions", "controls", "ledger_filings", "ledger_entries"} {
		if n := w.count("SELECT COUNT(*) FROM " + tb); n != before[tb] {
			t.Errorf("table %s changed: %d -> %d", tb, before[tb], n)
		}
	}
	if n := w.count(`SELECT COUNT(*) FROM cases`); n != 0 {
		t.Fatalf("a shadow monitor opened %d cases", n)
	}
	recs, _ := w.st.ShadowRecords(context.Background(), store.ShadowRecordFilter{Kind: dispatch.RecDetectOpen})
	if len(recs) != 2 || recs[0].Subject != "target=backup/host:b" {
		t.Fatalf("what it would have opened is a record: %+v", recs)
	}
}

func TestAScheduledInputThatWaitsIsSupersededAndCaseInputsAreBounded(t *testing.T) {
	w := newWorld(t, "")
	w.shadow.hold = make(chan struct{})
	w.shadow.started = make(chan string, 8)
	w.s.Shadows.QueueMax = 2
	a := w.runLive("schedule", nil)
	<-w.shadow.started // the first shadow is running and holds the shadow worker
	b := w.runLive("schedule", nil)
	c := w.runLive("schedule", nil)
	rows := w.shadowRuns()
	by := map[string]store.ShadowRun{}
	for _, r := range rows {
		by[r.InputKey] = r
	}
	if by[a.ID].Status != store.ShadowRunning || by[b.ID].Status != store.ShadowNotRun || by[b.ID].Reason != "superseded by "+c.ID || by[c.ID].Status != store.ShadowQueued {
		t.Fatalf("a waits running, b is dropped for the newer c: %+v", rows)
	}
	// case inputs queue, up to the bound
	env := map[string]string{"FLOTILLA_CASE": `{"id":"c1"}`}
	var cases []*store.Run
	for i := 0; i < 4; i++ {
		cases = append(cases, w.runLive("case", env))
	}
	by = map[string]store.ShadowRun{}
	for _, r := range w.shadowRuns() {
		by[r.InputKey] = r
	}
	if by[cases[0].ID].Status != store.ShadowQueued || by[cases[1].ID].Status != store.ShadowQueued || by[cases[2].ID].Status != store.ShadowNotRun || by[cases[2].ID].Reason != "queue full" || by[cases[3].ID].Reason != "queue full" {
		t.Fatalf("two case inputs queue, the rest are 'queue full': %+v", by)
	}
	close(w.shadow.hold)
	w.s.ShadowWait()
	done := 0
	for _, r := range w.shadowRuns() {
		if r.Status == store.ShadowDone {
			done++
		}
		if r.Status == store.ShadowQueued || r.Status == store.ShadowRunning {
			t.Errorf("left in the queue: %+v", r)
		}
	}
	if done != 4 {
		t.Fatalf("a, c and the two queued cases ran: %d done", done)
	}
}

func TestSkipRules(t *testing.T) {
	for name, c := range map[string]struct {
		prep func(w *world)
		want string
	}{
		"fleet STOPped": {func(w *world) { w.st.SetControl(context.Background(), "ops", sched.KeyStop, "1", "operator", "t") }, "fleet ops is STOPped"},
		"paused": {func(w *world) {
			w.st.SetControl(context.Background(), "ops/rerun", sched.KeyPaused, "1", "operator", "t")
		}, "routine is paused"},
		"not a shadow worker": {func(w *world) { w.shadow.mode = "live" }, "is not a shadow worker"},
		"budget": {func(w *world) {
			w.st.AddSpend(context.Background(), time.Now().In(time.Local).Format("2006-01-02"), "ops/rerun@v2", 5)
		}, "the shadow's daily budget is reached"},
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t, "")
			// the live run starts (a STOPped or paused routine's does not, and so has no shadows), then the world changes, then the
			// shadow would start: it waits for its live run (after_live), so the change lands between the two
			w.s.Shadows.AfterLive = true
			w.live.hold = make(chan struct{})
			done := make(chan struct{})
			go func() { w.runLive("schedule", nil); close(done) }()
			for len(w.live.snapshotOrder()) == 0 {
				time.Sleep(10 * time.Millisecond)
			}
			c.prep(w)
			close(w.live.hold)
			<-done
			w.s.ShadowWait()
			if len(w.shadow.requests()) != 0 {
				t.Fatal("the shadow ran")
			}
			rows := w.shadowRuns()
			if len(rows) != 1 || rows[0].Status != store.ShadowNotRun || !strings.Contains(rows[0].Reason, c.want) {
				t.Fatalf("%+v, want a reason with %q", rows, c.want)
			}
			if n := w.count(`SELECT COUNT(*) FROM runs WHERE shadow = 1`); n != 0 {
				t.Fatalf("a skipped shadow left a run row: %d", n)
			}
		})
	}
}

// A STOPped or paused routine's live run is skipped, so it has no input and no shadow: the shadow starts only if the live run did.
func TestAShadowStartsOnlyIfItsLiveRunDid(t *testing.T) {
	w := newWorld(t, "")
	w.st.SetControl(context.Background(), "ops", sched.KeyStop, "1", "operator", "t")
	r := w.runLive("schedule", nil)
	w.s.ShadowWait()
	if r.Status != store.StatusSkipped || len(w.shadowRuns()) != 0 || len(w.shadow.requests()) != 0 {
		t.Fatalf("%+v", r)
	}
}

func TestWhyAShadowDoesNotRunIsRecorded(t *testing.T) {
	// no shadow worker configured
	w := newWorld(t, "")
	delete(w.s.Workers, "ops-shadow")
	w.runLive("schedule", nil)
	if rows := w.shadowRuns(); len(rows) != 1 || !strings.Contains(rows[0].Reason, "no shadow worker ops-shadow is configured") {
		t.Fatalf("%+v", rows)
	}
	// a state snapshot that failed
	w = newWorld(t, "state: [st]\n")
	w.live.snapErr = fmt.Errorf("disk full")
	r := w.runLive("schedule", nil)
	if rows := w.shadowRuns(); len(rows) != 1 || rows[0].Reason != "no snapshot of the routine's state" {
		t.Fatalf("%+v", rows)
	}
	if r.Status != store.StatusOK {
		t.Fatalf("a failed snapshot does not fail the live run: %+v", r)
	}
	// a repository whose head could not be resolved
	w = newWorld(t, "repo: {name: example/shop, branch: main}\n")
	w.live.commitErr = fmt.Errorf("rate limited")
	r = w.runLive("schedule", nil)
	if rows := w.shadowRuns(); len(rows) != 1 || !strings.Contains(rows[0].Reason, "no commit") || r.Status != store.StatusOK {
		t.Fatalf("%+v %+v", rows, r)
	}
	// no recorder wired
	w = newWorld(t, "")
	w.s.Recorder = nil
	w.runLive("schedule", nil)
	w.s.ShadowWait()
	if rows := w.shadowRuns(); len(rows) != 1 || !strings.Contains(rows[0].Reason, "no recorder") {
		t.Fatalf("%+v", rows)
	}
}

// The same commit goes to the live run and to its shadows.
func TestTheCommitIsResolvedOnceAndHandedToBoth(t *testing.T) {
	w := newWorld(t, "repo: {name: example/shop, branch: main}\n")
	r := w.runLive("schedule", nil)
	w.s.ShadowWait()
	sha := "0123456789abcdef0123456789abcdef01234567"
	if e := w.live.reqs[0].Env; e["FLOTILLA_COMMIT"] != sha || e["FLOTILLA_REPO"] != "example/shop" {
		t.Fatalf("the live run: %v", e)
	}
	if e := w.shadow.requests()[0].Env; e["FLOTILLA_COMMIT"] != sha || e["FLOTILLA_REPO"] != "example/shop" {
		t.Fatalf("the shadow: %v", e)
	}
	in, _ := w.st.Input(context.Background(), r.ID)
	if in.CommitSHA != sha || in.CommitRepo != "example/shop" {
		t.Fatalf("%+v", in)
	}
}

func TestAfterLiveStartsTheShadowWhenItsLiveRunHasEnded(t *testing.T) {
	w := newWorld(t, "")
	w.s.Shadows.AfterLive = true
	w.live.hold = make(chan struct{})
	done := make(chan struct{})
	go func() { w.runLive("schedule", nil); close(done) }()
	time.Sleep(300 * time.Millisecond)
	if len(w.shadow.requests()) != 0 {
		t.Fatal("the shadow started while its live run was running")
	}
	close(w.live.hold)
	<-done
	w.s.ShadowWait()
	if len(w.shadow.requests()) != 1 {
		t.Fatal("the shadow did not run after its live run")
	}
}

func TestRetireCancelsARunningShadowAndDropsTheQueued(t *testing.T) {
	w := newWorld(t, "")
	w.shadow.hold = make(chan struct{})
	w.shadow.started = make(chan string, 4)
	w.runLive("schedule", nil)
	<-w.shadow.started
	w.runLive("case", map[string]string{"FLOTILLA_CASE": `{"id":"c1"}`})
	w.s.VersionRetired(context.Background(), "ops/rerun", 2)
	w.s.ShadowWait()
	rows := w.shadowRuns()
	if len(rows) != 2 || rows[0].Status != store.ShadowCancelled || rows[1].Status != store.ShadowNotRun || rows[1].Reason != "retired" {
		t.Fatalf("%+v", rows)
	}
	if len(w.shadow.cancels) != 1 {
		t.Fatalf("the running shadow was cancelled once on the shadow worker: %v", w.shadow.cancels)
	}
}

func TestPromoteLetsARunningShadowFinishAndDropsTheQueued(t *testing.T) {
	w := newWorld(t, "")
	w.shadow.hold = make(chan struct{})
	w.shadow.started = make(chan string, 4)
	w.runLive("schedule", nil)
	<-w.shadow.started
	w.runLive("case", map[string]string{"FLOTILLA_CASE": `{"id":"c1"}`})
	w.s.VersionPromoted(context.Background(), "ops/rerun", 2)
	close(w.shadow.hold)
	w.s.ShadowWait()
	rows := w.shadowRuns()
	if len(rows) != 2 || rows[0].Status != store.ShadowDone || rows[1].Status != store.ShadowNotRun || rows[1].Reason != "promoted before start" {
		t.Fatalf("a run in flight finishes as a shadow, what waits does not run: %+v", rows)
	}
}

func TestACancelOfAShadowKeyReachesTheShadowWorker(t *testing.T) {
	w := newWorld(t, "")
	w.shadow.hold = make(chan struct{})
	w.shadow.started = make(chan string, 4)
	w.runLive("schedule", nil)
	runID := <-w.shadow.started
	if got := w.s.Running()["ops/rerun@v2"]; got != runID {
		t.Fatalf("running: %v", w.s.Running())
	}
	if keys := w.s.ShadowKeysRunning("ops"); len(keys) != 1 || keys[0] != "ops/rerun@v2" {
		t.Fatalf("%v", keys)
	}
	got, err := w.s.Cancel(context.Background(), "ops/rerun@v2", "operator", "STOP")
	if err != nil || got != runID {
		t.Fatalf("%q %v", got, err)
	}
	w.s.ShadowWait()
	if len(w.shadow.cancels) != 1 || w.shadow.cancels[0] != runID {
		t.Fatalf("%v", w.shadow.cancels)
	}
}

// A restart ends what it left queued or running and deletes the snapshots nothing waits for, but not a pinned one.
func TestTheStartupSweep(t *testing.T) {
	w := newWorld(t, "state: [st]\n")
	ctx := context.Background()
	w.shadow.hold = make(chan struct{})
	w.shadow.started = make(chan string, 4)
	a := w.runLive("schedule", nil)
	<-w.shadow.started
	b := w.runLive("case", map[string]string{"FLOTILLA_CASE": `{"id":"c1"}`})
	if err := w.st.PinInput(ctx, b.ID, true); err != nil {
		t.Fatal(err)
	}
	// "restart": a new scheduler on the same store
	w.shadow.mode = "shadow"
	fresh := &sched.Scheduler{Store: w.st, Workers: w.s.Workers, Log: w.s.Log, Recorder: w.s.Recorder}
	fresh.SetRoutines(w.s.Routines())
	if err := fresh.SweepShadows(ctx); err != nil {
		t.Fatal(err)
	}
	rows := w.shadowRuns()
	if rows[0].Status != store.ShadowCancelled || rows[0].Reason != "flotillad restarted" || rows[1].Status != store.ShadowNotRun {
		t.Fatalf("%+v", rows)
	}
	ia, _ := w.st.Input(ctx, a.ID)
	ib, _ := w.st.Input(ctx, b.ID)
	if ia.SnapshotState != store.SnapshotDeleted || ib.SnapshotState != store.SnapshotHeld {
		t.Fatalf("an unpinned snapshot is deleted and a pinned one is kept: %s %s", ia.SnapshotState, ib.SnapshotState)
	}
	// unpin, and the release deletes it
	w.st.PinInput(ctx, b.ID, false)
	fresh.ReleaseSnapshot(ctx, b.ID)
	if ib, _ = w.st.Input(ctx, b.ID); ib.SnapshotState != store.SnapshotDeleted {
		t.Fatalf("%s", ib.SnapshotState)
	}
	close(w.shadow.hold)
	w.s.ShadowWait()
}

// Q7: a shadow version of an acting routine: its plan is a record "would run <action>", and the action is never enqueued, never decided
// and never reaches a host: no plan, no case move, no host execution.
func TestAShadowOfAnActingRoutineOnlyRecordsWhatItWouldRun(t *testing.T) {
	w := newWorldT(t, "{case: {kind: backup}}", "")
	ctx := context.Background()
	id, _, err := w.st.OpenCase(ctx, store.NewCase{Kind: "backup", Target: "host:b", Keys: []store.Key{{Kind: "target", Value: "backup/host:b"}}, Canonical: store.Key{Kind: "target", Value: "backup/host:b"}}, "ops/m", "d")
	if err != nil {
		t.Fatal(err)
	}
	w.shadow.res = &worker.Result{Status: "ok", ExitCode: new(int),
		Plan: json.RawMessage(`{"outcome":"plan","case_id":"` + id + `","action":"backup.rerun_observability","params":{"host":"b"},"detail":"rerun it"}`)}
	caseEnv := map[string]string{"FLOTILLA_CASE": `{"id":"` + id + `","kind":"backup"}`}
	before := w.count(`SELECT COUNT(*) FROM case_events`)
	r := w.runLive("case", caseEnv)
	w.s.ShadowWait()
	q := w.shadow.requests()[0]
	if q.Env["FLOTILLA_CASE"] != caseEnv["FLOTILLA_CASE"] || q.PlanFile == "" {
		t.Fatalf("the shadow got the case the live run got, and answers in a plan file: %+v", q)
	}
	if n := w.count(`SELECT COUNT(*) FROM plans`) + w.count(`SELECT COUNT(*) FROM host_executions`) + w.count(`SELECT COUNT(*) FROM target_locks`); n != 0 {
		t.Fatalf("a shadow's action reached the plans, a lock or a host: %d rows", n)
	}
	if n := w.count(`SELECT COUNT(*) FROM case_events`); n != before {
		t.Fatalf("the case's timeline changed: %d -> %d", before, n)
	}
	recs, _ := w.st.ShadowRecords(ctx, store.ShadowRecordFilter{InputKey: r.ID})
	var plan, would *store.ShadowRecord
	for i := range recs {
		switch recs[i].Kind {
		case dispatch.RecPlan:
			plan = &recs[i]
		case dispatch.RecWouldRun:
			would = &recs[i]
		}
	}
	if plan == nil || would == nil || plan.Subject != id || would.Subject != id || !strings.Contains(string(would.Payload), "would run backup.rerun_observability (blocked: shadow)") {
		t.Fatalf("%+v", recs)
	}
}

func TestSpecKeysOfAShadowRoutineReachTheScheduler(t *testing.T) {
	w := newWorld(t, "shadow_worker: ops-shadow\nstate: [a/b]\nrepo: {name: example/shop, branch: main}\n")
	j := w.s.Routine("ops/rerun")
	if j.ShadowWorkerName() != "ops-shadow" || len(j.State) != 1 || j.State[0] != "a/b" || j.Repo == nil || j.Repo.Name != "example/shop" {
		t.Fatalf("%+v", j)
	}
	if sj, err := j.WithVersion(2); err != nil || sj.State[0] != "a/b" || sj.Run.Command != "echo v2" {
		t.Fatalf("a shadow version keeps the routine's keys: %+v %v", sj, err)
	}
}

// The operator's shadow-run on a given input: a live run made while shadows were off has an input and no shadow run; the shadow-run gives
// its shadow version exactly one, by the same path, and refuses what that path would skip silently.
func TestShadowRunOnAGivenInput(t *testing.T) {
	w := newWorld(t, "")
	ctx := context.Background()
	w.s.Shadows.Enabled = false
	r := w.runLive("schedule", nil)
	w.s.ShadowWait()
	if len(w.shadowRuns()) != 0 || len(w.shadow.requests()) != 0 {
		t.Fatal("shadows were off: nothing ran")
	}
	if _, err := w.s.ShadowRunOn(ctx, "ops/rerun", 2, r.ID); !errors.Is(err, sched.ErrShadowRunRefused) || !strings.Contains(err.Error(), "shadows are off") {
		t.Fatalf("with shadows off: %v", err)
	}
	w.s.Shadows.Enabled = true
	for name, c := range map[string]struct {
		version int
		input   string
		want    string
	}{
		"an unknown input":            {2, "nope", "no such input"},
		"the live version":            {1, r.ID, "is not a shadow version"},
		"a version that is not there": {9, r.ID, "is not a shadow version"},
	} {
		if _, err := w.s.ShadowRunOn(ctx, "ops/rerun", c.version, c.input); !errors.Is(err, sched.ErrShadowRunRefused) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	row, err := w.s.ShadowRunOn(ctx, "ops/rerun", 2, r.ID)
	if err != nil || row.Status != store.ShadowQueued && row.Status != store.ShadowRunning && row.Status != store.ShadowDone {
		t.Fatalf("%+v %v", row, err)
	}
	w.s.ShadowWait()
	rows := w.shadowRuns()
	if len(rows) != 1 || rows[0].Status != store.ShadowDone || rows[0].InputKey != r.ID || rows[0].Version != 2 {
		t.Fatalf("%+v", rows)
	}
	if reqs := w.shadow.requests(); len(reqs) != 1 || !reqs[0].Shadow || reqs[0].InputKey != r.ID || reqs[0].Routine != "ops/rerun@v2" {
		t.Fatalf("the shadow worker got the live input's shadow run: %+v", reqs)
	}
	if live, _ := w.st.Runs(ctx, "ops/rerun", 10); len(live) != 1 {
		t.Fatalf("the live runs are untouched: %+v", live)
	}
	if _, err := w.s.ShadowRunOn(ctx, "ops/rerun", 2, r.ID); !errors.Is(err, store.ErrShadowRunExists) {
		t.Fatalf("a second shadow run of the same version on the input: %v", err)
	}
}
