// Package api is flotillad's HTTP API, the one surface the CLI (and later the UI and MCP) use.
//
// Every request but /healthz carries a caller's bearer token. A caller has a role:
//   - agent: read, start a run, pause a routine, STOP a fleet (making things safer is never gated);
//   - operator: also resume a routine, start a STOPped fleet, change a stage.
//
// An agent can never do what only the operator may (AGENTS.md rule 2). Every change is recorded in the
// event log with the caller's name.
package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/chipi/flotilla/internal/sched"
	"github.com/chipi/flotilla/internal/spec"
	"github.com/chipi/flotilla/internal/store"
	"github.com/chipi/flotilla/internal/worker"
)

const (
	RoleAgent    = "agent"
	RoleOperator = "operator"
)

type Caller struct {
	Name        string `yaml:"name"`
	Role        string `yaml:"role"`
	TokenSHA256 string `yaml:"token_sha256"`
}

type Callers struct {
	Callers []Caller `yaml:"callers"`
}

// LoadCallers reads the callers file. Tokens are stored as SHA-256 hashes, never in clear.
func LoadCallers(path string) ([]Caller, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	var c Callers
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	names := map[string]bool{}
	for _, x := range c.Callers {
		if x.Name == "" || names[x.Name] {
			return nil, fmt.Errorf("%s: caller names must be set and unique (%q)", path, x.Name)
		}
		names[x.Name] = true
		if x.Role != RoleAgent && x.Role != RoleOperator {
			return nil, fmt.Errorf("%s: caller %s: role %q, want agent or operator", path, x.Name, x.Role)
		}
		if b, err := hex.DecodeString(x.TokenSHA256); err != nil || len(b) != sha256.Size {
			return nil, fmt.Errorf("%s: caller %s: token_sha256 must be 64 hex characters", path, x.Name)
		}
	}
	if len(c.Callers) == 0 {
		return nil, fmt.Errorf("%s: no callers", path)
	}
	return c.Callers, nil
}

// HashToken is what goes in the callers file for a token.
func HashToken(token string) string {
	s := sha256.Sum256([]byte(token))
	return hex.EncodeToString(s[:])
}

type Server struct {
	Sched   *sched.Scheduler
	Store   *store.Store
	Workers map[string]*worker.Client
	Callers []Caller
	Version string
	Log     *slog.Logger
}

type ctxKey struct{}

func (s *Server) Handler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"ok": true, "version": s.Version})
	})
	m.Handle("GET /v1/status", s.auth(RoleAgent, s.status))
	m.Handle("GET /v1/runs", s.auth(RoleAgent, s.runs))
	m.Handle("GET /v1/events", s.auth(RoleAgent, s.events))
	m.Handle("POST /v1/routines/{fleet}/{routine}/run", s.auth(RoleAgent, s.run))
	m.Handle("POST /v1/routines/{fleet}/{routine}/pause", s.auth(RoleAgent, s.control(sched.KeyPaused, "1", true)))
	m.Handle("POST /v1/routines/{fleet}/{routine}/resume", s.auth(RoleOperator, s.control(sched.KeyPaused, "0", true)))
	m.Handle("POST /v1/routines/{fleet}/{routine}/stage", s.auth(RoleOperator, s.stage))
	m.Handle("POST /v1/fleets/{fleet}/stop", s.auth(RoleAgent, s.control(sched.KeyStop, "1", false)))
	m.Handle("POST /v1/fleets/{fleet}/start", s.auth(RoleOperator, s.control(sched.KeyStop, "0", false)))
	return m
}

func (s *Server) auth(need string, h func(http.ResponseWriter, *http.Request, Caller)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || tok == "" {
			writeJSON(w, 401, map[string]string{"error": "a bearer token is required"})
			return
		}
		got := HashToken(tok)
		var who *Caller
		for i := range s.Callers {
			if subtle.ConstantTimeCompare([]byte(got), []byte(s.Callers[i].TokenSHA256)) == 1 {
				who = &s.Callers[i]
			}
		}
		if who == nil {
			writeJSON(w, 401, map[string]string{"error": "unknown token"})
			return
		}
		if need == RoleOperator && who.Role != RoleOperator {
			s.Log.Warn("refused: operator-only", "caller", who.Name, "path", r.URL.Path)
			writeJSON(w, 403, map[string]string{"error": "only the operator may do this; an agent never can"})
			return
		}
		h(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, *who)), *who)
	})
}

type routineStatus struct {
	ID          string  `json:"id"`
	Description string  `json:"description"`
	Enabled     bool    `json:"enabled"`
	Worker      string  `json:"worker"`
	Every       string  `json:"every"`
	Stage       string  `json:"stage"`
	SpecStage   string  `json:"spec_stage"`
	SpecHash    string  `json:"spec_hash"`
	Paused      bool    `json:"paused"`
	Stopped     bool    `json:"fleet_stopped"`
	Running     string  `json:"running,omitempty"`
	LastSuccess string  `json:"last_success,omitempty"`
	Stale       bool    `json:"stale"`
	SpendToday  float64 `json:"spend_today_usd"`
	BudgetDay   float64 `json:"budget_day_usd"`
}

func (s *Server) status(w http.ResponseWriter, r *http.Request, _ Caller) {
	ctx := r.Context()
	running := s.Sched.Running()
	now := s.Sched.Now()
	day := now.In(s.Sched.Location).Format("2006-01-02")
	var routines []routineStatus
	for _, j := range s.Sched.Routines {
		st, err := s.Sched.Stage(ctx, j)
		if err != nil {
			writeErr(w, err)
			return
		}
		paused, _ := s.Store.Control(ctx, j.ID(), sched.KeyPaused)
		stopped, _ := s.Store.Control(ctx, j.Fleet, sched.KeyStop)
		last, _ := s.Store.LastSuccess(ctx, j.ID())
		spent, _ := s.Store.Spend(ctx, day, j.ID())
		js := routineStatus{ID: j.ID(), Description: j.Description, Enabled: j.Enabled, Worker: j.Worker, Every: j.Every.String(),
			Stage: st, SpecStage: j.Stage, SpecHash: j.Hash, Paused: paused == "1", Stopped: stopped == "1",
			Running: running[j.ID()], SpendToday: spent, BudgetDay: j.Budget.DayUSD,
			Stale: j.Enabled && (last.IsZero() || now.Sub(last) > j.Fresh)}
		if !last.IsZero() {
			js.LastSuccess = last.Format(time.RFC3339)
		}
		routines = append(routines, js)
	}
	workers := map[string]any{}
	for name, c := range s.Workers {
		if h, err := c.Health(ctx); err != nil {
			workers[name] = map[string]any{"ok": false, "error": err.Error()}
		} else {
			workers[name] = h
		}
	}
	writeJSON(w, 200, map[string]any{"version": s.Version, "routines": routines, "workers": workers})
}

func (s *Server) runs(w http.ResponseWriter, r *http.Request, _ Caller) {
	limit := 20
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 && v <= 500 {
		limit = v
	}
	runs, err := s.Store.Runs(r.Context(), r.URL.Query().Get("routine"), limit)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"runs": runs})
}

func (s *Server) events(w http.ResponseWriter, r *http.Request, _ Caller) {
	limit := 50
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 && v <= 1000 {
		limit = v
	}
	ev, err := s.Store.Events(r.Context(), limit)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"events": ev})
}

func (s *Server) routine(w http.ResponseWriter, r *http.Request) *spec.Routine {
	j := s.Sched.Routine(r.PathValue("fleet") + "/" + r.PathValue("routine"))
	if j == nil {
		writeJSON(w, 404, map[string]string{"error": "no such routine"})
	}
	return j
}

// run starts a run in the background and answers at once with the run id the CLI can follow.
func (s *Server) run(w http.ResponseWriter, r *http.Request, c Caller) {
	j := s.routine(w, r)
	if j == nil {
		return
	}
	if busy := s.Sched.Running()[j.ID()]; busy != "" {
		writeJSON(w, 409, map[string]string{"error": "already running", "run_id": busy})
		return
	}
	_ = s.Store.Event(r.Context(), c.Name, "api", "run.requested", j.ID(), "")
	go func() {
		ctx := context.WithoutCancel(r.Context())
		if _, err := s.Sched.RunNow(ctx, j.ID(), "manual", c.Name); err != nil {
			s.Log.Error("manual run failed", "routine", j.ID(), "caller", c.Name, "err", err)
		}
	}()
	writeJSON(w, 202, map[string]string{"started": j.ID(), "by": c.Name})
}

func (s *Server) control(key, value string, perJob bool) func(http.ResponseWriter, *http.Request, Caller) {
	return func(w http.ResponseWriter, r *http.Request, c Caller) {
		subject := r.PathValue("fleet")
		if perJob {
			j := s.routine(w, r)
			if j == nil {
				return
			}
			subject = j.ID()
		} else if !s.hasFleet(subject) {
			writeJSON(w, 404, map[string]string{"error": "no such fleet"})
			return
		}
		if err := s.Store.SetControl(r.Context(), subject, key, value, c.Name, "api"); err != nil {
			writeErr(w, err)
			return
		}
		s.Log.Info("control set", "subject", subject, "key", key, "value", value, "caller", c.Name)
		writeJSON(w, 200, map[string]string{"subject": subject, key: value, "by": c.Name})
	}
}

func (s *Server) stage(w http.ResponseWriter, r *http.Request, c Caller) {
	j := s.routine(w, r)
	if j == nil {
		return
	}
	var body struct {
		Stage string `json:"stage"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]string{"error": "body: {\"stage\": \"shadow|propose|live\"}"})
		return
	}
	switch body.Stage {
	case spec.StageShadow, spec.StagePropose, spec.StageLive:
	default:
		writeJSON(w, 400, map[string]string{"error": "stage must be shadow, propose or live"})
		return
	}
	if !j.Allows(body.Stage) {
		writeJSON(w, 400, map[string]string{"error": fmt.Sprintf("%s is capped at %s by its spec (max_stage)", j.ID(), j.MaxStage)})
		return
	}
	if err := s.Store.SetControl(r.Context(), j.ID(), sched.KeyStage, body.Stage, c.Name, "api"); err != nil {
		writeErr(w, err)
		return
	}
	s.Log.Info("stage set", "routine", j.ID(), "stage", body.Stage, "caller", c.Name)
	writeJSON(w, 200, map[string]string{"routine": j.ID(), "stage": body.Stage, "by": c.Name})
}

func (s *Server) hasFleet(f string) bool {
	for _, j := range s.Sched.Routines {
		if j.Fleet == f {
			return true
		}
	}
	return false
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	code := 500
	if errors.Is(err, context.Canceled) {
		code = 499
	}
	writeJSON(w, code, map[string]string{"error": err.Error()})
}
