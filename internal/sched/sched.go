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

	"github.com/chipi/flotilla/internal/spec"
	"github.com/chipi/flotilla/internal/store"
	"github.com/chipi/flotilla/internal/worker"
)

// Runner is the worker client (an interface so the tests can stand in for it).
type Runner interface {
	Run(ctx context.Context, r worker.Request) (*worker.Result, error)
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

type Scheduler struct {
	Store    *store.Store
	Routines []*spec.Routine
	Workers  map[string]Runner
	Metrics  Metrics
	Location *time.Location
	Log      *slog.Logger
	Now      func() time.Time
	// the installation's validated models section, as JSON: every run gets it in FLOTILLA_MODELS
	ModelsJSON string
	// where a routine sends its own spans (flotillad's otlp_traces_url) and the environment label: every run
	// gets them as FLOTILLA_OTLP_TRACES_URL and FLOTILLA_ENVIRONMENT (runtime/telemetry.py)
	OTLPTracesURL string
	Environment   string
	AfterRun      AfterRun

	mu      sync.Mutex
	running map[string]string // routine id -> run id
}

func (s *Scheduler) init() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running == nil {
		s.running = map[string]string{}
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

// Loop runs every enabled routine on its trigger until ctx ends: once at start, then every interval.
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
		if k := j.OnCase(); k != "" {
			s.Log.Info("routine runs on cases, not on time", "routine", j.ID(), "kind", k)
			continue
		}
		wg.Add(1)
		go func(j *spec.Routine) {
			defer wg.Done()
			s.Log.Info("routine loop started", "routine", j.ID(), "every", j.Every.String(), "worker", j.Worker)
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
	defer func() { s.mu.Lock(); delete(s.running, id); s.mu.Unlock() }()

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
	if s.ModelsJSON != "" {
		env["FLOTILLA_MODELS"] = s.ModelsJSON
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
	} else {
		status, exit, spend, dur = res.Status, res.ExitCode, res.SpendUSD, res.DurationS
		if reports {
			outcome = &CaseOutcome{Plan: res.Plan, PlanError: res.PlanError}
		}
		if status != store.StatusOK {
			detail = tail(res.OutputTail, 800)
		}
	}
	// a cancelled flotillad must still record the run it ran
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := s.Store.FinishRun(fctx, runID, status, exit, spend, dur, detail); err != nil {
		return nil, nil, err
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
