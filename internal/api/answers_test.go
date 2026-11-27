package api

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/chipi/flotilla/internal/cases"
	"github.com/chipi/flotilla/internal/casetimer"
	"github.com/chipi/flotilla/internal/hostwire"
	"github.com/chipi/flotilla/internal/spec"
	"github.com/chipi/flotilla/internal/store"
)

// escalatedCase is a proposed case its operator rejected: escalated, nothing locked.
func escalatedCase(t *testing.T, s *Server, key string) string {
	t.Helper()
	id := slackCase(t, s, key)
	if err := s.RejectCase(context.Background(), opCaller, "test", id, "not now"); err != nil {
		t.Fatal(err)
	}
	return id
}

// watchingCase is a case whose plan was recorded and which its monitor keeps watching (ADR-0023).
func watchingCase(t *testing.T, s *Server, key string) string {
	t.Helper()
	ctx := context.Background()
	id := detectedOn(t, s, key)
	_ = s.Store.MoveCase(ctx, id, cases.HandedOver, cases.ByDetector, "", "m", "")
	_ = s.Store.MoveCase(ctx, id, cases.Planned, cases.ByAct, "", "a", "")
	if _, err := s.Store.ApplyPlan(ctx, store.PlanRecord{CaseID: id, RunID: "r", Action: rerun, Params: map[string]string{"host": key},
		PlanHash: "hash-" + key + "-0123456789", Outcome: "record", Autonomy: "record"}, cases.Watching, "", "host:"+key, false); err != nil {
		t.Fatal(err)
	}
	return id
}

// detectedOn is a case just detected on host:<key>.
func detectedOn(t *testing.T, s *Server, key string) string {
	t.Helper()
	k := store.Key{Kind: "target", Value: "failed_backup/host:" + key}
	id, _, err := s.Store.OpenCase(context.Background(), store.NewCase{Kind: "failed_backup", Target: "host:" + key, Keys: []store.Key{k}, Canonical: k, Title: "backup " + key}, "m", "")
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func answerIDs(set AnswerSet) string {
	var out []string
	for _, a := range set.Answers {
		out = append(out, a.ID)
	}
	return strings.Join(out, ",")
}

// The case's status says what it asks and which answers it offers; a case that asks nothing is a 409.
func TestAnswersByKind(t *testing.T) {
	ctx := context.Background()
	s := newServer(t, func(*spec.Routine) {})
	plan := slackCase(t, s, "p")
	set, err := s.Answers(ctx, opCaller, "api", plan)
	if err != nil || set.Kind != KindPlan || answerIDs(set) != "approve,reject" || set.Question.PlanHash != "hash-p-0123456789" {
		t.Fatalf("a plan: %+v %v", set, err)
	}
	for _, a := range set.Answers {
		if !a.Allowed {
			t.Errorf("%s not allowed: %s", a.ID, a.Why)
		}
	}
	esc := escalatedCase(t, s, "e")
	if set, err = s.Answers(ctx, opCaller, "api", esc); err != nil || set.Kind != KindEscalation || answerIDs(set) != "dismiss,close,reassign" {
		t.Fatalf("an escalation: %+v %v", set, err)
	}
	watch := watchingCase(t, s, "w")
	if set, err = s.Answers(ctx, opCaller, "api", watch); err != nil || set.Kind != KindNotice || answerIDs(set) != "acknowledge" {
		t.Fatalf("a notice: %+v %v", set, err)
	}
	planned := detectedOn(t, s, "x")
	if _, err = s.Answers(ctx, opCaller, "api", planned); err == nil || AsProblem(err).Code != 409 {
		t.Fatalf("a planned case asks nothing: %v", err)
	}
	if _, err = s.Answers(ctx, opCaller, "api", "case-none"); err == nil || AsProblem(err).Code != 404 {
		t.Fatalf("no such case: %v", err)
	}
	// answers no case kind declares yet are refused, not guessed
	for _, a := range []string{"operator", "bugfix", "mute", "comment", "nonsense"} {
		if _, err := s.Answer(ctx, opCaller, "api", esc, a, AnswerIn{Reason: "x"}); err == nil || AsProblem(err).Code != 409 || !strings.Contains(err.Error(), "not an answer this case offers") {
			t.Errorf("%s: %v", a, err)
		}
	}
	// a frozen installation shows approve as not allowed, with why
	if err := s.SetFrozen(ctx, opCaller, "test", true, ""); err != nil {
		t.Fatal(err)
	}
	set, _ = s.Answers(ctx, opCaller, "api", plan)
	if set.Answers[0].Allowed || !strings.Contains(set.Answers[0].Why, "frozen") || !set.Answers[1].Allowed {
		t.Fatalf("frozen: %+v", set.Answers)
	}
}

// Approving through the answers is approving: the hash binds, a changed plan and a second answer are 409s, and the
// old route and the new one give the same reply.
func TestApproveViaAnswersEqualsApprove(t *testing.T) {
	ctx := context.Background()
	s := newServer(t, func(*spec.Routine) {})
	id := slackCase(t, s, "h")
	if _, err := s.Answer(ctx, opCaller, "api", id, "approve", AnswerIn{Plan: "deadbeefdeadbeef"}); err == nil || AsProblem(err).Code != 409 || !strings.Contains(err.Error(), "the plan changed") {
		t.Fatalf("a wrong plan hash: %v", err)
	}
	if c := caseStatus(t, s, id); c.Status != cases.AwaitingApproval {
		t.Fatalf("a refused approval moved the case: %s", c.Status)
	}
	out, err := s.Answer(ctx, opCaller, "api", id, "approve", AnswerIn{Plan: "hash-h-0123"})
	if err != nil || out["plan_hash"] != "hash-h-0123456789" || out["approved_by"] != "operator" {
		t.Fatalf("approve: %v %v", out, err)
	}
	if c := caseStatus(t, s, id); c.Status != cases.Executing {
		t.Fatalf("after: %s", c.Status)
	}
	if _, err := s.Answer(ctx, opCaller, "api", id, "approve", AnswerIn{}); err == nil || AsProblem(err).Code != 409 {
		t.Fatalf("approved twice: %v", err)
	}

	// over HTTP: POST /answers and POST /approve reply alike
	srv := newServer(t, func(*spec.Routine) {})
	hts := httptest.NewServer(srv.Handler())
	t.Cleanup(hts.Close)
	a, b := slackCase(t, srv, "i"), slackCase(t, srv, "j")
	c1, r1 := call(t, hts, "POST", "/v1/cases/"+a+"/answers", opTok, `{"answer":"approve","plan":"hash-i-0123"}`)
	c2, r2 := call(t, hts, "POST", "/v1/cases/"+b+"/approve", opTok, `{"plan":"hash-j-0123"}`)
	if c1 != 200 || c2 != 200 || r1["plan_hash"] != "hash-i-0123456789" || r2["plan_hash"] != "hash-j-0123456789" || r1["approved_by"] != r2["approved_by"] {
		t.Fatalf("answers %d %v, approve %d %v", c1, r1, c2, r2)
	}
	if c, _ := call(t, hts, "POST", "/v1/cases/"+a+"/answers", opTok, `{"answer":"approve"}`); c != 409 {
		t.Fatalf("a second approval over HTTP: %d", c)
	}
}

// Slack's limits live in answerPolicy: approve is capped and bound to a plan hash; reject is not capped.
func TestSlackRiskLimitInAnswers(t *testing.T) {
	ctx := context.Background()
	s := slackServer(t)
	for _, k := range []string{"a", "b"} {
		id := slackCase(t, s, k)
		ns, _ := s.SlackNotices(ctx)
		for _, n := range ns {
			if n.Case == id {
				if a := s.RelayedAnswer(ctx, "backup-01", hostwire.ApprovalBody{Nonce: n.Nonce, Team: "T0001", User: "U0OPER", Action: "approve"}); !a.OK {
					t.Fatal(a)
				}
			}
		}
	}
	id := slackCase(t, s, "c")
	cs, _ := s.Store.Case(ctx, id)
	ada := Caller{Name: "user:ada", Role: RoleOperator, User: true}
	if ok, why, err := s.answerPolicy(ctx, cs, ada, "slack", "approve", AnswerIn{Plan: "hash-c-0123456789"}); err != nil || ok || !strings.Contains(why, "daily cap (2)") {
		t.Fatalf("over the cap: %v %q %v", ok, why, err)
	}
	if ok, _, _ := s.answerPolicy(ctx, cs, ada, "slack", "reject", AnswerIn{}); !ok {
		t.Fatal("reject is capped")
	}
	if ok, _, _ := s.answerPolicy(ctx, cs, ada, "api", "approve", AnswerIn{}); !ok {
		t.Fatal("the cap applies outside Slack")
	}
	if ok, why, _ := s.answerPolicy(ctx, cs, ada, "slack", "approve", AnswerIn{}); ok || !strings.Contains(why, "bound to the plan") {
		t.Fatalf("an unbound approval from Slack: %v %q", ok, why)
	}
	// the same limit through Answer itself
	if _, err := s.Answer(ctx, ada, "slack", id, "approve", AnswerIn{Plan: "hash-c-0123456789"}); err == nil || AsProblem(err).Code != 403 {
		t.Fatalf("Answer over the cap: %v", err)
	}
	if c := caseStatus(t, s, id); c.Status != cases.AwaitingApproval {
		t.Fatalf("moved: %s", c.Status)
	}
}

// Agents never answer: not through Answer, and not through the routes, whatever the answer.
func TestMCPCannotAnswer(t *testing.T) {
	ctx := context.Background()
	s := newServer(t, func(*spec.Routine) {})
	id := slackCase(t, s, "m")
	for _, a := range []string{"approve", "reject", "close", "reassign", "dismiss", "acknowledge"} {
		if _, err := s.Answer(ctx, agentCaller, "mcp", id, a, AnswerIn{Plan: "hash-m-0123", Reason: "x"}); err == nil || AsProblem(err).Code != 403 {
			t.Errorf("an agent gave %s: %v", a, err)
		}
	}
	if _, err := s.Answers(ctx, agentCaller, "mcp", id); err == nil || AsProblem(err).Code != 403 {
		t.Fatalf("an agent listed the answers: %v", err)
	}
	hts := httptest.NewServer(s.Handler())
	t.Cleanup(hts.Close)
	if c, _ := call(t, hts, "GET", "/v1/cases/"+id+"/answers", agentTok, ""); c != 403 {
		t.Fatalf("GET answers as an agent: %d", c)
	}
	if c, _ := call(t, hts, "POST", "/v1/cases/"+id+"/answers", agentTok, `{"answer":"approve"}`); c != 403 {
		t.Fatalf("POST answers as an agent: %d", c)
	}
	if c := caseStatus(t, s, id); c.Status != cases.AwaitingApproval {
		t.Fatalf("moved: %s", c.Status)
	}
}

func caseIDOf(t *testing.T, s *Server, target string) string {
	t.Helper()
	cs, err := s.Store.Cases(context.Background(), store.CaseFilter{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cs {
		if c.Target == target {
			return c.ID
		}
	}
	t.Fatalf("no case on %s", target)
	return ""
}

// A relayed answer's actor is the flotilla person the directory names; the pin is the allowlist, and a directory that
// disagrees with it refuses.
func TestAnswerActorIsDirectoryResolved(t *testing.T) {
	ctx := context.Background()
	s := slackServer(t)
	click := func(key string) hostwire.AnsweredBody {
		id := slackCase(t, s, key)
		ns, _ := s.SlackNotices(ctx)
		for _, n := range ns {
			if n.Case == id {
				return s.RelayedAnswer(ctx, "backup-01", hostwire.ApprovalBody{Nonce: n.Nonce, Team: "T0001", User: "U0OPER", Action: "approve"})
			}
		}
		t.Fatal("no ask")
		return hostwire.AnsweredBody{}
	}
	person := func(flotilla string) {
		t.Helper()
		if _, err := s.Store.SyncPeople(ctx, []store.Person{{Email: "x@example.org", Name: "X", IDs: map[string]string{"slack": "U0OPER", "flotilla": flotilla}}}, "directory:file"); err != nil {
			t.Fatal(err)
		}
	}
	person("vic") // the directory says U0OPER is vic; the pin says ada
	if a := click("d1"); a.OK || !strings.Contains(a.Text, "the directory says") {
		t.Fatalf("a directory that disagrees with the pin: %+v", a)
	}
	person("ada")
	if a := click("d2"); !a.OK {
		t.Fatalf("a directory that agrees: %+v", a)
	}
	id := caseIDOf(t, s, "host:d2")
	plans, _ := s.Store.Plans(ctx, id)
	if who, err := s.approvedBy(ctx, caseStatus(t, s, id), plans[len(plans)-1]); err != nil || who != "user:ada" {
		t.Fatalf("approved by %q %v", who, err)
	}
	if _, err := s.Store.SyncPeople(ctx, nil, "directory:file"); err != nil {
		t.Fatal(err)
	}
	if a := click("d3"); !a.OK { // a person the directory does not know is the pinned name
		t.Fatalf("an unknown person: %+v", a)
	}
}

// Dismissing an escalated case closes it as dismissed, with a reason, releasing the target.
func TestDismissEscalated(t *testing.T) {
	ctx := context.Background()
	s := newServer(t, func(*spec.Routine) {})
	id := escalatedCase(t, s, "z")
	if _, err := s.Answer(ctx, opCaller, "api", id, "dismiss", AnswerIn{}); err == nil || AsProblem(err).Code != 400 {
		t.Fatalf("dismissed without a reason: %v", err)
	}
	out, err := s.Answer(ctx, opCaller, "api", id, "dismiss", AnswerIn{Reason: "noise"})
	if err != nil || out["dismissed_by"] != "operator" {
		t.Fatalf("dismiss: %v %v", out, err)
	}
	if c := caseStatus(t, s, id); c.Status != cases.Closed || c.Resolution != cases.Dismissed {
		t.Fatalf("after: %s %s", c.Status, c.Resolution)
	}
	ev, _ := s.Store.CaseEvents(ctx, id)
	if e := ev[len(ev)-1]; e.Actor != "operator" || e.By != cases.ByOperator || !strings.Contains(e.Detail, "noise") {
		t.Fatalf("the ledger: %+v", e)
	}
	if _, err := s.Answer(ctx, opCaller, "api", id, "dismiss", AnswerIn{Reason: "again"}); err == nil || AsProblem(err).Code != 409 {
		t.Fatalf("dismissed a closed case: %v", err)
	}
	other := slackCase(t, s, "y")
	if _, err := s.Answer(ctx, opCaller, "api", other, "dismiss", AnswerIn{Reason: "x"}); err == nil || AsProblem(err).Code != 409 {
		t.Fatalf("dismissed a case that is not escalated: %v", err)
	}
	// close and reassign still work as answers
	e2, e3 := escalatedCase(t, s, "k"), escalatedCase(t, s, "l")
	if _, err := s.Answer(ctx, opCaller, "api", e2, "close", AnswerIn{Reason: "done"}); err != nil {
		t.Fatal(err)
	}
	if c := caseStatus(t, s, e2); c.Resolution != cases.OperatorResolved {
		t.Fatalf("close: %s", c.Resolution)
	}
	if _, err := s.Answer(ctx, opCaller, "api", e3, "reassign", AnswerIn{Reason: "again"}); err != nil {
		t.Fatal(err)
	}
	if c := caseStatus(t, s, e3); c.Status != cases.HandedOver {
		t.Fatalf("reassign: %s", c.Status)
	}
}

// An acknowledged notice holds the next one back: the case timer reads the ack the answer wrote.
func TestAckNoticeSnoozes(t *testing.T) {
	ctx := context.Background()
	s := newServer(t, func(*spec.Routine) {})
	acked, plain := watchingCase(t, s, "1"), watchingCase(t, s, "2")
	now := time.Now()
	run := func(at time.Time) {
		t.Helper()
		if err := casetimer.Run(ctx, s.Store, at, casetimer.Config{}, s.Log, nil); err != nil {
			t.Fatal(err)
		}
	}
	count := func(id string) int {
		ns, err := s.Store.Notices(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return len(ns)
	}
	run(now)
	if count(acked) != 1 || count(plain) != 1 {
		t.Fatalf("the first notices: %d, %d", count(acked), count(plain))
	}
	if _, err := s.Answer(ctx, opCaller, "api", acked, "acknowledge", AnswerIn{}); err != nil {
		t.Fatal(err)
	}
	ns, _ := s.Store.Notices(ctx, acked)
	if ns[0].AckedBy != "operator" || ns[0].AckedAt == "" {
		t.Fatalf("the ack: %+v", ns[0])
	}
	ev, _ := s.Store.CaseEvents(ctx, acked)
	if e := ev[len(ev)-1]; e.Kind != "notice.acked" || e.Actor != "operator" {
		t.Fatalf("the event: %+v", e)
	}
	if c := caseStatus(t, s, acked); c.Status != cases.Watching {
		t.Fatalf("an ack moved the case: %s", c.Status)
	}
	// 5 hours on the second notice is due (at 4h): the unacknowledged case gets it, the acknowledged one is snoozed
	run(now.Add(5 * time.Hour))
	if count(plain) != 2 || count(acked) != 1 {
		t.Fatalf("after 5h: unacked %d notices (want 2), acked %d (want 1)", count(plain), count(acked))
	}
	run(now.Add(26 * time.Hour)) // past the snooze it falls due again
	if count(acked) != 2 {
		t.Fatalf("after the snooze: %d", count(acked))
	}
	if _, err := s.Answer(ctx, opCaller, "api", acked, "acknowledge", AnswerIn{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Answer(ctx, opCaller, "api", acked, "acknowledge", AnswerIn{}); err == nil || AsProblem(err).Code != 409 {
		t.Fatalf("acked twice: %v", err)
	}
	if _, err := s.Answer(ctx, opCaller, "api", slackCase(t, s, "n"), "acknowledge", AnswerIn{}); err == nil || AsProblem(err).Code != 409 {
		t.Fatalf("acked a case that is not watching: %v", err)
	}
}
