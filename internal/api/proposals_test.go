package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/chipi/flotilla/internal/cases"
	"github.com/chipi/flotilla/internal/spec"
	"github.com/chipi/flotilla/internal/store"
)

// proposalCase is a sop_proposal as ops/sop_draft.py reports it: handed over, no routine acting on it, its change in its data.
func proposalCase(t *testing.T, s *Server, sop string, data map[string]any) string {
	t.Helper()
	b, _ := json.Marshal(data)
	k := store.Key{Kind: "target", Value: "sop_proposal/sop:fleet/" + sop}
	id, _, err := s.Store.OpenCase(context.Background(), store.NewCase{Kind: ProposalCase, Target: "sop:fleet/" + sop, Keys: []store.Key{k}, Canonical: k, Data: string(b)}, "ops/sop-draft", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Store.MoveCase(context.Background(), id, cases.HandedOver, cases.ByDetector, "", "ops/sop-draft", ""); err != nil {
		t.Fatal(err)
	}
	return id
}

func goodProposal(sop string) map[string]any {
	return map[string]any{"sop_id": sop, "mode": "add", "fleet": "fleet", "path": "fleet/sops/" + sop + "/SKILL.md", "body": "b",
		"evidence": []string{"case-1"}, "diff": "--- /dev/null\n+++ b/fleet/sops/" + sop + "/SKILL.md\n+x\n"}
}

// A proposal that waits offers Accept and Reject (ADR-0023: answered in the core); each answer closes it with the person's name on
// the event, and nothing is written anywhere (rule 3).
func TestProposalAnswers(t *testing.T) {
	ctx := context.Background()
	s := newServer(t, func(*spec.Routine) {})
	acc, rej := proposalCase(t, s, "a", goodProposal("a")), proposalCase(t, s, "r", goodProposal("r"))
	set, err := s.Answers(ctx, opCaller, "api", acc)
	if err != nil || set.Kind != KindProposal || answerIDs(set) != "accept,reject" || !strings.Contains(set.Question.Text, "fleet/sops/a/SKILL.md") {
		t.Fatalf("a proposal asks: %+v %v", set, err)
	}
	out, err := s.Answer(ctx, opCaller, "api", acc, "accept", AnswerIn{Note: "reviewed"})
	if err != nil || out["accepted_by"] != "operator" {
		t.Fatalf("accept: %v %v", out, err)
	}
	c, _ := s.Store.Case(ctx, acc)
	if c.Status != cases.Closed || c.Resolution != cases.OperatorResolved {
		t.Fatalf("after accept: %s %s", c.Status, c.Resolution)
	}
	ev, _ := s.Store.CaseEvents(ctx, acc)
	if last := ev[len(ev)-1]; last.Actor != "operator" || last.By != cases.ByOperator || !strings.Contains(last.Detail, "accepted: commit the diff") || !strings.Contains(last.Detail, "reviewed") {
		t.Fatalf("the accept's event: %+v", last)
	}
	if _, err := s.Answer(ctx, opCaller, "api", acc, "accept", AnswerIn{}); err == nil || AsProblem(err).Code != 409 {
		t.Fatalf("accepting twice: %v", err)
	}
	if _, err := s.Answer(ctx, opCaller, "api", rej, "reject", AnswerIn{}); err == nil || AsProblem(err).Code != 400 {
		t.Fatalf("a reject without a reason: %v", err)
	}
	if _, err := s.Answer(ctx, opCaller, "api", rej, "reject", AnswerIn{Reason: "too wide"}); err != nil {
		t.Fatal(err)
	}
	c, _ = s.Store.Case(ctx, rej)
	ev, _ = s.Store.CaseEvents(ctx, rej)
	if c.Status != cases.Closed || c.Resolution != cases.Dismissed || ev[len(ev)-1].Actor != "operator" || !strings.HasSuffix(ev[len(ev)-1].Detail, "rejected: too wide") {
		t.Fatalf("after reject: %s %s %+v", c.Status, c.Resolution, ev[len(ev)-1])
	}
}

// What is not a waiting proposal offers no Accept: a sop_proposal with no diff, a closed one, and any other kind of case in handed_over.
func TestAcceptOnlyForAWaitingProposal(t *testing.T) {
	ctx := context.Background()
	s := newServer(t, func(*spec.Routine) {})
	bad := goodProposal("b")
	delete(bad, "diff")
	noDiff := proposalCase(t, s, "b", bad)
	if _, err := s.Answers(ctx, opCaller, "api", noDiff); err == nil || AsProblem(err).Code != 409 {
		t.Fatalf("a proposal with no diff asks: %v", err)
	}
	if _, err := s.Answer(ctx, opCaller, "api", noDiff, "accept", AnswerIn{}); err == nil || AsProblem(err).Code != 409 {
		t.Fatalf("accepted a proposal with no diff: %v", err)
	}
	other := detectedOn(t, s, "h")
	_ = s.Store.MoveCase(ctx, other, cases.HandedOver, cases.ByDetector, "", "m", "")
	for _, a := range []string{"accept", "reject"} {
		if _, err := s.Answer(ctx, opCaller, "api", other, a, AnswerIn{Reason: "x"}); err == nil || AsProblem(err).Code != 409 {
			t.Errorf("%s on a handed-over backup case: %v", a, err)
		}
	}
	if c, _ := s.Store.Case(ctx, other); c.Status != cases.HandedOver {
		t.Fatalf("moved: %s", c.Status)
	}
	// a plan's reject still escalates
	plan := slackCase(t, s, "pl")
	if _, err := s.Answer(ctx, opCaller, "api", plan, "reject", AnswerIn{Reason: "no"}); err != nil {
		t.Fatal(err)
	}
	if c, _ := s.Store.Case(ctx, plan); c.Status != cases.Escalated {
		t.Fatalf("a plan's reject: %s", c.Status)
	}
}

// Agents and chat connectors never answer a proposal: not MCP, not the routes, not Slack (a diff is read in the UI or the CLI).
func TestProposalNotAnsweredByAgentsOrSlack(t *testing.T) {
	ctx := context.Background()
	s := newServer(t, func(*spec.Routine) {})
	id := proposalCase(t, s, "m", goodProposal("m"))
	for _, a := range []string{"accept", "reject"} {
		if _, err := s.Answer(ctx, agentCaller, "mcp", id, a, AnswerIn{Reason: "x"}); err == nil || AsProblem(err).Code != 403 {
			t.Errorf("an agent gave %s: %v", a, err)
		}
		if _, err := s.Answer(ctx, opCaller, "slack", id, a, AnswerIn{Reason: "x"}); err == nil || AsProblem(err).Code != 403 {
			t.Errorf("Slack gave %s: %v", a, err)
		}
	}
	viewer := Caller{Name: "vic", Role: RoleViewer, User: true}
	if _, err := s.Answer(ctx, viewer, "api", id, "accept", AnswerIn{}); err == nil || AsProblem(err).Code != 403 {
		t.Errorf("a viewer accepted: %v", err)
	}
	if _, err := s.Answers(ctx, agentCaller, "mcp", id); err == nil || AsProblem(err).Code != 403 {
		t.Fatalf("an agent listed the answers: %v", err)
	}
	set, err := s.Answers(ctx, opCaller, "slack", id)
	if err != nil || len(set.Answers) != 2 || set.Answers[0].Allowed || set.Answers[1].Allowed {
		t.Fatalf("Slack is offered the answers as allowed: %+v %v", set, err)
	}
	hts := httptest.NewServer(s.Handler())
	t.Cleanup(hts.Close)
	if c, _ := call(t, hts, "POST", "/v1/cases/"+id+"/answers", agentTok, `{"answer":"accept"}`); c != 403 {
		t.Fatalf("POST accept as an agent: %d", c)
	}
	if c := caseStatus(t, s, id); c.Status != cases.HandedOver {
		t.Fatalf("moved: %s", c.Status)
	}
}
