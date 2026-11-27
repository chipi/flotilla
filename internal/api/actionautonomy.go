package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/chipi/flotilla/internal/spec"
	"github.com/chipi/flotilla/internal/store"
)

// An action's own autonomy (ADR-0008, RFC-0004 §5.2): record -> propose -> auto, each step earned on its
// record and promoted only by an operator or an admin, with a reason; one step at a time; down any
// number of steps, no reason needed. An action that cannot be undone never goes above propose. Policy
// reads it from the controls (actionSubject) and takes the lowest of it and the other caps.

// ActionStatus is one declared action, its autonomy and its record.
type ActionStatus struct {
	Name       string             `json:"name"`
	ForKinds   []string           `json:"for_kinds"`
	Risk       string             `json:"risk"`
	Reversible string             `json:"reversible"`
	Autonomy   string             `json:"autonomy"`
	Max        string             `json:"max"` // the highest it may be promoted to
	Record     store.ActionRecord `json:"record"`
}

// Actions lists every declared action with its autonomy and record.
func (s *Server) Actions(ctx context.Context) ([]ActionStatus, error) {
	var out []ActionStatus
	if s.Registry == nil {
		return out, nil
	}
	for _, name := range s.Registry.Actions() {
		a, _ := s.Registry.Action(name)
		lvl, err := s.Store.Control(ctx, actionSubject(name), "autonomy")
		if err != nil {
			return nil, err
		}
		if lvl == "" {
			lvl = spec.AutonomyRecord
		}
		rec, err := s.Store.ActionRecord(ctx, name)
		if err != nil {
			return nil, err
		}
		out = append(out, ActionStatus{Name: name, ForKinds: a.ForKinds, Risk: a.Risk, Reversible: a.Reversible,
			Autonomy: lvl, Max: a.MaxAutonomy(), Record: rec})
	}
	return out, nil
}

// SetActionAutonomy promotes or demotes an action.
func (s *Server) SetActionAutonomy(ctx context.Context, c Caller, via, name, level, reason string) error {
	if err := s.Check(ctx, c, ActAutonomy); err != nil {
		return err
	}
	if s.Registry == nil {
		return problem(404, "no such action")
	}
	a, ok := s.Registry.Action(name)
	if !ok {
		return problem(404, "no such action")
	}
	level, _ = spec.CanonicalLevel(level)
	switch level {
	case spec.AutonomyRecord, spec.AutonomyPropose, spec.AutonomyAuto:
	default:
		return problem(400, "autonomy must be record, propose or auto")
	}
	if spec.Rank(level) > spec.Rank(a.MaxAutonomy()) {
		if a.RecordOnly() {
			return problem(400, "%s is record-only (nothing carries it out): at most record", name)
		}
		return problem(400, "%s cannot be undone (reversible: no): at most propose", name)
	}
	cur, err := s.Store.Control(ctx, actionSubject(name), "autonomy")
	if err != nil {
		return err
	}
	if cur == "" {
		cur = spec.AutonomyRecord
	}
	reason = strings.TrimSpace(reason)
	if spec.Rank(level) > spec.Rank(cur) {
		if spec.Rank(level)-spec.Rank(cur) > 1 {
			return problem(400, "%s is %s: promote one step at a time (to propose first)", name, cur)
		}
		if reason == "" {
			return problem(400, "promoting %s from %s to %s needs a reason: what its record shows", name, cur, level)
		}
	}
	if err := s.Store.SetControlBecause(ctx, actionSubject(name), "autonomy", level, c.Name, via, reason); err != nil {
		return err
	}
	s.Log.Info("action autonomy set", "action", name, "from", cur, "to", level, "caller", c.Name, "via", via, "reason", reason)
	return nil
}

func (s *Server) actionsList(w http.ResponseWriter, r *http.Request, _ Caller) {
	as, err := s.Actions(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	if as == nil {
		as = []ActionStatus{}
	}
	writeJSON(w, 200, map[string]any{"actions": as})
}

func (s *Server) actionAutonomy(w http.ResponseWriter, r *http.Request, c Caller) {
	var body struct {
		Autonomy string `json:"autonomy"`
		Reason   string `json:"reason"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]string{"error": `body: {"autonomy": "record|propose|auto", "reason": "..."}`})
		return
	}
	level, note := spec.CanonicalLevel(body.Autonomy)
	err := s.SetActionAutonomy(r.Context(), c, "api", r.PathValue("name"), level, body.Reason)
	writeAction(w, err, 200, withNote(map[string]string{"action": r.PathValue("name"), "autonomy": level, "by": c.Name}, note))
}
