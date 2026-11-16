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
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/chipi/flotilla/internal/models"
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

// Control keys. A fleet's "stop" skips every routine of the fleet; a routine's "paused" skips that routine; a
// routine's "autonomy" overrides the spec's default. The installation's "frozen" (ADR-0014 decision 8) runs
// every routine at shadow; it is kept on FreezeSubject, which no fleet or routine id can be.
const (
	KeyStop       = "stop"
	KeyPaused     = "paused"
	KeyAutonomy   = "autonomy"
	KeyFrozen     = "frozen"
	FreezeSubject = "*"
)

var ErrRunning = errors.New("already running")

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
	Routines  []*spec.Routine
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

	mu      sync.Mutex
	running map[string]string // routine id -> run id
	cancels map[string]string // run id -> "cancelled: <who>, <why>", while a cancel of it is pending
}

func (s *Scheduler) init() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running == nil {
		s.running = map[string]string{}
		s.cancels = map[string]string{}
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

// Routine returns the loaded spec for "fleet/routine".
func (s *Scheduler) Routine(id string) *spec.Routine {
	for _, j := range s.Routines {
		if j.ID() == id {
			return j
		}
	}
	return nil
}

// Loop runs every enabled routine on its trigger until ctx ends: when it is due (its last run that was
// not skipped, plus its interval; at once if it has none), then every interval. A restart, a deploy's
// included, so starts nothing that is not due: before (2026-11-16) every routine ran the moment flotillad
// started, while compose was recreating its workers and the egress proxies, and failed on their names or
// its sources; a weekly routine ran again at every deploy. A run due at start first waits for its worker
// to answer (ReadyWait).
func (s *Scheduler) Loop(ctx context.Context) {
	s.init()
	var wg sync.WaitGroup
	for _, j := range s.Routines {
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
	s.init()
	j := s.Routine(id)
	if j == nil {
		return nil, nil, fmt.Errorf("no routine %s", id)
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
	rec := store.Run{ID: runID, Routine: id, SpecHash: j.Hash, Trigger: trigger, Actor: actor, Autonomy: autonomy}
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
		attribute.String("flotilla.worker", j.Worker), attribute.String("flotilla.spec_hash", j.Hash)))
	defer span.End()
	carrier := propagation.MapCarrier{}
	propagation.TraceContext{}.Inject(ctx, carrier)

	rec.StartedAt = s.Now().UTC().Format(store.TimeFormat)
	rec.Status = store.StatusRunning
	rec.TraceID = span.SpanContext().TraceID().String()
	if err := s.Store.StartRun(ctx, rec); err != nil {
		return nil, nil, err
	}
	env := map[string]string{
		"FLEETD_STAGE":       spec.FleetdStage(autonomy),
		"FLEETD_CYCLE_ID":    runID,
		"FLEETD_BUDGET_LEFT": fmt.Sprintf("%.4f", j.Budget.DayUSD-spent),
		"FLOTILLA_RUN_ID":    runID,
		"FLOTILLA_ROUTINE":   id,
	}
	if tp := carrier.Get("traceparent"); tp != "" {
		env["TRACEPARENT"] = tp
	}
	if s.Models != nil && len(j.Models) > 0 {
		sub, err := s.Models.Subset(j.Models)
		if err == nil {
			var js string
			if js, err = sub.JSON(); err == nil {
				env["FLOTILLA_MODELS"] = js
			}
		}
		if err != nil {
			s.Log.Error("the routine's models", "routine", id, "err", err) // -check refuses this config
		}
	}
	if sk := j.SkillsJSON(); sk != "" {
		env["FLOTILLA_SKILLS"] = sk // the spec's skills.rule (ADR-0012), matched by the routine's code
	}
	if h := j.HarnessJSON(); h != "" {
		env["FLOTILLA_HARNESS"] = h // the spec's harness: (RFC-0002 §4)
	}
	if s.OTLPTracesURL != "" {
		env["FLOTILLA_OTLP_TRACES_URL"] = s.OTLPTracesURL
	}
	if s.Environment != "" {
		env["FLOTILLA_ENVIRONMENT"] = s.Environment
	}
	planFile := ""
	for k, v := range caseEnv {
		env[k] = v
	}
	reports := caseEnv != nil || len(j.Detects) > 0 // an acting routine or a monitor answers in a plan file
	if reports {
		planFile = ".flotilla-plan-" + runID + ".json" // under the run's HOME; the worker reads and removes it
	}
	s.Log.Info("run start", "routine", id, "run_id", runID, "autonomy", autonomy, "trigger", trigger, "actor", actor, "trace_id", rec.TraceID)
	res, callErr := runner.Run(ctx, worker.Request{RunID: runID, Routine: id, Command: j.Run.Command, Workdir: j.Run.Workdir,
		TimeoutS: j.Limit.Seconds(), SpendFile: j.Run.SpendFile, PlanFile: planFile, Env: env})

	status, detail, spend, dur := store.StatusError, "", 0.0, 0.0
	var exit *int
	var outcome *CaseOutcome
	if callErr != nil {
		detail = callErr.Error()
		if errors.Is(callErr, worker.ErrUnreachable) {
			// the run never started (a deploy recreating the worker): a skip, not a failure
			status = store.StatusSkipped
		}
	} else {
		status, exit, spend, dur = res.Status, res.ExitCode, res.SpendUSD, res.DurationS
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
	if err := s.Store.FinishRun(fctx, runID, status, exit, spend, dur, detail); err != nil {
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
				CostSource: c.CostSource, LatencyS: c.LatencyS, Error: c.Error, Check: c.Check, CheckOK: c.CheckOK, Detail: c.Detail}
		}
		// the calls are a record of the run, not its outcome: a failure to keep them is logged, not fatal
		if err := s.Store.RecordCalls(fctx, calls); err != nil {
			s.Log.Error("recording the run's model calls", "run_id", runID, "err", err)
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
		}
	}
	total := spent
	if spend > 0 {
		if total, err = s.Store.AddSpend(fctx, day, id, spend); err != nil {
			return nil, nil, err
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
	j := s.Routine(id)
	if j == nil {
		return "", fmt.Errorf("no routine %s", id)
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
	c, ok := s.Workers[j.Worker].(Canceller)
	if !ok {
		return runID, fmt.Errorf("worker %s cannot cancel a run", j.Worker)
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

// Autonomy is the autonomy a run starts with: shadow while the installation is frozen, else the routine's
// own (Configured). A run keeps what it started with, so a freeze does not reach a run in flight.
func (s *Scheduler) Autonomy(ctx context.Context, j *spec.Routine) (string, error) {
	set, err := s.Configured(ctx, j)
	if err != nil {
		return set, err
	}
	if frozen, err := s.Frozen(ctx); err != nil || frozen {
		return spec.AutonomyShadow, err
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

// CaseHold is casetimer.Hold: a case of this kind and stage is held while its acting routine's fleet is
// STOPped or the routine is paused; released, the later of the two controls' last change is when.
func (s *Scheduler) CaseHold(ctx context.Context, kind, stage string) (bool, string, error) {
	for _, j := range s.Routines {
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
