// Package sched decides when a routine runs and runs it through its worker: fleetd's loop, with the
// controls, run records and budget in the store instead of flag files and a JSON state file.
package sched

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/chipi/flotilla/internal/input"
	"github.com/chipi/flotilla/internal/models"
	"github.com/chipi/flotilla/internal/runtoken"
	"github.com/chipi/flotilla/internal/spec"
	"github.com/chipi/flotilla/internal/store"
	"github.com/chipi/flotilla/internal/worker"
)

// Runner is the worker client (an interface so the tests can stand in for it).
type Runner interface {
	Run(ctx context.Context, r worker.Request) (*worker.Result, error)
}

// Canceller is a runner that can stop a run in flight: worker.Client, and a remote worker's runner (the
// link). The worker ends the run as its timeout would, and the run's own Run call returns "cancelled".
type Canceller interface {
	Cancel(ctx context.Context, runID string) error
}

// Metrics receives one sample per run; telemetry pushes them to VictoriaMetrics.
type Metrics interface {
	Run(routine, status, autonomy string, durationS, spendDayUSD float64)
	LastSuccess(routine string, at time.Time)
}

// StoreError counts a store write that failed without failing its caller, when the metrics sink has it.
func (s *Scheduler) StoreError(op string) {
	if m, ok := s.Metrics.(interface{ StoreError(string) }); ok {
		m.StoreError(op)
	}
}

// Control keys. A fleet's "stop" skips every routine of the fleet; a routine's "paused" skips that routine; a
// routine's "autonomy" overrides the spec's default. The installation's "frozen" (ADR-0014 decision 8) runs
// every routine at record; it is kept on FreezeSubject, which no fleet or routine id can be.
const (
	KeyStop       = "stop"
	KeyPaused     = "paused"
	KeyAutonomy   = "autonomy"
	KeyFrozen     = "frozen"
	FreezeSubject = "*"
)

var ErrRunning = errors.New("already running")

// ErrConfigDisabled is a run asked of a routine the installation's config lists in disabled_routines.
var ErrConfigDisabled = errors.New("disabled by this installation's configuration (disabled_routines): it does not run, by hand either")

// ErrNotRunning: Cancel found no run of that routine in flight.
var ErrNotRunning = errors.New("not running")

// cancelRetryEvery and cancelRetryFor bound how long a cancel waits for a run that flotillad has registered
// but whose request the worker has not taken yet (its answer to the cancel is "no such run").
const (
	cancelRetryEvery = 250 * time.Millisecond
	cancelRetryFor   = 15 * time.Second
)

type Scheduler struct {
	// ReadyWait bounds how long a routine's first run after Loop starts waits for its worker to answer
	// (the default 2 minutes): a deploy recreates the workers beside flotillad.
	ReadyWait time.Duration
	Store     *store.Store
	Workers   map[string]Runner
	Metrics   Metrics
	Location  *time.Location
	Log       *slog.Logger
	Now       func() time.Time
	// the installation's validated models section, as JSON, for the UI's Models page
	ModelsJSON string
	// the same, parsed: each run gets only the aliases its spec names (security T4)
	Models *models.Config
	// where a routine sends its own spans (flotillad's otlp_traces_url) and the environment label: every run
	// gets them as FLOTILLA_OTLP_TRACES_URL and FLOTILLA_ENVIRONMENT (runtime/telemetry.py)
	OTLPTracesURL string
	Environment   string
	AfterRun      AfterRun
	// RunTokens gives each run its own token for flotillad's workers' listener (internal/runtoken), issued before the worker is
	// asked to start the run and revoked when it ends; nil: runs get none and use their worker's token, as before.
	RunTokens *runtoken.Registry
	// Shadows are off unless flotillad.yaml turns them on; Recorder keeps what a shadow run did (see shadow.go).
	Shadows  ShadowConfig
	Recorder ShadowRecorder

	rmu      sync.RWMutex
	routines []*spec.Routine // each bound to the version that is live (Bind); read through Routines

	mu      sync.Mutex
	running map[string]string // routine id -> run id
	cancels map[string]string // run id -> "cancelled: <who>, <why>", while a cancel of it is pending

	shadowQ      *shadowQueues
	shadowCtx    context.Context   // what shadow runs run under: flotillad's, set by Loop
	shadowRunner map[string]Runner // shadow run id -> the shadow worker running it
}

func (s *Scheduler) init() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running == nil {
		s.running = map[string]string{}
		s.cancels = map[string]string{}
		s.shadowRunner = map[string]Runner{}
	}
	if s.Now == nil {
		s.Now = time.Now
	}
	if s.Location == nil {
		s.Location = time.Local
	}
	if s.Log == nil {
		s.Log = slog.Default()
	}
}

// Routines is the loaded routines, each bound to its live version: a copy of the list, the specs shared.
func (s *Scheduler) Routines() []*spec.Routine {
	s.rmu.RLock()
	defer s.rmu.RUnlock()
	return append([]*spec.Routine(nil), s.routines...)
}

// SetRoutines replaces the loaded routines (at start, and in tests).
func (s *Scheduler) SetRoutines(rs []*spec.Routine) {
	s.rmu.Lock()
	defer s.rmu.Unlock()
	s.routines = append([]*spec.Routine(nil), rs...)
}

// Routine returns the loaded spec for "fleet/routine".
func (s *Scheduler) Routine(id string) *spec.Routine {
	s.rmu.RLock()
	defer s.rmu.RUnlock()
	for _, j := range s.routines {
		if j.ID() == id {
			return j
		}
	}
	return nil
}

// Bind makes version n the one the routine runs from now on: it swaps in a copy of the routine bound to
// that version. A run in flight keeps the copy it started with, so it finishes as the version it began as.
func (s *Scheduler) Bind(id string, n int) error {
	s.rmu.Lock()
	defer s.rmu.Unlock()
	for i, j := range s.routines {
		if j.ID() != id {
			continue
		}
		c, err := j.WithVersion(n)
		if err != nil {
			return err
		}
		s.routines[i] = c
		return nil
	}
	return fmt.Errorf("no routine %s", id)
}

// Loop runs every enabled routine on its trigger until ctx ends: when it is due (its last run that was
// not skipped, plus its interval; at once if it has none), then every interval. A restart, a deploy's
// included, so starts nothing that is not due: before (2026-11-16) every routine ran the moment flotillad
// started, while compose was recreating its workers and the egress proxies, and failed on their names or
// its sources; a weekly routine ran again at every deploy. A run due at start first waits for its worker
// to answer (ReadyWait).
func (s *Scheduler) Loop(ctx context.Context) {
	s.init()
	s.mu.Lock()
	s.shadowCtx = ctx
	s.mu.Unlock()
	defer s.ShadowWait()
	var wg sync.WaitGroup
	for _, j := range s.Routines() {
		if !j.Enabled {
			s.Log.Info("routine disabled in its spec", "routine", j.ID())
			continue
		}
		if _, ok := s.Workers[j.Worker]; !ok {
			s.Log.Error("routine's worker is not configured: routine not started", "routine", j.ID(), "worker", j.Worker)
			continue
		}
		if j.OnEvent() {
			s.Log.Info("routine runs on cases, not on time", "routine", j.ID(), "kind", j.OnCase(), "execute", j.OnExecute(), "verify", j.OnCheck())
			continue
		}
		wg.Add(1)
		go func(j *spec.Routine) {
			defer wg.Done()
			wait := s.FirstWait(ctx, j)
			s.Log.Info("routine loop started", "routine", j.ID(), "every", j.Every.String(), "worker", j.Worker, "first_in", wait.Round(time.Second).String())
			if wait > 0 {
				select {
				case <-ctx.Done():
					return
				case <-time.After(wait):
				}
			}
			s.AwaitWorker(ctx, j)
			t := time.NewTicker(j.Every)
			defer t.Stop()
			for {
				if _, err := s.RunNow(ctx, j.ID(), "schedule", "flotillad"); err != nil && !errors.Is(err, ErrRunning) {
					s.Log.Error("run failed", "routine", j.ID(), "err", err)
				}
				select {
				case <-ctx.Done():
					return
				case <-t.C:
				}
			}
		}(j)
	}
	wg.Wait()
}

// FirstWait is how long a routine's first timed run waits after Loop starts: until its last run that was
// not skipped plus its interval, zero when that is past or it has never run.
func (s *Scheduler) FirstWait(ctx context.Context, j *spec.Routine) time.Duration {
	s.init()
	runs, err := s.Store.Runs(ctx, j.ID(), 50)
	if err != nil {
		s.Log.Error("first run: the routine's runs", "routine", j.ID(), "err", err)
		return 0
	}
	for _, r := range runs {
		if r.Status == store.StatusSkipped {
			continue
		}
		t, err := time.Parse(time.RFC3339Nano, r.StartedAt)
		if err != nil {
			return 0
		}
		if d := t.Add(j.Every).Sub(s.Clock()); d > 0 {
			return d
		}
		return 0
	}
	return 0
}

// AwaitWorker waits, at most ReadyWait (2 minutes by default), for the routine's worker to answer: a local
// worker its health check, a remote one its link. A worker that never answers is left to the run, which
// is then skipped.
func (s *Scheduler) AwaitWorker(ctx context.Context, j *spec.Routine) {
	s.init()
	ready, ok := s.Workers[j.Worker].(interface{ Ready(context.Context) bool })
	if !ok {
		if a, isA := s.Workers[j.Worker].(interface{ Available() bool }); isA {
			ready, ok = readyFunc(func(context.Context) bool { return a.Available() }), true
		}
	}
	if !ok {
		return
	}
	limit := s.ReadyWait
	if limit == 0 {
		limit = 2 * time.Minute
	}
	deadline := time.Now().Add(limit)
	for !ready.Ready(ctx) {
		if time.Now().After(deadline) {
			s.Log.Warn("worker did not answer before the first run", "routine", j.ID(), "worker", j.Worker, "waited", limit.String())
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
}

type readyFunc func(context.Context) bool

func (f readyFunc) Ready(ctx context.Context) bool { return f(ctx) }

// RunNow runs one routine now and waits for it. trigger says why ("schedule", "manual"), actor who.
// A skipped run (STOP, paused, budget) is recorded with its reason and returned without error.
func (s *Scheduler) RunNow(ctx context.Context, id, trigger, actor string) (*store.Run, error) {
	if j := s.Routine(id); j != nil && j.ConfigDisabled {
		return nil, fmt.Errorf("%s: %w", id, ErrConfigDisabled)
	}
	r, _, err := s.RunWith(ctx, id, trigger, actor, nil)
	return r, err
}

// CaseOutcome is what a run on a case reported: its plan file's object, or why there was none.
type CaseOutcome struct {
	Plan      json.RawMessage
	PlanError string
}

// AfterRun, when set, gets each run not on a case that reported, scheduled or manual (a monitor's
// detections, pipeline v1 step 6). flotillad sets it to the dispatcher.
type AfterRun func(ctx context.Context, j *spec.Routine, r *store.Run, out *CaseOutcome)

// RunWith is RunNow with the run's case (pipeline v1 step 5): caseEnv is added to the run's environment
// (FLOTILLA_CASE), and the run gets a plan file whose content comes back. caseEnv nil is RunNow.
func (s *Scheduler) RunWith(ctx context.Context, id, trigger, actor string, caseEnv map[string]string) (*store.Run, *CaseOutcome, error) {
	return s.RunWithin(ctx, id, trigger, actor, caseEnv, 0)
}

// RunWithin is RunWith with the run's timeout at most `within` (0: the spec's limit alone): a case's runs share
// one time limit (RFC-0013 §2), so a run gets no more than what is left of it.
func (s *Scheduler) RunWithin(ctx context.Context, id, trigger, actor string, caseEnv map[string]string, within time.Duration) (*store.Run, *CaseOutcome, error) {
	s.init()
	j := s.Routine(id)
	if j == nil {
		return nil, nil, fmt.Errorf("no routine %s", id)
	}
	timeout := j.Limit
	if within > 0 && within < timeout {
		timeout = within
	}
	runner, ok := s.Workers[j.Worker]
	if !ok {
		return nil, nil, fmt.Errorf("routine %s: worker %s is not configured", id, j.Worker)
	}
	runID := newRunID(j, s.Now())
	s.mu.Lock()
	if prev, busy := s.running[id]; busy {
		s.mu.Unlock()
		return nil, nil, fmt.Errorf("%s: %w (%s)", id, ErrRunning, prev)
	}
	s.running[id] = runID
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.running, id); delete(s.cancels, runID); s.mu.Unlock() }()

	autonomy, err := s.Autonomy(ctx, j)
	if err != nil {
		return nil, nil, err
	}
	rec := store.Run{ID: runID, Routine: id, SpecHash: j.Hash, Version: j.Version, Trigger: trigger, Actor: actor, Autonomy: autonomy}
	if reason, err := s.skipReason(ctx, j); err != nil {
		return nil, nil, err
	} else if reason != "" {
		rec.Detail, rec.Status = reason, store.StatusSkipped
		rec.StartedAt = s.Now().UTC().Format(store.TimeFormat)
		if err := s.Store.SkippedRun(ctx, rec); err != nil {
			return nil, nil, err
		}
		s.Log.Info("run skipped", "routine", id, "run_id", runID, "reason", reason)
		s.metric(j, store.StatusSkipped, autonomy, 0)
		return &rec, nil, nil
	}

	day := s.Now().In(s.Location).Format("2006-01-02")
	spent, err := s.Store.Spend(ctx, day, id)
	if err != nil {
		return nil, nil, err
	}
	tracer := otel.Tracer("flotillad")
	ctx, span := tracer.Start(ctx, "run "+id, trace.WithSpanKind(trace.SpanKindClient), trace.WithAttributes(
		attribute.String("flotilla.routine", id), attribute.String("flotilla.run_id", runID),
		attribute.String("flotilla.autonomy", autonomy), attribute.String("flotilla.trigger", trigger),
		attribute.String("flotilla.worker", j.Worker), attribute.String("flotilla.spec_hash", j.Hash), attribute.Int("flotilla.version", j.Version)))
	defer span.End()
	carrier := propagation.MapCarrier{}
	propagation.TraceContext{}.Inject(ctx, carrier)

	rec.StartedAt = s.Now().UTC().Format(store.TimeFormat)
	rec.Status = store.StatusRunning
	rec.TraceID = span.SpanContext().TraceID().String()
	apiToken := ""
	if s.RunTokens != nil {
		// before the run row, so a refusal leaves no run "running"; revoked on every way out of this function, the run's end included.
		// A routine with no recorded versions runs as its one, v1 (as store.StartRun reads it).
		// The token lives in this process only: after a flotillad restart an orphaned live run's ledger write gets 401 until the recovery's
		// cancel lands (its token went with the old process; it used to succeed with the worker's own). A window of seconds, for a run that
		// is cancelled anyway. The shadow path's issue (shadow.go) is the same.
		if apiToken, err = s.RunTokens.Issue(runtoken.Run{ID: runID, Routine: id, Version: max(j.Version, 1), Worker: j.Worker}); err != nil {
			return nil, nil, fmt.Errorf("run token: %w", err)
		}
		defer s.RunTokens.Revoke(runID)
	}
	if err := s.Store.StartRun(ctx, rec); err != nil {
		return nil, nil, err
	}
	liveDone := make(chan struct{})
	defer close(liveDone)
	commit := s.resolveCommit(ctx, j, runner)
	in, recorded := s.recordInput(ctx, j, runner, runID, trigger, actor, caseEnv, rec.StartedAt, commit)
	if recorded {
		s.enqueueShadows(ctx, j, in, liveDone)
	}
	env := s.runEnv(j, runID, id, spec.FleetdStage(autonomy), j.Budget.DayUSD-spent, carrier.Get("traceparent"))
	if pr := env["FLOTILLA_PROMPTS"]; pr != "" {
		if err := s.Store.Event(ctx, "flotillad", "flotillad", "run.prompts", runID, pr); err != nil {
			s.Log.Warn("the run's prompts event", "routine", id, "run_id", runID, "err", err)
		}
	}
	if commit.SHA != "" {
		env["FLOTILLA_COMMIT"], env["FLOTILLA_REPO"] = commit.SHA, commit.Repo
	}
	planFile := ""
	for k, v := range caseEnv {
		env[k] = v
	}
	if j.Resume != nil && caseEnv != nil {
		s.recordCaseRun(ctx, id, runID, caseEnv)
	}
	reports := caseEnv != nil || len(j.Detects) > 0 // an acting routine or a monitor answers in a plan file
	if reports {
		planFile = ".flotilla-plan-" + runID + ".json" // under the run's HOME; the worker reads and removes it
	}
	s.Log.Info("run start", "routine", id, "run_id", runID, "autonomy", autonomy, "trigger", trigger, "actor", actor, "trace_id", rec.TraceID)
	res, callErr := runner.Run(ctx, worker.Request{RunID: runID, Routine: id, Command: j.Run.Command, Workdir: j.Run.Workdir,
		TimeoutS: timeout.Seconds(), SpendFile: j.Run.SpendFile, PlanFile: planFile, Env: env, APIToken: apiToken})

	status, detail, spend, dur := store.StatusError, "", 0.0, 0.0
	var exit *int
	var outcome *CaseOutcome
	if callErr != nil {
		detail = callErr.Error()
		if errors.Is(callErr, worker.ErrUnreachable) {
			// the run never started (a deploy recreating the worker): a skip, not a failure
			status = store.StatusSkipped
		} else if from, err := time.Parse(store.TimeFormat, rec.StartedAt); err == nil {
			// a call lost mid-run (a reset) brings no duration: the time it ran is still the case's
			dur = max(s.Now().Sub(from).Seconds(), 0)
		}
	} else {
		status, exit, spend, dur = res.Status, res.ExitCode, res.SpendUSD, res.DurationS
		if j.Run.SpendFile != "" && res.SpendReported != nil && !*res.SpendReported {
			// a budget, the day's cap and a case's limit read this spend: an unreported one counts 0, say so
			s.Log.Warn("run reported no spend", "routine", id, "run_id", runID, "status", status)
		}
		if reports {
			outcome = &CaseOutcome{Plan: res.Plan, PlanError: res.PlanError}
		}
		if status == store.StatusCancelled {
			detail = s.cancelNote(runID, res.OutputTail)
		} else if status != store.StatusOK {
			detail = tail(res.OutputTail, 800)
		} else if reports && caseEnv == nil && res.PlanError != "" {
			// a monitor whose report cannot be read has detected nothing anyone sees: its run failed
			status, detail = store.StatusError, "the report was refused: "+res.PlanError
		}
	}
	// a cancelled flotillad must still record the run it ran
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	total, err := s.Store.EndRun(fctx, runID, status, exit, spend, dur, detail, day, id)
	if err != nil {
		return nil, nil, err
	}
	if callErr == nil && len(res.Calls) > 0 {
		calls := make([]store.ModelCall, len(res.Calls))
		for i, c := range res.Calls {
			at := s.Now().UTC()
			if t, err := time.Parse(time.RFC3339Nano, c.At); err == nil {
				at = t.UTC()
			}
			calls[i] = store.ModelCall{RunID: runID, Routine: id, At: at.Format(store.TimeFormat), Alias: c.Alias, Endpoint: c.Endpoint,
				Model: c.Model, Deployment: c.Deployment, TokensIn: c.TokensIn, TokensOut: c.TokensOut, CostUSD: c.CostUSD,
				CostSource: c.CostSource, LatencyS: c.LatencyS, Error: c.Error, Check: c.Check, CheckOK: c.CheckOK, Detail: c.Detail,
				FinishReason: c.FinishReason, Step: c.Step, DecisionKey: c.DecisionKey,
				Role: c.Role, Episode: c.Episode, EpisodeSeq: c.EpisodeSeq, Tools: store.NormalizeTools(c.Tools), Kind: c.Kind, Station: c.Station, TextBodies: callTexts(c.Texts)}
		}
		if res.CallsDropped > 0 {
			// the worker keeps the first lines of a run's calls file; say how many it did not, in the run's own rows
			calls = append(calls, store.ModelCall{RunID: runID, Routine: id, At: s.Now().UTC().Format(store.TimeFormat), Role: "harness",
				Kind: store.KindStep, Detail: fmt.Sprintf("%d more model calls were made and not recorded: the worker keeps the first %d lines of a run's calls file",
					res.CallsDropped, len(res.Calls))})
		}
		// the calls are a record of the run, not its outcome: a failure to keep them is logged, not fatal
		if err := s.Store.RecordCalls(fctx, calls); err != nil {
			s.Log.Error("recording the run's model calls", "run_id", runID, "err", err)
			s.StoreError("run_calls")
		}
	}
	if callErr == nil && len(res.Loaded) > 0 {
		items := make([]store.Loaded, len(res.Loaded))
		for i, l := range res.Loaded {
			at := s.Now()
			if t, err := time.Parse(time.RFC3339Nano, l.At); err == nil {
				at = t
			}
			items[i] = store.Loaded{RunID: runID, Routine: id, At: at.UTC().Format(store.TimeFormat), Kind: l.Kind, Trust: l.Trust,
				Title: l.Title, Source: l.Source, SHA256: l.SHA256}
		}
		if err := s.Store.RecordLoaded(fctx, items); err != nil {
			s.Log.Error("recording what the run read", "run_id", runID, "err", err)
			s.StoreError("run_loaded")
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
	s.Log.Log(ctx, lvl, "run end", "routine", id, "run_id", runID, "status", status, "duration_s", dur, "spend_usd", spend,
		"spend_day_usd", total, "detail", detail)
	if s.Metrics != nil {
		s.Metrics.Run(id, status, autonomy, dur, total)
		if status == store.StatusOK {
			s.Metrics.LastSuccess(id, s.Now())
		}
	}
	out := rec
	out.Status, out.ExitCode, out.SpendUSD, out.Detail = status, exit, spend, detail
	out.DurationS = &dur
	if caseEnv == nil && outcome != nil && s.AfterRun != nil {
		s.AfterRun(fctx, j, &out, outcome)
	}
	return &out, outcome, nil
}

// recordInput freezes what the run was handed (v2 step 2): the case, plan and tick time, the routine's ledger marks, the keys of
// the open cases of the kinds it detects, the commit it works from and, when a shadow version waits to be run, a snapshot of the
// routine's state, all as they are now, before the run can write. A shadow version of the routine is given exactly this. A failure
// to write it is logged and counted, never the run's: the live fleet does not stop for its shadows. ok is false then.
func (s *Scheduler) recordInput(ctx context.Context, j *spec.Routine, runner Runner, runID, trigger, actor string, caseEnv map[string]string, startedAt string, commit input.Commit) (store.Input, bool) {
	id := j.ID()
	in := store.Input{Key: runID, Routine: id, Kind: input.Kind(trigger, caseEnv != nil), Version: max(j.Version, 1), Trigger: trigger, Actor: actor,
		StartedAt: startedAt, Handed: input.Handed(caseEnv), CommitRepo: commit.Repo, CommitSHA: commit.SHA}
	var err error
	if in.MarkFilings, in.MarkEntries, err = s.Store.LedgerMark(ctx, id); err == nil && len(j.Detects) > 0 {
		in.LiveCaseKeys, err = s.Store.OpenCaseKeys(ctx, j.Detects)
	}
	if err == nil && s.snapshotWanted(ctx, j) {
		in.SnapshotPaths = j.State
		in.SnapshotState = s.takeSnapshot(ctx, j, runner, runID)
	}
	if err == nil {
		err = s.Store.NewInput(ctx, in)
	}
	if err != nil {
		s.Log.Warn("the run's input was not recorded; its shadows will not run", "routine", id, "run_id", runID, "err", err)
		// unmarked on purpose: flotillad's own store failing is not a shadow's signal, and must alert
		s.StoreError("run_input")
		return in, false
	}
	return in, true
}

// runEnv is the environment every run of a routine version gets from flotillad, live or shadow: the cycle contract, the run's
// identity (routineKey is the routine's id, or a shadow's "<id>@v<n>"), the models, skills, prompts and harness its spec names, and
// where its spans go. The case it was handed is added by the caller.
func (s *Scheduler) runEnv(j *spec.Routine, runID, routineKey, stage string, budgetLeft float64, traceparent string) map[string]string {
	env, err := RunContract(j, s.Models, EnvParams{RunID: runID, RoutineKey: routineKey, Stage: stage, BudgetLeft: budgetLeft, Traceparent: traceparent,
		OTLPTracesURL: s.OTLPTracesURL, Environment: s.Environment})
	if err != nil {
		s.Log.Error("the routine's models", "routine", routineKey, "err", err) // -check refuses this config
	}
	return env
}

// EnvParams is what varies between the runs of one routine version in the environment RunContract builds.
type EnvParams struct {
	RunID, RoutineKey, Stage string
	BudgetLeft               float64
	Traceparent              string
	OTLPTracesURL            string
	Environment              string
}

// RunContract is the environment a run of a routine version gets: the one function behind a live or shadow run (runEnv) and behind
// `flotillad -stations`, so an offline run gets exactly what flotillad would hand it. The error is the models subset's; the rest of the
// environment is returned with it.
func RunContract(j *spec.Routine, mc *models.Config, p EnvParams) (map[string]string, error) {
	runID, routineKey, stage, budgetLeft, traceparent := p.RunID, p.RoutineKey, p.Stage, p.BudgetLeft, p.Traceparent
	var modelsErr error
	env := map[string]string{
		"FLEETD_STAGE":       stage,
		"FLEETD_CYCLE_ID":    runID,
		"FLEETD_BUDGET_LEFT": fmt.Sprintf("%.4f", budgetLeft),
		"FLOTILLA_RUN_ID":    runID,
		"FLOTILLA_ROUTINE":   routineKey,
		// the routine's version the run is: the worker's logs and the routine's own spans and traces carry it (a shadow run also
		// gets FLOTILLA_SHADOW=1)
		"FLOTILLA_ROUTINE_VERSION": strconv.Itoa(max(j.Version, 1)),
	}
	if traceparent != "" {
		env["TRACEPARENT"] = traceparent
	}
	if mc != nil && len(j.Models) > 0 {
		sub, err := mc.Subset(j.Models)
		if err == nil {
			var js string
			if js, err = sub.JSON(); err == nil {
				env["FLOTILLA_MODELS"] = js
			}
		}
		modelsErr = err
	}
	if sk := j.SkillsJSON(); sk != "" {
		env["FLOTILLA_SKILLS"] = sk // the spec's skills.rule (ADR-0012), matched by the routine's code
	}
	if pr := j.PromptsJSON(); pr != "" {
		env["FLOTILLA_PROMPTS"] = pr // the spec's prompts: with the hashes their files had at load; a run refuses any other
	}
	if h := j.HarnessJSON(); h != "" {
		env["FLOTILLA_HARNESS"] = h // the spec's harness: (RFC-0002 §4)
	}
	if st := j.StationsJSON(); st != "" {
		env["FLOTILLA_STATIONS"] = st // the version's stations with their texts' hashes (v2-design.md section 2); only a version that declares some
	}
	if p.OTLPTracesURL != "" {
		env["FLOTILLA_OTLP_TRACES_URL"] = p.OTLPTracesURL
	}
	if p.Environment != "" {
		env["FLOTILLA_ENVIRONMENT"] = p.Environment
	}
	return env, modelsErr
}

// The case events of a routine that may resume (ADR-0019), and the env key its run reads. The store has no
// run-to-case link, so these events are the record: run.started says which run was the case's, run.resumed
// says that run was a resume of the one it names.
const (
	EventRunStarted = "run.started"
	EventRunResumed = "run.resumed"
	EnvResume       = "FLOTILLA_RESUME"
)

// Resumption is FLOTILLA_RESUME: the earlier run of the case the new one may continue, and the spend flotillad
// has on record for it (0 for an interrupted run), so the new run can report what the earlier one never did.
type Resumption struct {
	RunID    string  `json:"run_id"`
	Status   string  `json:"status"`
	SpendUSD float64 `json:"spend_usd"`
}

// ExitBusy is the exit code of a run that ended because another run of its case held the case's directory
// (bugfix/fix.py's lock, EX_TEMPFAIL): it never started its work, like a skipped run.
const ExitBusy = 75

// recordCaseRun puts the run on its case's timeline, and a resume's prior run beside it. A failure to write
// is logged: the run goes on, and only a later resume of it is lost.
func (s *Scheduler) recordCaseRun(ctx context.Context, routine, runID string, caseEnv map[string]string) {
	var c struct {
		ID string `json:"id"`
	}
	if json.Unmarshal([]byte(caseEnv["FLOTILLA_CASE"]), &c) != nil || c.ID == "" {
		return
	}
	if raw := caseEnv[EnvResume]; raw != "" {
		var r Resumption
		if json.Unmarshal([]byte(raw), &r) == nil && r.RunID != "" {
			if err := s.Store.AddCaseEvent(ctx, c.ID, EventRunResumed, routine, fmt.Sprintf("run %s resumes run %s (%s)", runID, r.RunID, r.Status)); err != nil {
				s.Log.Error("recording a resume", "case", c.ID, "run_id", runID, "err", err)
				s.StoreError("case_event")
			}
		}
	}
	if err := s.Store.AddCaseEvent(ctx, c.ID, EventRunStarted, routine, "run "+runID); err != nil {
		s.Log.Error("recording a case's run", "case", c.ID, "run_id", runID, "err", err)
		s.StoreError("case_event")
	}
}

// cancelNote is a cancelled run's detail: who asked and why, when flotillad was asked; else what the
// worker saw (someone cancelled at the worker itself).
func (s *Scheduler) cancelNote(runID, output string) string {
	s.mu.Lock()
	note := s.cancels[runID]
	s.mu.Unlock()
	if note != "" {
		return note
	}
	return "cancelled at the worker: " + tail(output, 700)
}

// Cancel stops routine id's run in flight and returns its run id. The run ends "cancelled" (detail
// "cancelled: <who>, <why>") when its worker reports it so, with its spend counted as for any run; a run
// that ends ok before the worker acts stays ok. ErrNotRunning when nothing of the routine is in flight.
// The first ask is sent at once; a worker that does not know the run yet (flotillad has it, the request is
// still on its way) is asked again in the background for a few seconds.
func (s *Scheduler) Cancel(ctx context.Context, id, who, why string) (string, error) {
	s.init()
	base, workerName := id, ""
	if store.IsShadowKey(id) {
		base = id[:strings.Index(id, "@v")] // a shadow version's run: the same cancel, on the shadow worker
	}
	j := s.Routine(base)
	if j == nil {
		return "", fmt.Errorf("no routine %s", id)
	}
	workerName = j.Worker
	if base != id {
		workerName = j.ShadowWorkerName()
	}
	s.mu.Lock()
	runID, busy := s.running[id]
	if busy {
		s.cancels[runID] = fmt.Sprintf("cancelled: %s, %s", who, why)
	}
	s.mu.Unlock()
	if !busy {
		return "", fmt.Errorf("%s: %w", id, ErrNotRunning)
	}
	c, ok := s.Workers[workerName].(Canceller)
	if !ok {
		return runID, fmt.Errorf("worker %s cannot cancel a run", workerName)
	}
	err := c.Cancel(ctx, runID)
	if errors.Is(err, worker.ErrNoSuchRun) {
		go s.retryCancel(context.WithoutCancel(ctx), c, id, runID)
		return runID, nil
	}
	return runID, err
}

func (s *Scheduler) retryCancel(ctx context.Context, c Canceller, id, runID string) {
	deadline := time.Now().Add(cancelRetryFor)
	for time.Now().Before(deadline) {
		time.Sleep(cancelRetryEvery)
		s.mu.Lock()
		still := s.running[id] == runID
		s.mu.Unlock()
		if !still {
			return
		}
		err := c.Cancel(ctx, runID)
		if err == nil {
			return
		}
		if !errors.Is(err, worker.ErrNoSuchRun) {
			s.Log.Error("cancel: the worker refused the retry", "routine", id, "run_id", runID, "err", err)
			return
		}
	}
	s.Log.Error("cancel: the worker never took the run", "routine", id, "run_id", runID)
}

// Autonomy is the autonomy a run starts with: record while the installation is frozen, else the routine's
// own (Configured). A run keeps what it started with, so a freeze does not reach a run in flight.
func (s *Scheduler) Autonomy(ctx context.Context, j *spec.Routine) (string, error) {
	set, err := s.Configured(ctx, j)
	if err != nil {
		return set, err
	}
	if frozen, err := s.Frozen(ctx); err != nil || frozen {
		return spec.AutonomyRecord, err
	}
	return set, nil
}

// Frozen reports whether the installation's freeze holds.
func (s *Scheduler) Frozen(ctx context.Context) (bool, error) {
	v, err := s.Store.Control(ctx, FreezeSubject, KeyFrozen)
	return v == "1", err
}

// Configured is the routine's own autonomy, what the freeze leaves alone: the control if set, else the
// spec's default. A control above the spec's max_autonomy (set before the cap existed, or written to the
// store by hand) is not obeyed: the routine runs at its cap.
func (s *Scheduler) Configured(ctx context.Context, j *spec.Routine) (string, error) {
	v, err := s.Store.Control(ctx, j.ID(), KeyAutonomy)
	if err != nil || v == "" {
		return j.Autonomy, err
	}
	if !j.Allows(v) {
		return j.MaxAutonomy, nil
	}
	return v, nil
}

// Held says whether the routine is held by an operator: its fleet STOPped or the routine paused.
func (s *Scheduler) Held(ctx context.Context, j *spec.Routine) (bool, error) {
	if v, err := s.Store.Control(ctx, j.Fleet, KeyStop); err != nil || v == "1" {
		return v == "1", err
	}
	v, err := s.Store.Control(ctx, j.ID(), KeyPaused)
	return v == "1", err
}

// CaseHold is casetimer.Hold: a case of this kind and stage is held while its acting routine's fleet is
// STOPped or the routine is paused; released, the later of the two controls' last change is when.
func (s *Scheduler) CaseHold(ctx context.Context, kind, stage string) (bool, string, error) {
	for _, j := range s.Routines() {
		if !j.Enabled || j.OnCase() != kind || j.OnStage() != stage {
			continue
		}
		sv, sat, err := s.Store.ControlAt(ctx, j.Fleet, KeyStop)
		if err != nil {
			return false, "", err
		}
		pv, pat, err := s.Store.ControlAt(ctx, j.ID(), KeyPaused)
		if err != nil {
			return false, "", err
		}
		if sv == "1" || pv == "1" {
			return true, "", nil
		}
		return false, max(sat, pat), nil // the store's time format sorts as text
	}
	return false, "", nil
}

// SkipReason is why a run starting now would be skipped ("" when it would run): what a dry run reports.
func (s *Scheduler) SkipReason(ctx context.Context, j *spec.Routine) (string, error) {
	return s.skipReason(ctx, j)
}

func (s *Scheduler) skipReason(ctx context.Context, j *spec.Routine) (string, error) {
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
	// a remote worker (RFC-0011) whose machine is off or unreachable: its routines wait
	if r, ok := s.Workers[j.Worker].(interface{ Available() bool }); ok && !r.Available() {
		return "worker " + j.Worker + " is not connected", nil
	}
	if j.Budget.DayUSD > 0 {
		day := s.Now().In(s.Location).Format("2006-01-02")
		spent, err := s.Store.Spend(ctx, day, j.ID())
		if err != nil {
			return "", err
		}
		if spent >= j.Budget.DayUSD {
			return fmt.Sprintf("daily budget reached ($%.4f of $%.2f)", spent, j.Budget.DayUSD), nil
		}
	}
	return "", nil
}

func (s *Scheduler) metric(j *spec.Routine, status, autonomy string, dur float64) {
	if s.Metrics != nil {
		s.Metrics.Run(j.ID(), status, autonomy, dur, 0)
	}
}

// Clock is the scheduler's time, safe to read before Loop has started: Now is filled in by init, and
// flotillad's heartbeat starts before Loop does.
func (s *Scheduler) Clock() time.Time {
	s.init()
	return s.Now()
}

// Running lists the routines with a run in flight.
func (s *Scheduler) Running() map[string]string {
	s.init()
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]string, len(s.running))
	for k, v := range s.running {
		out[k] = v
	}
	return out
}

// newShadowRunID is a shadow run's id: the live shape with the version, so it never equals a live run's.
func newShadowRunID(j *spec.Routine, version int, now time.Time) string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%s-%s-v%d-%s-%s", j.Fleet, j.Routine, version, now.UTC().Format("20060102T150405"), hex.EncodeToString(b))
}

func newRunID(j *spec.Routine, now time.Time) string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%s-%s-%s-%s", j.Fleet, j.Routine, now.UTC().Format("20060102T150405"), hex.EncodeToString(b))
}

func tail(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}

// callTexts is the bodies a call's rendered texts carry, as the store keeps them.
func callTexts(in []worker.CallText) []store.CallText {
	out := make([]store.CallText, len(in))
	for i, t := range in {
		out[i] = store.CallText{Name: t.Name, SHA256: t.SHA256, Bytes: t.Bytes, Text: t.Text, Redacted: t.Redacted, OverCap: t.OverCap}
	}
	return out
}
