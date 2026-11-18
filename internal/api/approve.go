package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/chipi/flotilla/internal/cases"
	"github.com/chipi/flotilla/internal/policy"
	"github.com/chipi/flotilla/internal/store"
)

// RFC-0004 §5 and ADR-0005: a plan policy proposed waits in awaiting_approval for a person. Approving
// sends it on (executing: flotillad signs and sends it to its host, approved by that person); rejecting
// escalates the case, releases the target and demotes the action to record. The CLI, HTTP and the UI
// (its Decision box) answer the same way.

func (s *Server) caseRoutes(m *http.ServeMux) {
	m.Handle("POST /v1/cases/{id}/approve", s.auth(ActApprove, s.caseApprove))
	m.Handle("POST /v1/cases/{id}/reject", s.auth(ActApprove, s.caseReject))
	m.Handle("POST /v1/cases/{id}/close", s.auth(ActApprove, s.caseClose))
	m.Handle("POST /v1/cases/{id}/applied", s.auth(ActApprove, s.caseApplied))
	m.Handle("POST /v1/cases/{id}/notes", s.auth(ActNotePropose, s.caseNote))
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
	return s.ApprovePlan(ctx, c, via, id, "", "")
}

// ApprovePlan is ApproveCase bound to the plan the caller saw: refused when the case's newest plan is
// not planHash (a prefix of it is enough; "" approves the newest). The note is kept with the answer.
func (s *Server) ApprovePlan(ctx context.Context, c Caller, via, id, planHash, note string) (string, error) {
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
	if hp := strings.TrimPrefix(planHash, "sha256:"); planHash != "" && (len(hp) < 8 || !strings.HasPrefix(p.PlanHash, hp)) {
		return "", problem(409, "the plan changed: %s is not the plan on case %s now (%s); open the new plan", shortHash(planHash), id, shortHash(p.PlanHash))
	}
	detail := fmt.Sprintf("approved %s on %s (plan %s)", p.Action, cs.Target, shortHash(p.PlanHash))
	if n := strings.TrimSpace(note); n != "" {
		detail += ": " + n
	}
	if err := s.Store.MoveCase(ctx, id, cases.Executing, cases.ByOperator, "", c.Name, detail); err != nil {
		return "", err
	}
	s.Log.Info("case approved", "case", id, "by", c.Name, "action", p.Action, "target", cs.Target, "via", via)
	return p.PlanHash, nil
}

// RejectCase escalates a proposed case, with the caller's reason; the store releases the target and
// demotes the action to record (RFC-0004 §5.2).
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
	var body struct {
		Plan string `json:"plan"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body)
	hash, err := s.ApprovePlan(r.Context(), c, "api", r.PathValue("id"), strings.TrimSpace(body.Plan), "")
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

var commitRe = regexp.MustCompile(`^[0-9a-f]{7,40}$`)

// RecordApplied closes an approved repo change that a person applied by hand as verified, naming the
// commit (ADR-0016: until step 4 opens PRs). Only a repo action's case, only while it is executing.
func (s *Server) RecordApplied(ctx context.Context, c Caller, via, id, commit string) error {
	if err := s.Check(ctx, c, ActApprove); err != nil {
		return err
	}
	commit = strings.ToLower(strings.TrimSpace(commit))
	if !commitRe.MatchString(commit) {
		return problem(400, "the commit: 7 to 40 hex characters")
	}
	cs, err := s.Store.Case(ctx, id)
	if errors.Is(err, store.ErrNoSuchCase) {
		return problem(404, "no such case")
	}
	if err != nil {
		return err
	}
	if cs.Status != cases.Executing {
		return problem(409, "case %s is %s, not executing (approved)", id, cs.Status)
	}
	plans, err := s.Store.Plans(ctx, id)
	if err != nil {
		return err
	}
	if len(plans) == 0 || s.Registry == nil {
		return problem(409, "case %s has no plan", id)
	}
	p := plans[len(plans)-1]
	if a, ok := s.Registry.Action(p.Action); !ok || !a.Repo() {
		return problem(409, "%s is not a repo change: its host reports the result", p.Action)
	}
	if err := s.Store.MoveCase(ctx, id, cases.Closed, cases.ByOperator, cases.Verified, c.Name,
		fmt.Sprintf("applied in commit %s (plan %s)", commit, shortHash(p.PlanHash))); err != nil {
		return err
	}
	s.Log.Info("repo change applied", "case", id, "by", c.Name, "commit", commit, "via", via)
	return nil
}

func (s *Server) caseApplied(w http.ResponseWriter, r *http.Request, c Caller) {
	var body struct {
		Commit string `json:"commit"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body)
	err := s.RecordApplied(r.Context(), c, "api", r.PathValue("id"), body.Commit)
	writeAction(w, err, 200, map[string]string{"case": r.PathValue("id"), "applied_by": c.Name})
}

// caseNote is a note proposed from a case (agents included: RFC-0008 Q4, ADR-0016); a person activates it.
func (s *Server) caseNote(w http.ResponseWriter, r *http.Request, c Caller) {
	var body struct {
		Text string `json:"text"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 16384)).Decode(&body)
	id, err := s.ProposeNote(r.Context(), c, "api", r.PathValue("id"), body.Text)
	writeAction(w, err, 200, map[string]string{"case": r.PathValue("id"), "note": strconv.FormatInt(id, 10)})
}
