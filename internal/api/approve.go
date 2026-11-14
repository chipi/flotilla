package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/chipi/flotilla/internal/cases"
	"github.com/chipi/flotilla/internal/policy"
	"github.com/chipi/flotilla/internal/store"
)

// RFC-0004 §5 and ADR-0005: a plan policy proposed waits in awaiting_approval for a person. Approving
// sends it on (executing: flotillad signs and sends it to its host, approved by that person); rejecting
// escalates the case, releases the target and demotes the action to shadow. The CLI and HTTP for now;
// the UI's approvals come with pipeline v1 step 8b.

func (s *Server) caseRoutes(m *http.ServeMux) {
	m.Handle("POST /v1/cases/{id}/approve", s.auth(ActApprove, s.caseApprove))
	m.Handle("POST /v1/cases/{id}/reject", s.auth(ActApprove, s.caseReject))
	m.Handle("POST /v1/cases/{id}/close", s.auth(ActApprove, s.caseClose))
	m.Handle("POST /v1/cases/{id}/reassign", s.auth(ActApprove, s.caseReassign))
}

// escalated is a person's move out of escalated (RFC-0004 §3.5): with a reason, on an escalated case.
func (s *Server) escalated(ctx context.Context, c Caller, id, why, what string) error {
	if err := s.Check(ctx, c, ActApprove); err != nil {
		return err
	}
	if strings.TrimSpace(why) == "" {
		return problem(400, "%s a case needs a reason (--reason)", what)
	}
	cs, err := s.Store.Case(ctx, id)
	if errors.Is(err, store.ErrNoSuchCase) {
		return problem(404, "no such case")
	}
	if err != nil {
		return err
	}
	if cs.Status != cases.Escalated {
		return problem(409, "case %s is %s: only an escalated case is %s by a person", id, cs.Status, what)
	}
	return nil
}

// CloseCase is a person resolving an escalated case (escalated -> closed, operator resolved), with the
// caller's reason. It releases the target: an escalation (not verified, failed, unknown) holds it
// until then.
func (s *Server) CloseCase(ctx context.Context, c Caller, via, id, why string) error {
	if err := s.escalated(ctx, c, id, why, "closing"); err != nil {
		return err
	}
	if err := s.Store.MoveCase(ctx, id, cases.Closed, cases.ByOperator, cases.OperatorResolved, c.Name, why); err != nil {
		return err
	}
	s.Log.Info("case closed", "case", id, "by", c.Name, "reason", why, "via", via)
	return nil
}

// ReassignCase is a person handing an escalated case back to its acting routine (escalated ->
// handed_over), with the caller's reason: it releases the target, the routine re-checks and plans
// again, and the hand-over clock starts anew.
func (s *Server) ReassignCase(ctx context.Context, c Caller, via, id, why string) error {
	if err := s.escalated(ctx, c, id, why, "reassigning"); err != nil {
		return err
	}
	if err := s.Store.MoveCase(ctx, id, cases.HandedOver, cases.ByOperator, "", c.Name, "reassigned: "+why); err != nil {
		return err
	}
	s.Log.Info("case reassigned", "case", id, "by", c.Name, "reason", why, "via", via)
	return nil
}

func (s *Server) awaiting(ctx context.Context, id string) (store.Case, error) {
	c, err := s.Store.Case(ctx, id)
	if errors.Is(err, store.ErrNoSuchCase) {
		return c, problem(404, "no such case")
	}
	if err != nil {
		return c, err
	}
	if c.Status != cases.AwaitingApproval {
		return c, problem(409, "case %s is %s, not awaiting approval", id, c.Status)
	}
	return c, nil
}

// ApproveCase moves a proposed case on to executing, as the caller. Refused while the installation is
// frozen or a hold covers the target or its host: nothing may start then, approved or not.
func (s *Server) ApproveCase(ctx context.Context, c Caller, via, id string) (string, error) {
	if err := s.Check(ctx, c, ActApprove); err != nil {
		return "", err
	}
	cs, err := s.awaiting(ctx, id)
	if err != nil {
		return "", err
	}
	if frozen, err := s.Sched.Frozen(ctx); err != nil {
		return "", err
	} else if frozen {
		return "", problem(409, "the installation is frozen: nothing starts, approved or not")
	}
	holds, err := s.Store.Holds(ctx, s.Sched.Clock(), false)
	if err != nil {
		return "", err
	}
	host := policy.HostOf(cs.Target)
	for _, h := range holds {
		if h.Scope == cs.Target || (host != "" && h.Scope == "host:"+host) {
			return "", problem(409, "held: %s", h.String())
		}
	}
	plans, err := s.Store.Plans(ctx, id)
	if err != nil {
		return "", err
	}
	if len(plans) == 0 {
		return "", problem(409, "case %s has no plan to approve", id)
	}
	p := plans[len(plans)-1]
	detail := fmt.Sprintf("approved %s on %s (plan %s)", p.Action, cs.Target, shortHash(p.PlanHash))
	if err := s.Store.MoveCase(ctx, id, cases.Executing, cases.ByOperator, "", c.Name, detail); err != nil {
		return "", err
	}
	s.Log.Info("case approved", "case", id, "by", c.Name, "action", p.Action, "target", cs.Target, "via", via)
	return p.PlanHash, nil
}

// RejectCase escalates a proposed case, with the caller's reason; the store releases the target and
// demotes the action to shadow (RFC-0004 §5.2).
func (s *Server) RejectCase(ctx context.Context, c Caller, via, id, why string) error {
	if err := s.Check(ctx, c, ActApprove); err != nil {
		return err
	}
	if strings.TrimSpace(why) == "" {
		return problem(400, "rejecting needs a reason (--reason)")
	}
	if _, err := s.awaiting(ctx, id); err != nil {
		return err
	}
	if err := s.Store.MoveCase(ctx, id, cases.Escalated, cases.ByOperator, "", c.Name, "rejected: "+why); err != nil {
		return err
	}
	s.Log.Info("case rejected", "case", id, "by", c.Name, "reason", why, "via", via)
	return nil
}

func shortHash(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

func (s *Server) caseApprove(w http.ResponseWriter, r *http.Request, c Caller) {
	hash, err := s.ApproveCase(r.Context(), c, "api", r.PathValue("id"))
	writeAction(w, err, 200, map[string]string{"case": r.PathValue("id"), "approved_by": c.Name, "plan_hash": hash})
}

func (s *Server) caseReject(w http.ResponseWriter, r *http.Request, c Caller) {
	err := s.RejectCase(r.Context(), c, "api", r.PathValue("id"), reason(r, w))
	writeAction(w, err, 200, map[string]string{"case": r.PathValue("id"), "rejected_by": c.Name})
}

func (s *Server) caseClose(w http.ResponseWriter, r *http.Request, c Caller) {
	err := s.CloseCase(r.Context(), c, "api", r.PathValue("id"), reason(r, w))
	writeAction(w, err, 200, map[string]string{"case": r.PathValue("id"), "closed_by": c.Name})
}

func (s *Server) caseReassign(w http.ResponseWriter, r *http.Request, c Caller) {
	err := s.ReassignCase(r.Context(), c, "api", r.PathValue("id"), reason(r, w))
	writeAction(w, err, 200, map[string]string{"case": r.PathValue("id"), "reassigned_by": c.Name})
}
