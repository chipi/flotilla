package sched

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/chipi/flotilla/internal/input"
	"github.com/chipi/flotilla/internal/runtoken"
	"github.com/chipi/flotilla/internal/spec"
	"github.com/chipi/flotilla/internal/store"
	"github.com/chipi/flotilla/internal/telemetry"
	"github.com/chipi/flotilla/internal/worker"
)

// Shadow runs (v2 step 2, docs/wip/v2-design.md section 4). When a live run of a routine starts, each shadow version of the routine
// is given the same input (what the live run was handed, the ledger as of its start, a snapshot of the routine's state, the same
// commit) and runs on the routine's shadow worker, which has the read credentials only, with its own token, its own namespace and its
// own model key. It writes to nothing but shadow records. Everything in this file is on the shadow path: it never calls a function
// that changes the live system (dispatch's TestTheShadowPathCallsNoLiveWrite reads this file and fails on one), and every line it
// logs goes through telemetry.ShadowLogger.

// ShadowConfig is flotillad.yaml's shadows section. Shadows start off.
type ShadowConfig struct {
	Enabled   bool `yaml:"enabled"`
	QueueMax  int  `yaml:"queue_max"`  // case inputs that may wait per version (default 20)
	AfterLive bool `yaml:"after_live"` // a shadow starts only when its live run has ended (a host without room for both)
}

// ShadowRecorder turns a shadow run's start and end into shadow records (dispatch.ShadowRecords is the one flotillad uses; it
// lives in dispatch because it reads a monitor's report as Detected does, and dispatch imports this package).
type ShadowRecorder interface {
	ShadowStarted(ctx context.Context, run store.Run, hash string, in store.Input) error
	ShadowDone(ctx context.Context, sj *spec.Routine, run store.Run, hash string, in store.Input, out *CaseOutcome) error
}

// ShadowMetrics is what a metrics sink needs to record a shadow run: its own series, never freshness or last success.
type ShadowMetrics interface {
	ShadowRun(key string, version int, status, autonomy string, durationS, spendDayUSD float64)
}

// The optional things a worker's runner can do. worker.Client does all three; a remote worker's runner none yet.
type (
	snapshotter interface {
		TakeSnapshot(ctx context.Context, runID string, paths []string) (*worker.Snapshot, error)
		DeleteSnapshot(ctx context.Context, runID string) (bool, error)
	}
	healthReader interface {
		Health(ctx context.Context) (*worker.Health, error)
	}
	commitResolver interface {
		ResolveCommit(ctx context.Context, repo, branch string) (string, error)
	}
)

type shadowTask struct {
	inputKey, routineID, kind, hash, worker string
	version                                 int
	rowID                                   int64
	liveDone                                <-chan struct{}
}

type shadowQueues struct {
	mu      sync.Mutex
	queues  map[string][]*shadowTask // by shadow worker name; one run at a time per worker
	running map[string]bool
	wg      sync.WaitGroup
}

func (s *Scheduler) sq() *shadowQueues {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.shadowQ == nil {
		s.shadowQ = &shadowQueues{queues: map[string][]*shadowTask{}, running: map[string]bool{}}
	}
	return s.shadowQ
}

// ShadowWait blocks until every shadow run started so far has ended (tests, and flotillad's shutdown).
func (s *Scheduler) ShadowWait() { s.sq().wg.Wait() }

func (s *Scheduler) shadowBase() context.Context {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.shadowCtx != nil {
		return s.shadowCtx
	}
	return context.Background()
}

func (s *Scheduler) queueMax() int {
	if s.Shadows.QueueMax > 0 {
		return s.Shadows.QueueMax
	}
	return 20
}

// shadowLog is the logger of one shadow version's run: every line carries shadow=true and routine_version.
func (s *Scheduler) shadowLog(version int) *slog.Logger {
	return telemetry.ShadowLogger(s.Log, version)
}

// resolveCommit is the commit a routine that names a repository works from, read by the live worker at dispatch and handed to the
// live run and to its shadows. A failure is logged and the run goes on without one; a shadow then does not run (no commit).
func (s *Scheduler) resolveCommit(ctx context.Context, j *spec.Routine, runner Runner) input.Commit {
	if j.Repo == nil {
		return input.Commit{}
	}
	r, ok := runner.(commitResolver)
	if !ok {
		s.Log.Warn("the routine names a repository, but its worker cannot resolve a commit", "routine", j.ID(), "worker", j.Worker)
		return input.Commit{}
	}
	sha, err := r.ResolveCommit(ctx, j.Repo.Name, j.Repo.Branch)
	if err != nil {
		s.Log.Warn("the commit of the routine's repository was not resolved", "routine", j.ID(), "repo", j.Repo.Name, "branch", j.Repo.Branch, "err", err)
		// unmarked on purpose: flotillad's own store failing is not a shadow's signal, and must alert
		s.StoreError("run_commit")
		return input.Commit{}
	}
	return input.Commit{Repo: j.Repo.Name, SHA: sha}
}

// shadowVersions are the routine's recorded versions in the shadow state.
func (s *Scheduler) shadowVersions(ctx context.Context, id string) ([]store.RoutineVersion, error) {
	vs, err := s.Store.Versions(ctx, id)
	if err != nil {
		return nil, err
	}
	var out []store.RoutineVersion
	for _, v := range vs {
		if v.State == store.VersionShadow {
			out = append(out, v)
		}
	}
	return out, nil
}

// snapshotWanted: shadows are on, the routine declares state, and a shadow version of it waits.
func (s *Scheduler) snapshotWanted(ctx context.Context, j *spec.Routine) bool {
	if !s.Shadows.Enabled || len(j.State) == 0 {
		return false
	}
	vs, err := s.shadowVersions(ctx, j.ID())
	return err == nil && len(vs) > 0
}

// takeSnapshot has the live worker copy the routine's state paths into its snapshot volume before the live run can write; the
// state is "held" or "none" (a failure is logged, and the shadows then do not run: no snapshot).
func (s *Scheduler) takeSnapshot(ctx context.Context, j *spec.Routine, runner Runner, runID string) string {
	sn, ok := runner.(snapshotter)
	if !ok {
		s.Log.Warn("the routine declares state, but its worker cannot snapshot it", "routine", j.ID(), "worker", j.Worker)
		return store.SnapshotNone
	}
	res, err := sn.TakeSnapshot(ctx, runID, j.State)
	if err != nil {
		s.Log.Warn("the state snapshot was not taken; the shadows will not run", "routine", j.ID(), "run_id", runID, "err", err)
		// unmarked on purpose: flotillad's own store failing is not a shadow's signal, and must alert
		s.StoreError("run_snapshot")
		return store.SnapshotNone
	}
	s.Log.Info("state snapshot taken", "routine", j.ID(), "run_id", runID, "bytes", res.Bytes, "files", res.Files, "skipped", res.Skipped)
	return store.SnapshotHeld
}

// enqueueShadows gives each shadow version of the routine its place in the queue for this input, or records why it will not run.
// Nothing here waits for a shadow: the live run goes straight on.
func (s *Scheduler) enqueueShadows(ctx context.Context, j *spec.Routine, in store.Input, liveDone <-chan struct{}) {
	if !s.Shadows.Enabled {
		return
	}
	vs, err := s.shadowVersions(ctx, j.ID())
	if err != nil {
		s.Log.Error("shadow versions", "routine", j.ID(), "err", err)
		return
	}
	for _, v := range vs {
		s.enqueueOne(ctx, j, in, v, liveDone)
	}
	s.afterShadowTerminal(ctx, in.Key)
}

func (s *Scheduler) enqueueOne(ctx context.Context, j *spec.Routine, in store.Input, v store.RoutineVersion, liveDone <-chan struct{}) {
	log := s.shadowLog(v.Version)
	wname := j.ShadowWorkerName()
	row := store.ShadowRun{InputKey: in.Key, Routine: j.ID(), Version: v.Version, VersionHash: v.Hash, LiveStartedAt: in.StartedAt}
	notRun := func(why string) {
		row.Status, row.Reason = store.ShadowNotRun, why
		if _, err := s.Store.QueueShadowRun(ctx, row); err != nil {
			log.Error("a shadow run's skip was not recorded", "routine", j.ID(), "input", in.Key, "why", why, "err", err)
		}
	}
	sv := j.VersionNamed(v.Version)
	switch {
	case sv == nil || sv.Err != nil:
		notRun("the version does not load")
		return
	case s.Workers[wname] == nil:
		notRun("no shadow worker " + wname + " is configured")
		return
	case j.Repo != nil && in.CommitSHA == "":
		notRun("no commit: the repository's head was not resolved")
		return
	case len(j.State) > 0 && in.SnapshotState != store.SnapshotHeld:
		notRun("no snapshot of the routine's state")
		return
	}
	q := s.sq()
	q.mu.Lock()
	defer q.mu.Unlock()
	var kept []*shadowTask
	pending := 0
	for _, t := range q.queues[wname] {
		if t.routineID == j.ID() && t.version == v.Version {
			if in.Kind == store.InputTick && t.kind == store.InputTick {
				// a scheduled input that waits while a newer one arrives is dropped and recorded as not run
				if err := s.Store.ShadowRunStep(ctx, t.rowID, store.ShadowNotRun, "superseded by "+in.Key, ""); err != nil {
					log.Error("a superseded shadow run was not recorded", "input", t.inputKey, "err", err)
				}
				continue
			}
			if t.kind != store.InputTick {
				pending++
			}
		}
		kept = append(kept, t)
	}
	q.queues[wname] = kept
	if in.Kind != store.InputTick && pending >= s.queueMax() {
		notRun("queue full")
		return
	}
	row.Status = store.ShadowQueued
	id, err := s.Store.QueueShadowRun(ctx, row)
	if err != nil {
		log.Error("a shadow run was not queued", "routine", j.ID(), "input", in.Key, "err", err)
		return
	}
	q.queues[wname] = append(q.queues[wname], &shadowTask{inputKey: in.Key, routineID: j.ID(), kind: in.Kind, hash: v.Hash, worker: wname,
		version: v.Version, rowID: id, liveDone: liveDone})
	if !q.running[wname] {
		q.running[wname] = true
		q.wg.Add(1)
		go s.pumpShadows(wname)
	}
}

// pumpShadows runs a shadow worker's queue, one run at a time, until it is empty.
func (s *Scheduler) pumpShadows(wname string) {
	q := s.sq()
	defer q.wg.Done()
	for {
		q.mu.Lock()
		if len(q.queues[wname]) == 0 {
			q.running[wname] = false
			q.mu.Unlock()
			return
		}
		t := q.queues[wname][0]
		q.queues[wname] = q.queues[wname][1:]
		q.mu.Unlock()
		s.runShadow(s.shadowBase(), t)
	}
}

// shadowSkip is why a shadow run that was queued does not start now ("" when it starts): the live skip rules, which a shadow keeps,
// and its own budget.
func (s *Scheduler) shadowSkip(ctx context.Context, j *spec.Routine, key string, runner Runner) (string, error) {
	if v, err := s.Store.Control(ctx, j.Fleet, KeyStop); err != nil {
		return "", err
	} else if v == "1" {
		return "fleet " + j.Fleet + " is STOPped", nil
	}
	if v, err := s.Store.Control(ctx, j.ID(), KeyPaused); err != nil {
		return "", err
	} else if v == "1" {
		return "routine is paused", nil
	}
	if !j.Enabled {
		return "routine is disabled", nil
	}
	h, ok := runner.(healthReader)
	if !ok {
		return "its worker cannot say it is a shadow worker (a remote worker's shadows are not supported)", nil
	}
	hc, err := h.Health(ctx)
	if err != nil {
		return "the shadow worker did not answer: " + err.Error(), nil
	}
	if !hc.ShadowCapable() {
		return "the worker is not a shadow worker (its /healthz does not say mode shadow)", nil
	}
	if j.Budget.DayUSD > 0 {
		day := s.Now().In(s.Location).Format("2006-01-02")
		spent, err := s.Store.Spend(ctx, day, key)
		if err != nil {
			return "", err
		}
		if spent >= j.Budget.DayUSD {
			return fmt.Sprintf("the shadow's daily budget is reached ($%.4f of $%.2f)", spent, j.Budget.DayUSD), nil
		}
	}
	return "", nil
}

// runShadow runs one queued shadow run: its own run row under "<fleet>/<routine>@v<n>", its own token, the shadow worker, and at
// the end its records. It changes nothing of the live system.
func (s *Scheduler) runShadow(ctx context.Context, t *shadowTask) {
	defer s.afterShadowTerminal(context.WithoutCancel(ctx), t.inputKey)
	log := s.shadowLog(t.version)
	key := store.ShadowKey(t.routineID, t.version)
	fail := func(status, why string) {
		if err := s.Store.ShadowRunStep(context.WithoutCancel(ctx), t.rowID, status, why, ""); err != nil {
			log.Error("a shadow run's ending was not recorded", "input", t.inputKey, "err", err)
		}
	}
	if s.Shadows.AfterLive {
		select {
		case <-t.liveDone:
		case <-ctx.Done():
			fail(store.ShadowNotRun, "flotillad stopped while it waited for its live run")
			return
		}
	}
	if s.Recorder == nil {
		fail(store.ShadowNotRun, "no recorder is wired: nothing could keep the shadow's records")
		return
	}
	live := s.Routine(t.routineID)
	if live == nil {
		fail(store.ShadowNotRun, "the routine is no longer loaded")
		return
	}
	sj, err := live.WithVersion(t.version)
	if err != nil {
		fail(store.ShadowNotRun, err.Error())
		return
	}
	runner := s.Workers[t.worker]
	if runner == nil {
		fail(store.ShadowNotRun, "no shadow worker "+t.worker+" is configured")
		return
	}
	in, err := s.Store.Input(ctx, t.inputKey)
	if err != nil {
		fail(store.ShadowNotRun, "the input is not recorded: "+err.Error())
		return
	}
	if why, err := s.shadowSkip(ctx, sj, key, runner); err != nil {
		log.Error("shadow skip rules", "routine", key, "err", err)
		fail(store.ShadowNotRun, "the skip rules could not be read")
		return
	} else if why != "" {
		log.Info("shadow run skipped", "routine", key, "input", t.inputKey, "why", why)
		fail(store.ShadowNotRun, why)
		return
	}
	now := s.Now()
	runID := newShadowRunID(sj, t.version, now)
	s.mu.Lock()
	if prev, busy := s.running[key]; busy {
		s.mu.Unlock()
		fail(store.ShadowNotRun, "already running ("+prev+")")
		return
	}
	s.running[key] = runID
	s.shadowRunner[runID] = runner
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.running, key)
		delete(s.cancels, runID)
		delete(s.shadowRunner, runID)
		s.mu.Unlock()
	}()

	var apiToken string
	if s.RunTokens != nil {
		if apiToken, err = s.RunTokens.Issue(runtoken.Run{ID: runID, Routine: t.routineID, Version: t.version, Worker: t.worker, Shadow: true, InputKey: t.inputKey}); err != nil {
			fail(store.ShadowNotRun, "run token: "+err.Error())
			return
		}
		defer s.RunTokens.Revoke(runID)
	}
	day := now.In(s.Location).Format("2006-01-02")
	spent, err := s.Store.Spend(ctx, day, key)
	if err != nil {
		fail(store.ShadowNotRun, "the shadow's spend could not be read")
		return
	}
	ctx, span := otel.Tracer("flotillad").Start(ctx, "shadow run "+key, trace.WithSpanKind(trace.SpanKindClient), trace.WithAttributes(
		attribute.String("flotilla.routine", key), attribute.String("flotilla.run_id", runID), attribute.Bool("flotilla.shadow", true),
		attribute.Int("flotilla.version", t.version), attribute.String("flotilla.input_key", t.inputKey), attribute.String("flotilla.worker", t.worker)))
	defer span.End()
	carrier := propagation.MapCarrier{}
	propagation.TraceContext{}.Inject(ctx, carrier)

	rec := store.Run{ID: runID, Routine: key, SpecHash: sj.Hash, Version: t.version, Trigger: "shadow", Actor: "flotillad", Autonomy: spec.AutonomyRecord,
		StartedAt: now.UTC().Format(store.TimeFormat), Status: store.StatusRunning, TraceID: span.SpanContext().TraceID().String(), Shadow: true, InputKey: t.inputKey}
	if err := s.Store.StartRun(ctx, rec); err != nil {
		fail(store.ShadowNotRun, "the run row: "+err.Error())
		return
	}
	if err := s.Store.ShadowRunStep(ctx, t.rowID, store.ShadowRunning, "", runID); err != nil {
		log.Error("a shadow run's start was not recorded", "input", t.inputKey, "err", err)
	}
	if err := s.Recorder.ShadowStarted(ctx, rec, t.hash, in); err != nil {
		log.Error("a shadow run's start record", "run_id", runID, "err", err)
	}

	env := s.runEnv(sj, runID, key, spec.FleetdStage(spec.AutonomyRecord), sj.Budget.DayUSD-spent, carrier.Get("traceparent"))
	handed := input.EnvOf(in.Handed)
	for k, v := range handed {
		env[k] = v
	}
	env["FLOTILLA_SHADOW"], env["FLOTILLA_INPUT_KEY"], env["FLOTILLA_LIVE_RUN_ID"] = "1", t.inputKey, t.inputKey
	if in.CommitSHA != "" {
		env["FLOTILLA_COMMIT"], env["FLOTILLA_REPO"] = in.CommitSHA, in.CommitRepo
	}
	planFile := ""
	if len(handed) > 0 || len(sj.Detects) > 0 {
		planFile = ".flotilla-plan-" + runID + ".json"
	}
	var snapshotOf string
	if in.SnapshotState == store.SnapshotHeld {
		snapshotOf = t.inputKey
	}
	log.Info("shadow run start", "routine", key, "run_id", runID, "input", t.inputKey, "worker", t.worker, "trace_id", rec.TraceID)
	res, callErr := runner.Run(ctx, worker.Request{RunID: runID, Routine: key, Command: sj.Run.Command, Workdir: sj.Run.Workdir,
		TimeoutS: sj.Limit.Seconds(), SpendFile: sj.Run.SpendFile, PlanFile: planFile, Env: env, APIToken: apiToken, Shadow: true,
		InputKey: t.inputKey, SnapshotOf: snapshotOf, State: sj.State})

	status, detail, spend, dur := store.StatusError, "", 0.0, 0.0
	var exit *int
	var outcome *CaseOutcome
	if callErr != nil {
		detail = callErr.Error()
		if from, err := time.Parse(store.TimeFormat, rec.StartedAt); err == nil && !errors.Is(callErr, worker.ErrUnreachable) {
			dur = max(s.Now().Sub(from).Seconds(), 0)
		}
		if errors.Is(callErr, worker.ErrUnreachable) || errors.Is(callErr, worker.ErrRefused) {
			status = store.StatusSkipped
		}
	} else {
		status, exit, spend, dur = res.Status, res.ExitCode, res.SpendUSD, res.DurationS
		outcome = &CaseOutcome{Plan: res.Plan, PlanError: res.PlanError}
		if status == store.StatusCancelled {
			detail = s.cancelNote(runID, res.OutputTail)
		} else if status != store.StatusOK {
			detail = tail(res.OutputTail, 800)
		}
	}
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := s.Store.FinishRun(fctx, runID, status, exit, spend, dur, detail); err != nil {
		log.Error("a shadow run's end", "run_id", runID, "err", err)
		fail(store.ShadowCancelled, "the run's end was not recorded")
		return
	}
	if callErr == nil && len(res.Calls) > 0 {
		calls := make([]store.ModelCall, len(res.Calls))
		for i, c := range res.Calls {
			at := s.Now().UTC()
			if tt, err := time.Parse(time.RFC3339Nano, c.At); err == nil {
				at = tt.UTC()
			}
			calls[i] = store.ModelCall{RunID: runID, Routine: key, At: at.Format(store.TimeFormat), Alias: c.Alias, Endpoint: c.Endpoint, Model: c.Model,
				Deployment: c.Deployment, TokensIn: c.TokensIn, TokensOut: c.TokensOut, CostUSD: c.CostUSD, CostSource: c.CostSource, LatencyS: c.LatencyS,
				Error: c.Error, Check: c.Check, CheckOK: c.CheckOK, Detail: c.Detail, FinishReason: c.FinishReason, Step: c.Step, DecisionKey: c.DecisionKey,
				Role: c.Role, Episode: c.Episode, EpisodeSeq: c.EpisodeSeq, Tools: store.NormalizeTools(c.Tools), Kind: c.Kind}
		}
		if err := s.Store.RecordCalls(fctx, calls); err != nil {
			log.Error("recording the shadow run's model calls", "run_id", runID, "err", err)
			// unmarked on purpose: flotillad's own store failing is not a shadow's signal, and must alert
			s.StoreError("run_calls")
		}
	}
	if callErr == nil && len(res.Loaded) > 0 {
		items := make([]store.Loaded, len(res.Loaded))
		for i, l := range res.Loaded {
			at := s.Now()
			if tt, err := time.Parse(time.RFC3339Nano, l.At); err == nil {
				at = tt
			}
			items[i] = store.Loaded{RunID: runID, Routine: key, At: at.UTC().Format(store.TimeFormat), Kind: l.Kind, Trust: l.Trust, Title: l.Title, Source: l.Source, SHA256: l.SHA256}
		}
		if err := s.Store.RecordLoaded(fctx, items); err != nil {
			log.Error("recording what the shadow run read", "run_id", runID, "err", err)
			// unmarked on purpose: flotillad's own store failing is not a shadow's signal, and must alert
			s.StoreError("run_loaded")
		}
	}
	total := spent
	if spend > 0 {
		if total, err = s.Store.AddSpend(fctx, day, key, spend); err != nil {
			log.Error("a shadow run's spend", "run_id", runID, "err", err)
		}
	}
	span.SetAttributes(attribute.String("flotilla.status", status), attribute.Float64("flotilla.spend_usd", spend))
	if status != store.StatusOK {
		span.SetStatus(codes.Error, status)
	}
	lvl := slog.LevelInfo
	if status != store.StatusOK {
		lvl = slog.LevelWarn
	}
	log.Log(ctx, lvl, "shadow run end", "routine", key, "run_id", runID, "status", status, "duration_s", dur, "spend_usd", spend, "detail", detail)
	if m, ok := s.Metrics.(ShadowMetrics); ok {
		m.ShadowRun(key, t.version, status, spec.AutonomyRecord, dur, total)
	}
	out := rec
	out.Status, out.ExitCode, out.SpendUSD, out.Detail = status, exit, spend, detail
	out.DurationS = &dur
	if err := s.Recorder.ShadowDone(fctx, sj, out, t.hash, in, outcome); err != nil {
		log.Error("a shadow run's records", "run_id", runID, "err", err)
	}
	end := store.ShadowDone
	if status == store.StatusCancelled {
		end = store.ShadowCancelled
	}
	if err := s.Store.ShadowRunStep(fctx, t.rowID, end, detail, ""); err != nil {
		log.Error("a shadow run's ending was not recorded", "input", t.inputKey, "err", err)
	}
}

// afterShadowTerminal deletes an input's state snapshot once every shadow run on it has ended, unless the input is pinned.
func (s *Scheduler) afterShadowTerminal(ctx context.Context, inputKey string) {
	n, err := s.Store.ShadowRunsActive(ctx, inputKey)
	if err != nil || n > 0 {
		return
	}
	s.releaseSnapshot(ctx, inputKey)
}

// ReleaseSnapshot deletes an input's held snapshot when nothing waits for it and it is not pinned: after the last shadow run, and when
// an operator unpins an input.
func (s *Scheduler) ReleaseSnapshot(ctx context.Context, inputKey string) {
	s.init()
	if n, err := s.Store.ShadowRunsActive(ctx, inputKey); err != nil || n > 0 {
		return
	}
	s.releaseSnapshot(ctx, inputKey)
}

func (s *Scheduler) releaseSnapshot(ctx context.Context, inputKey string) {
	in, err := s.Store.Input(ctx, inputKey)
	if err != nil || in.SnapshotState != store.SnapshotHeld || in.Pinned {
		return
	}
	j := s.Routine(in.Routine)
	if j == nil {
		return
	}
	sn, ok := s.Workers[j.Worker].(snapshotter)
	if !ok {
		return
	}
	if _, err := sn.DeleteSnapshot(ctx, inputKey); err != nil {
		s.shadowLog(in.Version).Warn("a snapshot was not deleted; the startup sweep will try again", "input", inputKey, "err", err)
		return
	}
	if err := s.Store.SetSnapshot(ctx, inputKey, store.SnapshotDeleted, in.SnapshotPaths); err != nil {
		s.shadowLog(in.Version).Error("a deleted snapshot was not recorded", "input", inputKey, "err", err)
	}
}

// SweepShadows is flotillad's start: shadow runs a stopped flotillad left queued or running are ended (their workers' runs were
// cancelled by RecoverInterrupted), and every snapshot nothing waits for any more, an unpinned one, is deleted.
func (s *Scheduler) SweepShadows(ctx context.Context) error {
	s.init()
	if _, err := s.Store.ExpireShadowRuns(ctx, "flotillad restarted"); err != nil {
		return err
	}
	held, err := s.Store.HeldSnapshots(ctx)
	if err != nil {
		return err
	}
	for _, in := range held {
		s.releaseSnapshot(ctx, in.Key)
	}
	return nil
}

// VersionRetired is the operator's retire of a shadow version: its run in flight is cancelled and what waits is not run.
func (s *Scheduler) VersionRetired(ctx context.Context, id string, n int) {
	s.dropQueued(ctx, id, n, "retired")
	key := store.ShadowKey(id, n)
	if _, err := s.Cancel(ctx, key, "flotillad", fmt.Sprintf("v%d retired", n)); err != nil && !errors.Is(err, ErrNotRunning) {
		s.shadowLog(n).Warn("a retired version's shadow run was not cancelled", "routine", key, "err", err)
	}
}

// VersionPromoted is the operator's promote of a shadow version: a run in flight finishes as a shadow, and what waits is not run (the
// live version runs from now on).
func (s *Scheduler) VersionPromoted(ctx context.Context, id string, n int) {
	s.dropQueued(ctx, id, n, "promoted before start")
}

func (s *Scheduler) dropQueued(ctx context.Context, id string, n int, why string) {
	q := s.sq()
	q.mu.Lock()
	var dropped []*shadowTask
	for w, ts := range q.queues {
		var kept []*shadowTask
		for _, t := range ts {
			if t.routineID == id && t.version == n {
				dropped = append(dropped, t)
				continue
			}
			kept = append(kept, t)
		}
		q.queues[w] = kept
	}
	q.mu.Unlock()
	for _, t := range dropped {
		if err := s.Store.ShadowRunStep(ctx, t.rowID, store.ShadowNotRun, why, ""); err != nil {
			s.shadowLog(n).Error("a dropped shadow run was not recorded", "input", t.inputKey, "err", err)
		}
		s.afterShadowTerminal(ctx, t.inputKey)
	}
}

// ShadowKeysRunning lists the shadow keys of a fleet with a run in flight (a STOP cancels them with the live runs).
func (s *Scheduler) ShadowKeysRunning(fleet string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for k := range s.running {
		if store.IsShadowKey(k) && strings.HasPrefix(k, fleet+"/") {
			out = append(out, k)
		}
	}
	return out
}

// ShadowRunOn runs a shadow version on an input a live run already recorded (the operator's "shadow-run on a given input": an eval's way in,
// and the way to see a before and after of exactly one shadow run). It takes the same path as a shadow started by its live run: the same queue,
// worker, token and records. It refuses what that path would skip silently: shadows off, an input of another routine, a version that is not a
// shadow, a state snapshot that is not held, and a version that already has a shadow run on the input.
func (s *Scheduler) ShadowRunOn(ctx context.Context, id string, version int, inputKey string) (store.ShadowRun, error) {
	s.init()
	if !s.Shadows.Enabled {
		return store.ShadowRun{}, fmt.Errorf("%w: shadows are off (shadows.enabled in flotillad.yaml)", ErrShadowRunRefused)
	}
	j := s.Routine(id)
	if j == nil {
		return store.ShadowRun{}, fmt.Errorf("no routine %s", id)
	}
	in, err := s.Store.Input(ctx, inputKey)
	if err != nil {
		return store.ShadowRun{}, fmt.Errorf("%w: %v", ErrShadowRunRefused, err)
	}
	if in.Routine != id {
		return store.ShadowRun{}, fmt.Errorf("%w: input %s belongs to %s, not %s", ErrShadowRunRefused, inputKey, in.Routine, id)
	}
	vs, err := s.shadowVersions(ctx, id)
	if err != nil {
		return store.ShadowRun{}, err
	}
	var v *store.RoutineVersion
	for i := range vs {
		if vs[i].Version == version {
			v = &vs[i]
		}
	}
	if v == nil {
		return store.ShadowRun{}, fmt.Errorf("%w: %s v%d is not a shadow version", ErrShadowRunRefused, id, version)
	}
	if len(j.State) > 0 && in.SnapshotState != store.SnapshotHeld {
		return store.ShadowRun{}, fmt.Errorf("%w: the input's state snapshot is %s: pin the input before its live run's shadows end, or use a newer input", ErrShadowRunRefused, in.SnapshotState)
	}
	if rows, err := s.Store.ShadowRuns(ctx, store.ShadowRunFilter{InputKey: inputKey, Routine: id, Version: version}); err != nil {
		return store.ShadowRun{}, err
	} else if len(rows) > 0 {
		return rows[0], fmt.Errorf("%w: %s v%d already has a shadow run on %s (%s)", store.ErrShadowRunExists, id, version, inputKey, rows[0].Status)
	}
	done := make(chan struct{})
	close(done)
	s.enqueueOne(ctx, j, in, *v, done)
	rows, err := s.Store.ShadowRuns(ctx, store.ShadowRunFilter{InputKey: inputKey, Routine: id, Version: version})
	if err != nil || len(rows) == 0 {
		return store.ShadowRun{}, fmt.Errorf("the shadow run was not queued: %v", err)
	}
	if rows[0].Status == store.ShadowNotRun {
		return rows[0], fmt.Errorf("%w: %s", ErrShadowRunRefused, rows[0].Reason)
	}
	return rows[0], nil
}

// ErrShadowRunRefused is ShadowRunOn's refusal: the shadow run was not queued, and the message says why.
var ErrShadowRunRefused = errors.New("shadow run refused")
