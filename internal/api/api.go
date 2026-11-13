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
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/chipi/flotilla/internal/hostca"
	"github.com/chipi/flotilla/internal/policy"
	"github.com/chipi/flotilla/internal/registry"
	"github.com/chipi/flotilla/internal/sched"
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
	// The registry policy reads (ADR-0011) and the guardrails' numbers (RFC-0004 §5.3); a nil registry
	// refuses every plan.
	Registry *registry.Registry
	Limits   policy.Limits
	// The host agents' intermediate CA and the root over it (ADR-0006 §3); nil: no host can enrol.
	HostCA   *hostca.CA
	HostRoot *x509.Certificate
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
	m.Handle("GET /v1/cases", s.auth(ActRead, s.cases))
	m.Handle("GET /v1/cases/{id}", s.auth(ActRead, s.caseShow))
	m.Handle("GET /v1/actions", s.auth(ActRead, s.actionsList))
	m.Handle("POST /v1/actions/{name}/autonomy", s.auth(ActAutonomy, s.actionAutonomy))
	m.Handle("GET /v1/holds", s.auth(ActRead, s.holds))
	m.Handle("POST /v1/holds", s.auth(ActHold, s.holdAdd))
	m.Handle("POST /v1/holds/{id}/lift", s.auth(ActUnhold, s.holdLift))
	m.Handle("POST /v1/routines/{fleet}/{routine}/run", s.auth(ActRun, s.run))
	m.Handle("POST /v1/routines/{fleet}/{routine}/pause", s.auth(ActPause, s.pause))
	m.Handle("POST /v1/routines/{fleet}/{routine}/resume", s.auth(ActResume, s.resume))
	m.Handle("POST /v1/routines/{fleet}/{routine}/autonomy", s.auth(ActAutonomy, s.autonomy))
	m.Handle("POST /v1/fleets/{fleet}/stop", s.auth(ActStop, s.stopFleet))
	m.Handle("POST /v1/fleets/{fleet}/start", s.auth(ActStart, s.startFleet))
	m.Handle("POST /v1/freeze", s.auth(ActFreeze, s.freeze))
	m.Handle("POST /v1/unfreeze", s.auth(ActUnfreeze, s.unfreeze))
	s.userRoutes(m)
	s.hostRoutes(m)
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

// WhoIs resolves a bearer token: the callers file first, then the store's users. nil when unknown.
func (s *Server) WhoIs(ctx context.Context, tok string) (*Caller, error) {
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

// EffectiveRole is the role the table is read with. The callers-file operator token is an operator only
// while no admin exists (so an installation works before its first user); after that it is retired:
// bootstrap and break-glass are the users endpoint's own check (users.go).
func (s *Server) EffectiveRole(ctx context.Context, c Caller) (string, error) {
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
		who, err := s.WhoIs(r.Context(), tok)
		if err != nil {
			writeErr(w, err)
			return
		}
		if who == nil {
			writeJSON(w, 401, map[string]string{"error": "unknown token"})
			return
		}
		// The users endpoints decide the callers-file operator's bootstrap themselves (users.go).
		if !(a == ActUsers && !who.User && who.Role == RoleOperator) {
			if err := s.Check(r.Context(), *who, a); err != nil {
				writeAction(w, err, 0, nil)
				return
			}
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

func (s *Server) status(w http.ResponseWriter, r *http.Request, _ Caller) {
	routines, workers, err := s.Status(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	fr, err := s.FreezeState(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"version": s.Version, "freeze": fr, "routines": routines, "workers": workers})
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

// cases lists cases, the most recently changed first: open ones unless all=1; status and kind narrow it.
func (s *Server) cases(w http.ResponseWriter, r *http.Request, _ Caller) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	cs, err := s.Store.Cases(r.Context(), store.CaseFilter{Open: q.Get("all") != "1", Status: q.Get("status"), Kind: q.Get("kind"), Limit: limit})
	if err != nil {
		writeErr(w, err)
		return
	}
	if cs == nil {
		cs = []store.Case{} // a list, never null, for every client
	}
	writeJSON(w, 200, map[string]any{"cases": cs})
}

// caseShow is one case and its whole timeline: "flotilla case show <id> is the whole story" (ADR-0005).
func (s *Server) caseShow(w http.ResponseWriter, r *http.Request, _ Caller) {
	c, err := s.Store.Case(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNoSuchCase) {
		writeJSON(w, 404, map[string]string{"error": "no such case"})
		return
	}
	if err != nil {
		writeErr(w, err)
		return
	}
	ev, err := s.Store.CaseEvents(r.Context(), c.ID)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"case": c, "events": ev})
}

func routineID(r *http.Request) string { return r.PathValue("fleet") + "/" + r.PathValue("routine") }

// writeAction answers an action's result: a Problem's code and message, or ok with the body given.
func writeAction(w http.ResponseWriter, err error, code int, ok map[string]string) {
	if p := AsProblem(err); p != nil {
		writeJSON(w, p.Code, map[string]string{"error": p.Msg})
		return
	}
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, code, ok)
}

// run starts a run in the background and answers at once.
func (s *Server) run(w http.ResponseWriter, r *http.Request, c Caller) {
	err := s.RunNow(r.Context(), c, "api", routineID(r))
	writeAction(w, err, 202, map[string]string{"started": routineID(r), "by": c.Name})
}

func (s *Server) pause(w http.ResponseWriter, r *http.Request, c Caller) {
	err := s.SetPaused(r.Context(), c, "api", routineID(r), true, "")
	writeAction(w, err, 200, map[string]string{"subject": routineID(r), sched.KeyPaused: "1", "by": c.Name})
}

func (s *Server) resume(w http.ResponseWriter, r *http.Request, c Caller) {
	err := s.SetPaused(r.Context(), c, "api", routineID(r), false, reason(r, w))
	writeAction(w, err, 200, map[string]string{"subject": routineID(r), sched.KeyPaused: "0", "by": c.Name})
}

func (s *Server) stopFleet(w http.ResponseWriter, r *http.Request, c Caller) {
	err := s.SetStopped(r.Context(), c, "api", r.PathValue("fleet"), true, "")
	writeAction(w, err, 200, map[string]string{"subject": r.PathValue("fleet"), sched.KeyStop: "1", "by": c.Name})
}

func (s *Server) startFleet(w http.ResponseWriter, r *http.Request, c Caller) {
	err := s.SetStopped(r.Context(), c, "api", r.PathValue("fleet"), false, reason(r, w))
	writeAction(w, err, 200, map[string]string{"subject": r.PathValue("fleet"), sched.KeyStop: "0", "by": c.Name})
}

func (s *Server) freeze(w http.ResponseWriter, r *http.Request, c Caller) {
	err := s.SetFrozen(r.Context(), c, "api", true, "")
	writeAction(w, err, 200, map[string]string{"subject": sched.FreezeSubject, sched.KeyFrozen: "1", "by": c.Name})
}

func (s *Server) unfreeze(w http.ResponseWriter, r *http.Request, c Caller) {
	err := s.SetFrozen(r.Context(), c, "api", false, reason(r, w))
	writeAction(w, err, 200, map[string]string{"subject": sched.FreezeSubject, sched.KeyFrozen: "0", "by": c.Name})
}

func (s *Server) autonomy(w http.ResponseWriter, r *http.Request, c Caller) {
	var body struct {
		Autonomy string `json:"autonomy"`
		Reason   string `json:"reason"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]string{"error": "body: {\"autonomy\": \"shadow|propose|auto\", \"reason\": \"...\"}"})
		return
	}
	err := s.SetAutonomy(r.Context(), c, "api", routineID(r), body.Autonomy, body.Reason)
	writeAction(w, err, 200, map[string]string{"routine": routineID(r), "autonomy": body.Autonomy, "by": c.Name})
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
