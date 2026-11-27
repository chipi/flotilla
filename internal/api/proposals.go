package api

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/chipi/flotilla/internal/cases"
	"github.com/chipi/flotilla/internal/store"
)

// A fleet's proposal (operator step 1 build plan, C6b): a case of kind sop_proposal that no routine acts on waits in
// handed_over with the change in its data (ops/sop_draft.py: the SOP's path, its whole text, the ready diff). A person
// answers it in the core, like every question (ADR-0023): Accept records the decision and closes the case operator
// resolved; flotilla writes nothing to the fleet's repo (rule 3), the person commits the diff the case shows. Reject
// closes it dismissed, with a reason. Only a person answers: never MCP, never a chat connector (a diff is read in full
// in the UI or the CLI).

const (
	AnsAccept    = "accept"
	KindProposal = "proposal"
	// ProposalCase is the case kind the SOP draft opens; its target is sop:<fleet>/<id>.
	ProposalCase = "sop_proposal"
)

// Proposal is what a sop_proposal case carries.
type Proposal struct {
	SopID    string   `json:"sop_id"`
	Mode     string   `json:"mode"`
	Fleet    string   `json:"fleet"`
	Path     string   `json:"path"`
	Body     string   `json:"body"`
	Diff     string   `json:"diff"`
	Evidence []string `json:"evidence"`
}

// ProposalOf is the proposal a case asks an answer to now: a sop_proposal in handed_over whose newest detection data has
// a diff and an id. Anything else is not one (ok false).
func ProposalOf(ctx context.Context, st *store.Store, cs store.Case) (Proposal, bool, error) {
	var p Proposal
	if cs.Kind != ProposalCase || cs.Status != cases.HandedOver {
		return p, false, nil
	}
	ev, err := st.CaseEvents(ctx, cs.ID)
	if err != nil {
		return p, false, err
	}
	for i := len(ev) - 1; i >= 0; i-- {
		if ev[i].Data == "" || (ev[i].Kind != "case.detected" && ev[i].Kind != "signal.attached") {
			continue
		}
		if json.Unmarshal([]byte(ev[i].Data), &p) != nil || p.Diff == "" || p.SopID == "" {
			return Proposal{}, false, nil
		}
		return p, true, nil
	}
	return Proposal{}, false, nil
}

func (s *Server) openProposal(ctx context.Context, id string) (store.Case, Proposal, error) {
	cs, err := s.Store.Case(ctx, id)
	if errors.Is(err, store.ErrNoSuchCase) {
		return cs, Proposal{}, problem(404, "no such case")
	}
	if err != nil {
		return cs, Proposal{}, err
	}
	p, ok, err := ProposalOf(ctx, s.Store, cs)
	if err != nil {
		return cs, p, err
	}
	if !ok {
		return cs, p, problem(409, "case %s (%s, %s) is not a proposal waiting for an answer", id, cs.Kind, cs.Status)
	}
	return cs, p, nil
}

// AcceptProposal is a person accepting a proposal: the case closes operator resolved with the diff left for them to
// commit. Nothing is written to any fleet folder.
func (s *Server) AcceptProposal(ctx context.Context, c Caller, via, id, note string) error {
	if err := s.Check(ctx, c, ActApprove); err != nil {
		return err
	}
	_, p, err := s.openProposal(ctx, id)
	if err != nil {
		return err
	}
	detail := "accepted: commit the diff (" + p.Path + ")"
	if n := strings.TrimSpace(note); n != "" {
		detail += ": " + n
	}
	if err := s.Store.MoveCase(ctx, id, cases.Closed, cases.ByOperator, cases.OperatorResolved, c.Name, detail); err != nil {
		return err
	}
	s.Log.Info("proposal accepted", "case", id, "by", c.Name, "path", p.Path, "via", via)
	return nil
}

// RejectProposal is a person rejecting a proposal, with a reason: the case closes dismissed.
func (s *Server) RejectProposal(ctx context.Context, c Caller, via, id, why string) error {
	if err := s.Check(ctx, c, ActApprove); err != nil {
		return err
	}
	if strings.TrimSpace(why) == "" {
		return problem(400, "rejecting a proposal needs a reason (--reason)")
	}
	_, p, err := s.openProposal(ctx, id)
	if err != nil {
		return err
	}
	if err := s.Store.MoveCase(ctx, id, cases.Closed, cases.ByOperator, cases.Dismissed, c.Name, "rejected: "+why); err != nil {
		return err
	}
	s.Log.Info("proposal rejected", "case", id, "by", c.Name, "path", p.Path, "reason", why, "via", via)
	return nil
}

// IsProposal is whether a reject or accept on this case is the proposal's (its kind, not its status, so a refusal is the proposal's too).
func IsProposal(cs store.Case) bool { return cs.Kind == ProposalCase }
