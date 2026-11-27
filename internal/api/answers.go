package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/chipi/flotilla/internal/cases"
	"github.com/chipi/flotilla/internal/store"
)

// ADR-0023 decision 2: a question to the operator is answered in the core, whoever asks it. The case's status says
// what kind of question it is and which answers it offers; the UI, the CLI and a chat connector all answer
// through Answer, and answerPolicy is the one place that holds a connector's limits (ADR-0005's, for Slack).

// Answer ids. The first four are the routes /approve, /reject, /close and /reassign by their older names.
const (
	AnsApprove     = "approve"
	AnsReject      = "reject"
	AnsClose       = "close"
	AnsReassign    = "reassign"
	AnsDismiss     = "dismiss"
	AnsAcknowledge = "acknowledge"
)

// AnswerIn is what an answer carries: the plan hash a plan answer is bound to, the reason a move out of
// escalated needs, and a note kept with the answer.
type AnswerIn struct {
	Plan   string `json:"plan"`
	Reason string `json:"reason"`
	Note   string `json:"note"`
}

// AnswerOption is one answer a case offers now, with whether this caller may give it and why not.
type AnswerOption struct {
	ID      string `json:"id"`
	Label   string `json:"label"`
	Allowed bool   `json:"allowed"`
	Why     string `json:"why"`
}

// AnswerSet is GET /v1/cases/{id}/answers.
type AnswerSet struct {
	Case     string         `json:"case"`
	Kind     string         `json:"kind"`
	Question AnswerQuestion `json:"question"`
	Answers  []AnswerOption `json:"answers"`
}

// AnswerQuestion is what is asked.
type AnswerQuestion struct {
	Title    string `json:"title"`
	Text     string `json:"text"`
	PlanHash string `json:"plan_hash,omitempty"`
}

// Question kinds follow the case's status.
const (
	KindPlan       = "plan"
	KindEscalation = "escalation"
	KindNotice     = "notice"
)

// offered is the answers a status offers, in order. Escalated offers only what exists today: the operator, bugfix,
// mute and comment answers wait for a case kind that declares them.
func offered(status string) (kind string, ids []string, labels map[string]string) {
	labels = map[string]string{AnsApprove: "Approve", AnsReject: "Reject", AnsDismiss: "Dismiss", AnsClose: "Close",
		AnsReassign: "Hand back", AnsAcknowledge: "Acknowledge"}
	switch status {
	case cases.AwaitingApproval:
		return KindPlan, []string{AnsApprove, AnsReject}, labels
	case cases.Escalated:
		return KindEscalation, []string{AnsDismiss, AnsClose, AnsReassign}, labels
	case cases.Watching:
		return KindNotice, []string{AnsAcknowledge}, labels
	}
	return "", nil, labels
}

// Answers is what the case asks the operator now, and which answers the caller may give through via.
func (s *Server) Answers(ctx context.Context, c Caller, via, id string) (AnswerSet, error) {
	if err := s.Check(ctx, c, ActApprove); err != nil {
		return AnswerSet{}, err
	}
	cs, err := s.Store.Case(ctx, id)
	if errors.Is(err, store.ErrNoSuchCase) {
		return AnswerSet{}, problem(404, "no such case")
	}
	if err != nil {
		return AnswerSet{}, err
	}
	kind, ids, labels := offered(cs.Status)
	if kind == "" {
		return AnswerSet{}, problem(409, "case %s is %s: it asks nothing now", id, cs.Status)
	}
	title := cs.Title
	if title == "" {
		title = cs.Kind + " on " + cs.Target
	}
	set := AnswerSet{Case: id, Kind: kind, Question: AnswerQuestion{Title: title}}
	var plan store.PlanRecord
	hasPlan := false
	if plans, err := s.Store.Plans(ctx, id); err != nil {
		return set, err
	} else if len(plans) > 0 {
		plan, hasPlan = plans[len(plans)-1], true
	}
	switch kind {
	case KindPlan:
		if hasPlan {
			set.Question.Text, set.Question.PlanHash = plan.Action+" on "+cs.Target, plan.PlanHash
		}
	case KindEscalation:
		set.Question.Text = "escalated: " + cs.Kind + " on " + cs.Target
	case KindNotice:
		set.Question.Text = "watched: " + cs.Kind + " on " + cs.Target
		if ns, err := s.Store.Notices(ctx, id); err == nil && len(ns) > 0 {
			n := ns[len(ns)-1]
			set.Question.Text += " (notice " + strconv.Itoa(n.N) + " due " + n.DueAt + ")"
		}
	}
	for _, a := range ids {
		o := AnswerOption{ID: a, Label: labels[a], Allowed: true}
		in := AnswerIn{}
		if via == "slack" {
			in.Plan = set.Question.PlanHash
		}
		ok, why, err := s.answerPolicy(ctx, cs, c, via, a, in)
		if err != nil {
			return set, err
		}
		if ok && a == AnsApprove {
			if berr := s.approveBlock(ctx, cs); berr != nil {
				if p := AsProblem(berr); p != nil {
					ok, why = false, p.Msg
				} else {
					return set, berr
				}
			}
		}
		o.Allowed, o.Why = ok, why
		set.Answers = append(set.Answers, o)
	}
	return set, nil
}

// slackDayStart is the start of the daily cap's day (in the scheduler's location), as the store writes times.
func (s *Server) slackDayStart() string {
	loc := time.UTC
	if s.Sched != nil && s.Sched.Location != nil {
		loc = s.Sched.Location
	}
	now := time.Now()
	if s.Sched != nil {
		now = s.Sched.Clock()
	}
	n := now.In(loc)
	return time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, loc).UTC().Format(store.TimeFormat)
}

// answerPolicy is whether the caller may give this answer through via, and why not. It holds the limits that belong
// to a connector rather than to the answer: Slack approves only a low or medium risk, reversible or regenerable
// plan, bound to a plan hash, under its daily cap (ADR-0005 decision 8). Rejecting is not capped.
func (s *Server) answerPolicy(ctx context.Context, cs store.Case, c Caller, via, answer string, in AnswerIn) (bool, string, error) {
	if via != "slack" || answer != AnsApprove {
		return true, "", nil
	}
	if s.Slack == nil {
		return false, "Slack is not configured", nil
	}
	if strings.TrimSpace(in.Plan) == "" {
		return false, "an approval from Slack is bound to the plan it showed", nil
	}
	plans, err := s.Store.Plans(ctx, cs.ID)
	if err != nil {
		return false, "", err
	}
	if len(plans) == 0 {
		return false, "case " + cs.ID + " has no plan to approve", nil
	}
	if ok, why := s.SlackApprovable(plans[len(plans)-1].Action); !ok {
		return false, why, nil
	}
	used, err := s.Store.SlackApprovalsSince(ctx, s.slackDayStart())
	if err != nil {
		return false, "", err
	}
	if used >= s.Slack.Cap() {
		return false, "Slack's daily cap (" + strconv.Itoa(s.Slack.Cap()) + ") is used; answer in the UI or the CLI", nil
	}
	return true, "", nil
}

// Answer gives one answer to a case's question, as the caller, through via ("api", "ui", "slack"). The older moves
// keep their own checks and messages; the reply names who answered.
func (s *Server) Answer(ctx context.Context, c Caller, via, id, answer string, in AnswerIn) (map[string]string, error) {
	if err := s.Check(ctx, c, ActApprove); err != nil {
		return nil, err
	}
	switch answer {
	case AnsApprove, AnsReject, AnsClose, AnsReassign, AnsDismiss, AnsAcknowledge:
	default:
		return nil, problem(409, "%q is not an answer this case offers", answer)
	}
	cs, err := s.Store.Case(ctx, id)
	if errors.Is(err, store.ErrNoSuchCase) {
		return nil, problem(404, "no such case")
	}
	if err != nil {
		return nil, err
	}
	if ok, why, err := s.answerPolicy(ctx, cs, c, via, answer, in); err != nil {
		return nil, err
	} else if !ok {
		return nil, problem(403, "refused: %s", why)
	}
	return s.give(ctx, c, via, id, answer, in)
}

// give makes the move an answer names, after the checks above. RelayedApproval calls it directly for a click whose
// policy it checked before it spent the ask: the policy counts spent asks, so it must not run again after.
func (s *Server) give(ctx context.Context, c Caller, via, id, answer string, in AnswerIn) (map[string]string, error) {
	out := map[string]string{"case": id}
	switch answer {
	case AnsApprove:
		hash, err := s.ApprovePlan(ctx, c, via, id, strings.TrimSpace(in.Plan), in.Note)
		if err != nil {
			return nil, err
		}
		out["approved_by"], out["plan_hash"] = c.Name, hash
	case AnsReject:
		if err := s.RejectCase(ctx, c, via, id, in.Reason); err != nil {
			return nil, err
		}
		out["rejected_by"] = c.Name
	case AnsClose:
		if err := s.CloseCase(ctx, c, via, id, in.Reason); err != nil {
			return nil, err
		}
		out["closed_by"] = c.Name
	case AnsReassign:
		if err := s.ReassignCase(ctx, c, via, id, in.Reason); err != nil {
			return nil, err
		}
		out["reassigned_by"] = c.Name
	case AnsDismiss:
		if err := s.DismissCase(ctx, c, via, id, in.Reason); err != nil {
			return nil, err
		}
		out["dismissed_by"] = c.Name
	case AnsAcknowledge:
		if err := s.AckNotice(ctx, c, via, id); err != nil {
			return nil, err
		}
		out["acknowledged_by"] = c.Name
	}
	out["answer"] = answer
	return out, nil
}

// AckNotice is a person acknowledging a watched case's newest notice: it records who, and holds the next notice back
// (the case timer's snooze). The case stays watching.
func (s *Server) AckNotice(ctx context.Context, c Caller, via, id string) error {
	if err := s.Check(ctx, c, ActApprove); err != nil {
		return err
	}
	cs, err := s.Store.Case(ctx, id)
	if errors.Is(err, store.ErrNoSuchCase) {
		return problem(404, "no such case")
	}
	if err != nil {
		return err
	}
	if cs.Status != cases.Watching {
		return problem(409, "case %s is %s: only a watching case has a notice to acknowledge", id, cs.Status)
	}
	switch _, err := s.Store.AckNotice(ctx, id, c.Name); {
	case errors.Is(err, store.ErrNoNotice), errors.Is(err, store.ErrNoticeAcked), errors.Is(err, store.ErrNotWatching):
		return problem(409, "case %s: %v", id, err)
	case err != nil:
		return err
	}
	s.Log.Info("notice acknowledged", "case", id, "by", c.Name, "via", via)
	return nil
}

func (s *Server) caseAnswers(w http.ResponseWriter, r *http.Request, c Caller) {
	set, err := s.Answers(r.Context(), c, "api", r.PathValue("id"))
	if err != nil {
		writeAction(w, err, 200, nil)
		return
	}
	writeJSON(w, 200, set)
}

func (s *Server) caseAnswer(w http.ResponseWriter, r *http.Request, c Caller) {
	var body struct {
		Answer string `json:"answer"`
		AnswerIn
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16384)).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]string{"error": "the body is not a JSON answer: " + err.Error()})
		return
	}
	body.Plan, body.Reason = strings.TrimSpace(body.Plan), strings.TrimSpace(body.Reason)
	if n := strings.TrimSpace(body.Note); n != "" {
		body.Note = "via api: " + n // a note from HTTP cannot pass for the Slack connector's
	}
	out, err := s.Answer(r.Context(), c, "api", r.PathValue("id"), body.Answer, body.AnswerIn)
	writeAction(w, err, 200, out)
}

// resolveSlackActor is who a pinned Slack user is as the ledger names people: the pin is the allowlist, and the
// directory, when it knows the Slack user, must agree with it. A person the directory does not know, or knows without
// a flotilla id, is the pinned name.
func (s *Server) resolveSlackActor(ctx context.Context, slackID, pinned string) (string, error) {
	p, known, err := s.Store.PersonBy(ctx, "slack", slackID)
	if err != nil {
		return "", err
	}
	if known {
		// the directory's flotilla id is a bare user name like the pin and the users table (names compare exactly); the
		// ledger's "user:" actor form is the same person
		if id := strings.TrimPrefix(p.IDs["flotilla"], "user:"); id != "" && id != pinned {
			return "", problem(403, "the directory says this Slack user is flotilla user %s, the pin says %s", id, pinned)
		}
	}
	return pinned, nil
}
