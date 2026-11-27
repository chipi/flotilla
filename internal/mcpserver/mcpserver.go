// Package mcpserver is flotilla's MCP server (RFC-0008): other agents see and steer flotilla with the
// API's own functions, under the caller's own identity, so the API's authorisation decides every call.
// It never offers a tool that approves or loosens (security T3). Streamable HTTP, stateless, JSON
// responses, on the UI listener's /mcp, one bearer token per caller (callers.yaml, role agent).
package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/chipi/flotilla/internal/api"
	"github.com/chipi/flotilla/internal/cases"
	"github.com/chipi/flotilla/internal/store"
)

// PerMinute is how many tool calls one caller may make a minute.
const PerMinute = 60

// MaxRows bounds every list a tool returns.
const MaxRows = 50

// Never are tools the server must never offer: approving and loosening stay with people (T3). A test
// asserts none of them is registered.
var Never = []string{"approve", "reject", "promote", "rollback", "retire", "rerecord", "resume", "start", "unfreeze", "unhold", "autonomy", "users", "hosts_revoke", "enrol", "input", "input_export", "input_pin", "input_unpin", "answer", "acknowledge", "dismiss", "accept"}

type Server struct {
	API     *api.Server
	Version string

	mu      sync.Mutex
	window  map[string]*window
	servers map[string]*mcp.Server
}

type window struct {
	start time.Time
	n     int
}

type ctxCaller struct{}

// Handler serves /mcp: the bearer token names the caller, the rate limit applies, then the protocol.
func (s *Server) Handler() http.Handler {
	h := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		c, _ := r.Context().Value(ctxCaller{}).(api.Caller)
		return s.serverFor(c)
	}, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || tok == "" {
			http.Error(w, "a bearer token is required", http.StatusUnauthorized)
			return
		}
		who, err := s.API.WhoIs(r.Context(), tok)
		if err != nil || who == nil {
			http.Error(w, "unknown token", http.StatusUnauthorized)
			return
		}
		if !s.allow(who.Name) {
			http.Error(w, fmt.Sprintf("at most %d calls a minute", PerMinute), http.StatusTooManyRequests)
			return
		}
		h.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxCaller{}, *who)))
	})
}

func (s *Server) allow(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.window == nil {
		s.window = map[string]*window{}
	}
	now := time.Now()
	w := s.window[name]
	if w == nil || now.Sub(w.start) >= time.Minute {
		w = &window{start: now}
		s.window[name] = w
	}
	w.n++
	return w.n <= PerMinute
}

func (s *Server) serverFor(c api.Caller) *mcp.Server {
	if c.Name == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.servers == nil {
		s.servers = map[string]*mcp.Server{}
	}
	key := c.Name + "\x00" + c.Role
	if srv := s.servers[key]; srv != nil {
		return srv
	}
	srv := mcp.NewServer(&mcp.Implementation{Name: "flotilla", Version: s.Version}, nil)
	s.register(srv, c)
	s.servers[key] = srv
	return srv
}

// ToolNames is every tool the server registers, for the tests and the docs.
func ToolNames() []string {
	return (&Server{}).register(mcp.NewServer(&mcp.Implementation{Name: "flotilla"}, nil), api.Caller{})
}

// reg is one server being registered for one caller, and the tools it got.
type reg struct {
	srv   *mcp.Server
	s     *Server
	c     api.Caller
	names []string
}

func add[In, Out any](r *reg, name, desc string, h func(context.Context, In) (Out, error)) {
	r.names = append(r.names, name)
	s, c, srv := r.s, r.c, r.srv
	mcp.AddTool(srv, &mcp.Tool{Name: name, Description: desc}, func(ctx context.Context, _ *mcp.CallToolRequest, in In) (*mcp.CallToolResult, Out, error) {
		out, err := h(ctx, in)
		if s.API != nil && s.API.Store != nil {
			outcome := "ok"
			if err != nil {
				outcome = err.Error()
			}
			_ = s.API.Store.Event(context.WithoutCancel(ctx), c.Name, "mcp", "mcp."+name, "", outcome)
		}
		var zero Out
		if err != nil {
			return nil, zero, err
		}
		return nil, out, nil
	})
}

func rows(n int) int {
	if n <= 0 || n > MaxRows {
		return MaxRows
	}
	return n
}

// Inputs and outputs.

type none struct{}

type routineIn struct {
	Routine string `json:"routine" jsonschema:"the routine's id, fleet/routine, such as ops/backup-monitor"`
}

type idIn struct {
	ID string `json:"id" jsonschema:"the id"`
}

type listIn struct {
	Limit int `json:"limit,omitempty" jsonschema:"at most this many, up to 50"`
}

type runsIn struct {
	Routine  string `json:"routine,omitempty" jsonschema:"only this routine"`
	Problems bool   `json:"problems,omitempty" jsonschema:"only failed, timed-out or interrupted runs"`
	Limit    int    `json:"limit,omitempty" jsonschema:"at most this many, newest first, up to 50"`
}

type casesIn struct {
	Status string `json:"status,omitempty" jsonschema:"only this status, such as awaiting_approval or closed"`
	Open   bool   `json:"open,omitempty" jsonschema:"only cases not closed"`
	Limit  int    `json:"limit,omitempty" jsonschema:"at most this many, up to 50"`
}

type ledgerIn struct {
	Namespace string `json:"namespace,omitempty" jsonschema:"a ledger namespace, such as triage/cycle; empty lists the namespaces"`
	Limit     int    `json:"limit,omitempty" jsonschema:"at most this many decisions, newest first, up to 50"`
}

type holdIn struct {
	Scope   string `json:"scope" jsonschema:"what to hold: host:<name> or a case target such as host:host-01"`
	Minutes int    `json:"minutes" jsonschema:"how long, in minutes"`
	Reason  string `json:"reason" jsonschema:"why"`
}

type reasonIn struct {
	Reason string `json:"reason" jsonschema:"why"`
}

type fleetIn struct {
	Fleet string `json:"fleet" jsonschema:"the fleet, such as triage"`
}

type noteIn struct {
	Case string `json:"case" jsonschema:"the case the note comes from"`
	Text string `json:"text" jsonschema:"what agents should know; a person activates it in Knowledge before any agent reads it"`
}

type statusOut struct {
	Routines []api.RoutineStatus `json:"routines"`
	Workers  map[string]any      `json:"workers"`
	Frozen   bool                `json:"frozen"`
}

type runOut struct {
	Run    *store.Run        `json:"run"`
	Calls  []store.ModelCall `json:"model_calls"`
	Loaded []store.Loaded    `json:"read"`
}

type caseOut struct {
	Case   store.Case         `json:"case"`
	Events []store.CaseEvent  `json:"timeline"`
	Plans  []store.PlanRecord `json:"plans"`
}

type okOut struct {
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

func (s *Server) read(ctx context.Context, c api.Caller, a api.Action) error {
	return s.API.Check(ctx, c, a)
}

func (s *Server) register(srv *mcp.Server, c api.Caller) []string {
	r := &reg{srv: srv, s: s, c: c}
	add(r, "status", "Every routine's state (autonomy, paused, stale, spend today) and each worker's health.",
		func(ctx context.Context, _ none) (statusOut, error) {
			if err := s.read(ctx, c, api.ActRead); err != nil {
				return statusOut{}, err
			}
			rs, ws, err := s.API.Status(ctx)
			if err != nil {
				return statusOut{}, err
			}
			fr, err := s.API.FreezeState(ctx)
			return statusOut{Routines: rs, Workers: ws, Frozen: fr.Frozen}, err
		})
	add(r, "runs_list", "Runs, newest first.", func(ctx context.Context, in runsIn) ([]store.Run, error) {
		if err := s.read(ctx, c, api.ActRead); err != nil {
			return nil, err
		}
		rs, _, err := s.API.Store.RunsPage(ctx, store.RunQuery{Routine: in.Routine, Problems: in.Problems, Desc: true, Limit: rows(in.Limit)})
		return rs, err
	})
	add(r, "run_show", "One run: its record, the model calls it made and what it read into a model, with trust.",
		func(ctx context.Context, in idIn) (runOut, error) {
			if err := s.read(ctx, c, api.ActRead); err != nil {
				return runOut{}, err
			}
			r, err := s.API.Store.Run(ctx, in.ID)
			if err != nil || r == nil {
				return runOut{}, errors.Join(err, errNotFound(r == nil, "run "+in.ID))
			}
			calls, err := s.API.Store.RunCalls(ctx, []string{in.ID})
			if err != nil {
				return runOut{}, err
			}
			ld, err := s.API.Store.RunLoaded(ctx, []string{in.ID})
			return runOut{Run: r, Calls: calls[in.ID], Loaded: ld[in.ID]}, err
		})
	add(r, "cases_list", "Cases, the most recently changed first.", func(ctx context.Context, in casesIn) ([]store.Case, error) {
		if err := s.read(ctx, c, api.ActRead); err != nil {
			return nil, err
		}
		return s.API.Store.Cases(ctx, store.CaseFilter{Status: in.Status, Open: in.Open, Limit: rows(in.Limit)})
	})
	add(r, "case_show", "One case: its record, its timeline and its plans with policy's reasons.",
		func(ctx context.Context, in idIn) (caseOut, error) {
			if err := s.read(ctx, c, api.ActRead); err != nil {
				return caseOut{}, err
			}
			cs, err := s.API.Store.Case(ctx, in.ID)
			if err != nil {
				return caseOut{}, err
			}
			evs, err := s.API.Store.CaseEvents(ctx, in.ID)
			if err != nil {
				return caseOut{}, err
			}
			ps, err := s.API.Store.Plans(ctx, in.ID)
			return caseOut{Case: cs, Events: evs, Plans: ps}, err
		})
	add(r, "approvals_list", "Plans waiting for a person. Read only: no agent can approve or reject.",
		func(ctx context.Context, in listIn) ([]caseOut, error) {
			if err := s.read(ctx, c, api.ActRead); err != nil {
				return nil, err
			}
			cs, err := s.API.Store.Cases(ctx, store.CaseFilter{Status: cases.AwaitingApproval, Limit: rows(in.Limit)})
			if err != nil {
				return nil, err
			}
			out := []caseOut{}
			for _, x := range cs {
				ps, err := s.API.Store.Plans(ctx, x.ID)
				if err != nil {
					return nil, err
				}
				out = append(out, caseOut{Case: x, Plans: ps})
			}
			return out, nil
		})
	add(r, "hosts_list", "Hosts, their agents' state and what their configs allow.", func(ctx context.Context, _ none) ([]store.Host, error) {
		if err := s.read(ctx, c, api.ActRead); err != nil {
			return nil, err
		}
		return s.API.Store.Hosts(ctx)
	})
	add(r, "models_calls", "The last day's model calls and contract checks, newest first.",
		func(ctx context.Context, in listIn) ([]store.ModelCall, error) {
			if err := s.read(ctx, c, api.ActRead); err != nil {
				return nil, err
			}
			cs, err := s.API.Store.CallsSince(ctx, time.Now().Add(-24*time.Hour).UTC().Format(time.RFC3339))
			if err != nil {
				return nil, err
			}
			n := rows(in.Limit)
			out := []store.ModelCall{}
			for i := len(cs) - 1; i >= 0 && len(out) < n; i-- {
				out = append(out, cs[i])
			}
			return out, nil
		})
	add(r, "brief_show", "The newest morning brief, as it was written.", func(ctx context.Context, _ none) (*store.Brief, error) {
		if err := s.read(ctx, c, api.ActRead); err != nil {
			return nil, err
		}
		bs, err := s.API.Store.Briefs(ctx, 1)
		if err != nil || len(bs) == 0 {
			return nil, errors.Join(err, errNotFound(len(bs) == 0, "a brief"))
		}
		return &bs[0], nil
	})
	add(r, "ledger_query", "The decision ledger: without a namespace, the namespaces; with one, its decisions, newest first.",
		func(ctx context.Context, in ledgerIn) (any, error) {
			if err := s.read(ctx, c, api.ActLedgerRead); err != nil {
				return nil, err
			}
			if in.Namespace == "" {
				return s.API.Store.Namespaces(ctx)
			}
			return s.API.Store.RecentDecisions(ctx, in.Namespace, 0, rows(in.Limit))
		})

	// Actions: each through the API function the CLI and the UI use, which records it with via=mcp.
	add(r, "routine_run", "Start a run of a routine now.", func(ctx context.Context, in routineIn) (okOut, error) {
		if err := s.API.RunNow(ctx, c, "mcp", in.Routine); err != nil {
			return okOut{}, err
		}
		return okOut{OK: true, Detail: "started " + in.Routine}, nil
	})
	add(r, "routine_dry_run", "What a run of the routine starting now would be, and whether it would start; nothing starts.",
		func(ctx context.Context, in routineIn) (api.DryRun, error) {
			return s.API.DryRunNow(ctx, c, "mcp", in.Routine)
		})
	add(r, "routine_pause", "Pause a routine (only a person resumes it).", func(ctx context.Context, in routineIn) (okOut, error) {
		if err := s.API.SetPaused(ctx, c, "mcp", in.Routine, true, ""); err != nil {
			return okOut{}, err
		}
		return okOut{OK: true, Detail: "paused " + in.Routine}, nil
	})
	add(r, "fleet_stop", "STOP a fleet: none of its routines start (only a person starts it again).",
		func(ctx context.Context, in fleetIn) (okOut, error) {
			if err := s.API.SetStopped(ctx, c, "mcp", in.Fleet, true, ""); err != nil {
				return okOut{}, err
			}
			return okOut{OK: true, Detail: "stopped " + in.Fleet}, nil
		})
	add(r, "freeze", "Freeze all autonomy: every routine at record (only a person unfreezes).", func(ctx context.Context, in reasonIn) (okOut, error) {
		if err := s.API.SetFrozen(ctx, c, "mcp", true, in.Reason); err != nil {
			return okOut{}, err
		}
		return okOut{OK: true, Detail: "frozen"}, nil
	})
	add(r, "hold", "Hold a host or a target for some minutes: nothing runs on it (only a person lifts it early).",
		func(ctx context.Context, in holdIn) (okOut, error) {
			id, err := s.API.AddHold(ctx, c, "mcp", in.Scope, time.Duration(in.Minutes)*time.Minute, in.Reason)
			if err != nil {
				return okOut{}, err
			}
			return okOut{OK: true, Detail: fmt.Sprintf("hold %d on %s", id, in.Scope)}, nil
		})
	add(r, "note_propose", "Propose a note from a case for agents to know; it stays proposed until a person activates it.",
		func(ctx context.Context, in noteIn) (okOut, error) {
			id, err := s.API.ProposeNote(ctx, c, "mcp", in.Case, in.Text)
			if err != nil {
				return okOut{}, err
			}
			return okOut{OK: true, Detail: fmt.Sprintf("note %d proposed", id)}, nil
		})
	return r.names
}

func errNotFound(missing bool, what string) error {
	if missing {
		return errors.New("no " + what)
	}
	return nil
}
