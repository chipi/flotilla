// Package api is flotillad's HTTP API, the one surface the CLI (and later the UI and MCP) use.
//
// Every request but /healthz carries a bearer token: a caller's from the callers file (agents, workers,
// and the operator token kept for bootstrap and break-glass), or a person's (users.go, ADR-0014). What a
// role may do is one table, authorize.go:
//   - agent: read, start a run, pause a routine, STOP a fleet (making things safer is never gated);
//   - viewer: read; operator: also resume, start a STOPped fleet, set autonomy; admin: also users.
//
// An agent can never do what only an operator may (AGENTS.md rule 2). Loosening needs a reason. Every
// change is recorded in the event log with the caller's name (a person's is user:<name>).
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
	// RoleWorker is a worker's routine reading and writing its fleet's ledger (RFC-0002 §9 step 5), and
	// nothing else. Its callers are not in the callers file: flotillad makes one per worker from the token
	// it already holds for that worker (WorkerCallers).
	RoleWorker = "worker"
)

// WorkerCallers are the ledger's callers: one per worker, by the token flotillad shares with it.
func WorkerCallers(tokens map[string]string) []Caller {
	var out []Caller
	for name, tok := range tokens {
		out = append(out, Caller{Name: "worker:" + name, Role: RoleWorker, TokenSHA256: HashToken(tok)})
	}
	return out
}

type Caller struct {
	Name        string `yaml:"name"`
	Role        string `yaml:"role"`
	TokenSHA256 string `yaml:"token_sha256"`
	// User is a person from the store (ADR-0014), not a callers-file entry.
	User bool `yaml:"-"`
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
	m.Handle("GET /v1/status", s.auth(ActRead, s.status))
	m.Handle("GET /v1/runs", s.auth(ActRead, s.runs))
	m.Handle("GET /v1/events", s.auth(ActRead, s.events))
	m.Handle("POST /v1/routines/{fleet}/{routine}/run", s.auth(ActRun, s.run))
	m.Handle("POST /v1/routines/{fleet}/{routine}/pause", s.auth(ActPause, s.control(ActPause, sched.KeyPaused, "1", true)))
	m.Handle("POST /v1/routines/{fleet}/{routine}/resume", s.auth(ActResume, s.control(ActResume, sched.KeyPaused, "0", true)))
	m.Handle("POST /v1/routines/{fleet}/{routine}/autonomy", s.auth(ActAutonomy, s.autonomy))
	m.Handle("POST /v1/fleets/{fleet}/stop", s.auth(ActStop, s.control(ActStop, sched.KeyStop, "1", false)))
	m.Handle("POST /v1/fleets/{fleet}/start", s.auth(ActStart, s.control(ActStart, sched.KeyStop, "0", false)))
	s.userRoutes(m)
	s.ledgerRoutes(m)
	return m
}

// WorkerHandler serves the workers' listener (flotillad's worker_listen): the ledger and /healthz, and
// nothing else, so the full API can stay on loopback while a worker's routine reaches its ledger.
func (s *Server) WorkerHandler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"ok": true, "version": s.Version})
	})
	s.ledgerRoutes(m)
	return m
}

func (s *Server) ledgerRoutes(m *http.ServeMux) {
	m.Handle("GET /v1/ledger/filings", s.auth(ActLedger, s.ledgerFilings))
	m.Handle("POST /v1/ledger/filings", s.auth(ActLedger, s.ledgerWriteFiling))
	m.Handle("GET /v1/ledger/entries", s.auth(ActLedger, s.ledgerEntries))
	m.Handle("POST /v1/ledger/entries", s.auth(ActLedger, s.ledgerAppendEntry))
	m.Handle("GET /v1/ledger/counts", s.auth(ActLedger, s.ledgerCounts))
	m.Handle("POST /v1/ledger/import", s.auth(ActLedger, s.ledgerImport))
}

// whoIs resolves a bearer token: the callers file first, then the store's users. nil when unknown.
func (s *Server) whoIs(ctx context.Context, tok string) (*Caller, error) {
	got := HashToken(tok)
	var who *Caller
	for i := range s.Callers {
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.Callers[i].TokenSHA256)) == 1 {
			c := s.Callers[i]
			who = &c
		}
	}
	if who != nil {
		return who, nil
	}
	u, err := s.Store.UserByToken(ctx, got)
	if err != nil || u == nil {
		return nil, err
	}
	return &Caller{Name: "user:" + u.Name, Role: u.Role, User: true}, nil
}

// effectiveRole is the role the table is read with. The callers-file operator token is an operator only
// while no admin exists (so an installation works before its first user); after that it is retired:
// bootstrap and break-glass are the users endpoint's own check (users.go).
func (s *Server) effectiveRole(ctx context.Context, c Caller) (string, error) {
	if c.User || c.Role != RoleOperator {
		return c.Role, nil
	}
	n, err := s.Store.ActiveAdmins(ctx)
	if err != nil {
		return "", err
	}
	if n > 0 {
		return "retired-operator-token", nil
	}
	return RoleOperator, nil
}

func (s *Server) auth(a Action, h func(http.ResponseWriter, *http.Request, Caller)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || tok == "" {
			writeJSON(w, 401, map[string]string{"error": "a bearer token is required"})
			return
		}
		who, err := s.whoIs(r.Context(), tok)
		if err != nil {
			writeErr(w, err)
			return
		}
		if who == nil {
			writeJSON(w, 401, map[string]string{"error": "unknown token"})
			return
		}
		role, err := s.effectiveRole(r.Context(), *who)
		if err != nil {
			writeErr(w, err)
			return
		}
		// The users endpoints decide the callers-file operator's bootstrap themselves (users.go).
		if !Authorize(role, a) && !(a == ActUsers && !who.User && who.Role == RoleOperator) {
			s.Log.Warn("refused", "caller", who.Name, "role", role, "action", a, "path", r.URL.Path)
			msg := "your role may not do this"
			switch {
			case role == RoleWorker:
				msg = "the ledger is a worker's; a worker reaches nothing else"
			case a == ActLedger:
				msg = "the ledger is a worker's"
			case role == RoleAgent:
				msg = "only an operator may do this; an agent never can"
			case role == "retired-operator-token":
				msg = "an admin exists: the callers-file operator token is for bootstrap and break-glass only; use your user token"
			}
			writeJSON(w, 403, map[string]string{"error": msg})
			return
		}
		h(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, *who)), *who)
	})
}

// reason reads a loosening action's reason from the JSON body ({"reason": "..."}); "" when absent.
func reason(r *http.Request, w http.ResponseWriter) string {
	var body struct {
		Reason string `json:"reason"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body)
	return strings.TrimSpace(body.Reason)
}

type routineStatus struct {
	ID           string  `json:"id"`
	Description  string  `json:"description"`
	Enabled      bool    `json:"enabled"`
	Worker       string  `json:"worker"`
	Every        string  `json:"every"`
	Autonomy     string  `json:"autonomy"`
	SpecAutonomy string  `json:"spec_autonomy"`
	SpecHash     string  `json:"spec_hash"`
	Paused       bool    `json:"paused"`
	Stopped      bool    `json:"fleet_stopped"`
	Running      string  `json:"running,omitempty"`
	LastSuccess  string  `json:"last_success,omitempty"`
	Stale        bool    `json:"stale"`
	SpendToday   float64 `json:"spend_today_usd"`
	BudgetDay    float64 `json:"budget_day_usd"`
}

func (s *Server) status(w http.ResponseWriter, r *http.Request, _ Caller) {
	ctx := r.Context()
	running := s.Sched.Running()
	now := s.Sched.Now()
	day := now.In(s.Sched.Location).Format("2006-01-02")
	var routines []routineStatus
	for _, j := range s.Sched.Routines {
		st, err := s.Sched.Autonomy(ctx, j)
		if err != nil {
			writeErr(w, err)
			return
		}
		paused, _ := s.Store.Control(ctx, j.ID(), sched.KeyPaused)
		stopped, _ := s.Store.Control(ctx, j.Fleet, sched.KeyStop)
		last, _ := s.Store.LastSuccess(ctx, j.ID())
		spent, _ := s.Store.Spend(ctx, day, j.ID())
		js := routineStatus{ID: j.ID(), Description: j.Description, Enabled: j.Enabled, Worker: j.Worker, Every: j.Every.String(),
			Autonomy: st, SpecAutonomy: j.Autonomy, SpecHash: j.Hash, Paused: paused == "1", Stopped: stopped == "1",
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

func (s *Server) control(a Action, key, value string, perJob bool) func(http.ResponseWriter, *http.Request, Caller) {
	return func(w http.ResponseWriter, r *http.Request, c Caller) {
		why := ""
		if loosening(a) {
			if why = reason(r, w); why == "" {
				writeJSON(w, 400, map[string]string{"error": "loosening needs a reason: body {\"reason\": \"...\"}"})
				return
			}
		}
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
		if err := s.Store.SetControlBecause(r.Context(), subject, key, value, c.Name, "api", why); err != nil {
			writeErr(w, err)
			return
		}
		s.Log.Info("control set", "subject", subject, "key", key, "value", value, "caller", c.Name, "reason", why)
		writeJSON(w, 200, map[string]string{"subject": subject, key: value, "by": c.Name})
	}
}

func (s *Server) autonomy(w http.ResponseWriter, r *http.Request, c Caller) {
	j := s.routine(w, r)
	if j == nil {
		return
	}
	var body struct {
		Autonomy string `json:"autonomy"`
		Reason   string `json:"reason"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]string{"error": "body: {\"autonomy\": \"shadow|propose|auto\", \"reason\": \"...\"}"})
		return
	}
	body.Reason = strings.TrimSpace(body.Reason)
	switch body.Autonomy {
	case spec.AutonomyShadow, spec.AutonomyPropose, spec.AutonomyAuto:
	default:
		writeJSON(w, 400, map[string]string{"error": "autonomy must be shadow, propose or auto"})
		return
	}
	if !j.Allows(body.Autonomy) {
		writeJSON(w, 400, map[string]string{"error": fmt.Sprintf("%s is capped at %s by its spec (max_autonomy)", j.ID(), j.MaxAutonomy)})
		return
	}
	cur, err := s.Sched.Autonomy(r.Context(), j)
	if err != nil {
		writeErr(w, err)
		return
	}
	if spec.Rank(body.Autonomy) > spec.Rank(cur) && body.Reason == "" { // raising loosens (ADR-0014)
		writeJSON(w, 400, map[string]string{"error": fmt.Sprintf("raising %s from %s to %s needs a reason", j.ID(), cur, body.Autonomy)})
		return
	}
	if err := s.Store.SetControlBecause(r.Context(), j.ID(), sched.KeyAutonomy, body.Autonomy, c.Name, "api", body.Reason); err != nil {
		writeErr(w, err)
		return
	}
	s.Log.Info("autonomy set", "routine", j.ID(), "autonomy", body.Autonomy, "caller", c.Name, "reason", body.Reason)
	writeJSON(w, 200, map[string]string{"routine": j.ID(), "autonomy": body.Autonomy, "by": c.Name})
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
