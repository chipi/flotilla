package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/chipi/flotilla/internal/sched"
	"github.com/chipi/flotilla/internal/spec"
	"github.com/chipi/flotilla/internal/store"
)

// Routine versions (docs/wip/v2-design.md section 1): list, promote, roll back and retire. Only an operator
// may change a version's state (ActVersion), never an agent and never through MCP: which version acts is the
// operator's word. Each action is one function the HTTP handlers, the CLI's endpoints and the UI all call.

func (s *Server) versionRoutes(m *http.ServeMux) {
	m.Handle("GET /v1/routines/{fleet}/{routine}/versions", s.auth(ActRead, s.versionsList))
	m.Handle("POST /v1/routines/{fleet}/{routine}/versions/{n}/promote", s.auth(ActVersion, s.versionPromote))
	m.Handle("POST /v1/routines/{fleet}/{routine}/versions/{n}/retire", s.auth(ActVersion, s.versionRetire))
	m.Handle("POST /v1/routines/{fleet}/{routine}/rollback", s.auth(ActVersion, s.versionRollback))
}

// VersionInfo is a recorded version with what the loaded fleets folder says about it.
type VersionInfo struct {
	store.RoutineVersion
	Loaded bool   `json:"loaded"`          // its folder is in the fleets folder flotillad loaded
	Error  string `json:"error,omitempty"` // why it does not load
	Pairs  int    `json:"pairs"`           // compared pairs: 0 until shadows record (build step 2)
}

// VersionsView is a routine's versions, oldest number first, with the acceptance a promote needs.
type VersionsView struct {
	Routine    string        `json:"routine"`
	Acceptance []string      `json:"acceptance"`
	Live       int           `json:"live"`
	Versions   []VersionInfo `json:"versions"`
}

// RoutineVersions reads one routine's versions.
func (s *Server) RoutineVersions(ctx context.Context, routineID string) (VersionsView, error) {
	j, err := s.routineByID(routineID)
	if err != nil {
		return VersionsView{}, err
	}
	rows, err := s.Store.Versions(ctx, routineID)
	if err != nil {
		return VersionsView{}, err
	}
	v := VersionsView{Routine: routineID, Acceptance: j.Accepts(), Versions: []VersionInfo{}}
	for _, r := range rows {
		vi := VersionInfo{RoutineVersion: r}
		if lv := j.VersionNamed(r.Version); lv != nil {
			vi.Loaded = true
			if lv.Err != nil {
				vi.Error = lv.Err.Error()
			}
		}
		if vi.Pairs, err = s.Store.PairsFor(ctx, routineID, r.Version); err != nil {
			return v, err
		}
		if r.State == store.VersionLive {
			v.Live = r.Version
		}
		v.Versions = append(v.Versions, vi)
	}
	return v, nil
}

// storeProblem maps a version refusal of the store to the HTTP status that says it.
func storeProblem(err error) error {
	switch {
	case errors.Is(err, store.ErrNoSuchVersion):
		return problem(404, "%s", err)
	case errors.Is(err, store.ErrVersionConflict):
		return problem(409, "%s", err)
	case errors.Is(err, store.ErrVersionInvalid):
		return problem(400, "%s", err)
	}
	return err
}

// PromoteRequest is what an operator gives to promote a shadow: the autonomy chosen for the routine
// (required: it is never inherited in silence), a reason, and the evidence the routine's acceptance names.
// flotilla stores the verdicts; it never computes them.
type PromoteRequest struct {
	Autonomy     string          `json:"autonomy"`
	Reason       string          `json:"reason"`
	WithoutPairs bool            `json:"without_pairs"`
	Gates        []store.Gate    `json:"gates"`
	Evals        []store.EvalRun `json:"evals"`
}

// PromoteVersion makes shadow n of a routine live. Refused for a disabled routine or one whose worker is
// not known, for a version whose folder does not load, for an autonomy above the spec's cap, and by the
// store for the evidence rules (a declared kind missing or a failed result, without a reason).
func (s *Server) PromoteVersion(ctx context.Context, c Caller, via, routineID string, n int, req PromoteRequest) error {
	if err := s.Check(ctx, c, ActVersion); err != nil {
		return err
	}
	j, err := s.routineByID(routineID)
	if err != nil {
		return err
	}
	if !j.Enabled {
		return problem(409, "%s is disabled: a version of a disabled routine is not promoted", routineID)
	}
	if _, ok := s.Sched.Workers[j.Worker]; !ok {
		return problem(409, "%s runs on worker %q, which flotillad does not know", routineID, j.Worker)
	}
	v := j.VersionNamed(n)
	if v == nil {
		return problem(404, "%s has no version v%d in the fleets folder", routineID, n)
	}
	if v.Err != nil {
		return problem(409, "%s v%d does not load: %v", routineID, n, v.Err)
	}
	switch req.Autonomy {
	case spec.AutonomyRecord, spec.AutonomyPropose, spec.AutonomyAuto:
	default:
		return problem(400, "promoting needs the routine's autonomy: record, propose or auto (pre-filled with what it runs at now)")
	}
	if !j.Allows(req.Autonomy) {
		return problem(400, "%s is capped at %s by its spec (max_autonomy)", routineID, j.MaxAutonomy)
	}
	cur, err := s.Sched.Configured(ctx, j)
	if err != nil {
		return err
	}
	reason := strings.TrimSpace(req.Reason)
	if err := s.Store.Promote(ctx, store.PromoteArgs{Routine: routineID, Version: n, Hash: v.Hash, Autonomy: req.Autonomy, Current: cur,
		Actor: c.Name, Via: via, Reason: reason, WithoutPairs: req.WithoutPairs, Acceptance: j.Accepts(),
		Evidence: store.Evidence{Gates: req.Gates, Evals: req.Evals}}); err != nil {
		return storeProblem(err)
	}
	if err := s.Sched.Bind(routineID, n); err != nil {
		return err
	}
	s.Log.Info("version promoted", "routine", routineID, "version", n, "autonomy", req.Autonomy, "caller", c.Name, "via", via, "reason", reason)
	return nil
}

// RollbackRequest is what an operator gives to roll back: the autonomy for the version that comes back
// (required, pre-filled) and a reason.
type RollbackRequest struct {
	Autonomy string `json:"autonomy"`
	Reason   string `json:"reason"`
}

// RollbackResult is what a rollback did.
type RollbackResult struct {
	RolledBack int      `json:"rolled_back"`
	Live       int      `json:"live"`
	Cases      []string `json:"cases,omitempty"`     // plans withdrawn: each case is back with the acting routine
	Cancelled  []string `json:"cancelled,omitempty"` // runs of the rolled-back version cancelled in flight
}

// RollbackVersion makes the previous version live at once. The store marks the rolled-back version's plans
// awaiting approval and returns their cases to the acting routine; here its runs in flight are cancelled, as a
// STOP cancels them, and the routine is bound to the version that is live again.
func (s *Server) RollbackVersion(ctx context.Context, c Caller, via, routineID string, req RollbackRequest) (RollbackResult, error) {
	var out RollbackResult
	if err := s.Check(ctx, c, ActVersion); err != nil {
		return out, err
	}
	j, err := s.routineByID(routineID)
	if err != nil {
		return out, err
	}
	reason := strings.TrimSpace(req.Reason)
	if reason == "" {
		return out, problem(400, "a rollback needs a reason")
	}
	switch req.Autonomy {
	case spec.AutonomyRecord, spec.AutonomyPropose, spec.AutonomyAuto:
	default:
		return out, problem(400, "a rollback needs the routine's autonomy: record, propose or auto (pre-filled with what it runs at now)")
	}
	if !j.Allows(req.Autonomy) {
		return out, problem(400, "%s is capped at %s by its spec (max_autonomy)", routineID, j.MaxAutonomy)
	}
	rows, err := s.Store.Versions(ctx, routineID)
	if err != nil {
		return out, err
	}
	prev := 0
	for _, r := range rows {
		if r.State == store.VersionPrevious {
			prev = r.Version
		}
	}
	if prev == 0 {
		return out, problem(409, "%s has no previous version to roll back to", routineID)
	}
	pv := j.VersionNamed(prev)
	if pv == nil || pv.Err != nil {
		return out, problem(409, "%s v%d, the previous version, is not in the fleets folder or does not load", routineID, prev)
	}
	cur, err := s.Sched.Configured(ctx, j)
	if err != nil {
		return out, err
	}
	res, err := s.Store.Rollback(ctx, store.RollbackArgs{Routine: routineID, Hash: pv.Hash, Autonomy: req.Autonomy, Current: cur,
		Actor: c.Name, Via: via, Reason: reason})
	if err != nil {
		return out, storeProblem(err)
	}
	if err := s.Sched.Bind(routineID, res.Restored); err != nil {
		return out, err
	}
	out = RollbackResult{RolledBack: res.RolledBack, Live: res.Restored, Cases: res.Cases}
	running := s.Sched.Running()[routineID]
	for _, id := range res.RunsInFlight {
		if id != running {
			continue
		}
		if err := s.cancelRun(ctx, c, via, routineID, fmt.Sprintf("v%d rolled back", res.RolledBack)); err != nil && !errors.Is(err, sched.ErrNotRunning) {
			s.Log.Error("rollback: the run in flight was not cancelled", "routine", routineID, "run", id, "caller", c.Name, "err", err)
		}
		out.Cancelled = append(out.Cancelled, id)
	}
	s.Log.Info("version rolled back", "routine", routineID, "rolled_back", res.RolledBack, "live", res.Restored, "caller", c.Name, "via", via, "reason", reason)
	return out, nil
}

// RetireVersion stops a shadow. A live or previous version is moved by promote or roll back, never retired.
func (s *Server) RetireVersion(ctx context.Context, c Caller, via, routineID string, n int, why string) error {
	if err := s.Check(ctx, c, ActVersion); err != nil {
		return err
	}
	if _, err := s.routineByID(routineID); err != nil {
		return err
	}
	if err := s.Store.Retire(ctx, routineID, n, c.Name, via, strings.TrimSpace(why)); err != nil {
		return storeProblem(err)
	}
	s.Log.Info("version retired", "routine", routineID, "version", n, "caller", c.Name, "via", via, "reason", why)
	return nil
}

func versionNumber(r *http.Request) (int, error) {
	n, err := strconv.Atoi(r.PathValue("n"))
	if err != nil || n < 1 {
		return 0, problem(400, "the version is a number: 1, 2, ...")
	}
	return n, nil
}

func (s *Server) versionsList(w http.ResponseWriter, r *http.Request, _ Caller) {
	v, err := s.RoutineVersions(r.Context(), routineID(r))
	if err != nil {
		writeAction(w, err, 200, nil)
		return
	}
	writeJSON(w, 200, v)
}

func (s *Server) versionPromote(w http.ResponseWriter, r *http.Request, c Caller) {
	n, err := versionNumber(r)
	if err != nil {
		writeAction(w, err, 200, nil)
		return
	}
	var body PromoteRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16384)).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]string{"error": `body: {"autonomy": "record|propose|auto", "reason": "...", "without_pairs": false, "gates": [{"name": "...", "result": "pass|fail", "ref": "..."}], "evals": [{"run_id": "...", "verdict": "pass|fail"}]}`})
		return
	}
	err = s.PromoteVersion(r.Context(), c, "api", routineID(r), n, body)
	writeAction(w, err, 200, map[string]string{"routine": routineID(r), "version": strconv.Itoa(n), "state": store.VersionLive, "autonomy": body.Autonomy, "by": c.Name})
}

func (s *Server) versionRetire(w http.ResponseWriter, r *http.Request, c Caller) {
	n, err := versionNumber(r)
	if err != nil {
		writeAction(w, err, 200, nil)
		return
	}
	err = s.RetireVersion(r.Context(), c, "api", routineID(r), n, reason(r, w))
	writeAction(w, err, 200, map[string]string{"routine": routineID(r), "version": strconv.Itoa(n), "state": store.VersionRetired, "by": c.Name})
}

func (s *Server) versionRollback(w http.ResponseWriter, r *http.Request, c Caller) {
	var body RollbackRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]string{"error": `body: {"autonomy": "record|propose|auto", "reason": "..."}`})
		return
	}
	res, err := s.RollbackVersion(r.Context(), c, "api", routineID(r), body)
	if err != nil {
		writeAction(w, err, 200, nil)
		return
	}
	writeJSON(w, 200, res)
}
