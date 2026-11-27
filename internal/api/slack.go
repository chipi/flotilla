package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/chipi/flotilla/internal/cases"
	"github.com/chipi/flotilla/internal/hostwire"
	"github.com/chipi/flotilla/internal/store"
)

// Approvals in Slack (G9; ADR-0005 decision 8, ADR-0006 decided 4, security T16). The connector, a role
// of one host's agent, posts each plan that waits for a person and relays the click. flotillad decides
// everything again here: the relaying host must be the configured connector; the workspace and the
// Slack user must be pinned, and the user mapped to an active flotilla operator or admin; the nonce
// must be one flotillad made and unused; the plan unchanged; the action low or medium risk and
// reversible or regenerable; under the daily cap. High-risk or irreversible plans are answered in the
// UI or the CLI only.

// SlackConfig is flotillad.yaml's slack section.
type SlackConfig struct {
	ConnectorHost string            `yaml:"connector_host"` // the one host whose agent may relay
	TeamID        string            `yaml:"team_id"`        // the pinned workspace
	Users         map[string]string `yaml:"users"`          // Slack user id -> flotilla user name
	DailyCap      int               `yaml:"daily_cap"`      // plans approved from Slack a day (default 10)
	UIURL         string            `yaml:"ui_url"`         // the UI's address, for the message's link
}

// Cap is the daily cap with its default.
func (c *SlackConfig) Cap() int {
	if c == nil || c.DailyCap <= 0 {
		return 10
	}
	return c.DailyCap
}

// SlackApprovable is whether Slack may approve a plan for this action at all, and why not.
func (s *Server) SlackApprovable(action string) (bool, string) {
	if s.Registry == nil {
		return false, "no registry"
	}
	a, ok := s.Registry.Action(action)
	if !ok {
		return false, "the action is not in the registry"
	}
	if a.Risk != "low" && a.Risk != "medium" {
		return false, a.Risk + " risk: answer in the UI or the CLI"
	}
	if a.Reversible != "yes" && a.Reversible != "regenerable" {
		return false, "irreversible: answer in the UI or the CLI"
	}
	return true, ""
}

// SlackNotices makes an ask for every case waiting on its newest plan, and for every watched case whose newest notice
// is due and unsent, and returns the asks the connector has not posted yet (ADR-0023 §3).
func (s *Server) SlackNotices(ctx context.Context) ([]hostwire.NotifyBody, error) {
	if s.Slack == nil {
		return nil, nil
	}
	waiting, err := s.Store.Cases(ctx, store.CaseFilter{Status: cases.AwaitingApproval, Limit: 1000})
	if err != nil {
		return nil, err
	}
	for _, c := range waiting {
		plans, err := s.Store.Plans(ctx, c.ID)
		if err != nil {
			return nil, err
		}
		if len(plans) == 0 {
			continue
		}
		if _, err := s.Store.AskInSlack(ctx, c.ID, plans[len(plans)-1].PlanHash); err != nil {
			return nil, err
		}
	}
	watched, err := s.Store.Cases(ctx, store.CaseFilter{Status: cases.Watching, Limit: 1000})
	if err != nil {
		return nil, err
	}
	for _, c := range watched {
		n, ok, err := s.dueNotice(ctx, c)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		if _, err := s.Store.AskNoticeInSlack(ctx, c.ID, n.ID, []string{AnsAcknowledge}); err != nil {
			return nil, err
		}
	}
	asks, err := s.Store.UnsentAsks(ctx)
	if err != nil {
		return nil, err
	}
	var out []hostwire.NotifyBody
	for _, a := range asks {
		c, err := s.Store.Case(ctx, a.CaseID)
		if err != nil {
			return nil, err
		}
		if a.Kind == store.AskNotice {
			n, ok, err := s.dueNotice(ctx, c)
			if err != nil {
				return nil, err
			}
			if !ok || n.ID != a.NoticeID() {
				continue // acknowledged, closed or superseded before it was posted
			}
			detail, err := s.noticeDetail(ctx, c)
			if err != nil {
				return nil, err
			}
			out = append(out, hostwire.NotifyBody{Nonce: a.Nonce, Case: c.ID, Title: caseTitle(c), Plan: detail, URL: s.caseURL(c), Kind: hostwire.KindNotice,
				Repeat: n.N > 1, Answers: []hostwire.NotifyAnswer{{ID: AnsAcknowledge, Label: "Acknowledge", Allowed: true}}})
			continue
		}
		if c.Status != cases.AwaitingApproval {
			continue // answered elsewhere before it was posted
		}
		plans, err := s.Store.Plans(ctx, c.ID)
		if err != nil {
			return nil, err
		}
		if len(plans) == 0 || plans[len(plans)-1].PlanHash != a.PlanHash {
			continue
		}
		p := plans[len(plans)-1]
		ok, why := s.SlackApprovable(p.Action)
		// A plan Slack may not approve offers nothing from Slack, as before: the message says where to answer.
		out = append(out, hostwire.NotifyBody{Nonce: a.Nonce, Case: c.ID, Title: caseTitle(c), Plan: p.Action + " on " + c.Target,
			PlanHash: shortHash(p.PlanHash), URL: s.caseURL(c), Approvable: ok, Why: why, Kind: hostwire.KindPlan,
			Answers: []hostwire.NotifyAnswer{
				{ID: AnsApprove, Label: "Approve", Style: "primary", Allowed: ok, Why: why},
				{ID: AnsReject, Label: "Reject", Style: "danger", Allowed: ok, Why: why}}})
	}
	return out, nil
}

func caseTitle(c store.Case) string {
	if c.Title != "" {
		return c.Title
	}
	return c.Kind + " on " + c.Target
}

func (s *Server) caseURL(c store.Case) string {
	return strings.TrimRight(s.Slack.UIURL, "/") + "/cases/" + c.ID
}

// dueNotice is a watching case's newest notice, when it belongs to this watching period and is unsent and
// unacknowledged: the one a connector should be told about.
func (s *Server) dueNotice(ctx context.Context, c store.Case) (store.Notice, bool, error) {
	ns, err := s.Store.Notices(ctx, c.ID)
	if err != nil || len(ns) == 0 {
		return store.Notice{}, false, err
	}
	n := ns[len(ns)-1]
	start, serr := time.Parse(time.RFC3339Nano, c.StatusAt)
	due, derr := time.Parse(time.RFC3339Nano, n.DueAt)
	if serr != nil || derr != nil || due.Before(start) || n.Kind != store.NoticeWatching || n.SentAt != "" || n.AckedAt != "" {
		return store.Notice{}, false, nil
	}
	return n, true, nil
}

// noticeDetail is the line a notice shows under the title: what is watched, how often it repeated, for how long, and
// what the plan on record would have done.
func (s *Server) noticeDetail(ctx context.Context, c store.Case) (string, error) {
	d := "watched: " + c.Kind + " on " + c.Target
	if c.Repeats > 0 {
		d += fmt.Sprintf(", repeated %d times unchanged", c.Repeats)
	}
	if opened, err := time.Parse(time.RFC3339Nano, c.OpenedAt); err == nil {
		now := time.Now()
		if s.Sched != nil {
			now = s.Sched.Clock()
		}
		d += ", open " + now.Sub(opened).Round(time.Minute).String()
	}
	plans, err := s.Store.Plans(ctx, c.ID)
	if err != nil {
		return "", err
	}
	if len(plans) > 0 {
		d += "; the recorded plan would have run " + plans[len(plans)-1].Action + " on " + c.Target
	}
	return d, nil
}

// RelayedAnswer answers a click the connector relayed: any answer the ask offered (ADR-0023 §3), checked again here.
func (s *Server) RelayedAnswer(ctx context.Context, host string, b hostwire.ApprovalBody) hostwire.AnsweredBody {
	ans := func(ok bool, f string, a ...any) hostwire.AnsweredBody {
		text := fmt.Sprintf(f, a...)
		if !ok {
			s.Log.Warn("slack approval refused", "host", host, "slack_user", b.User, "team", b.Team, "nonce", shortHash(b.Nonce), "why", text)
		}
		return hostwire.AnsweredBody{Nonce: b.Nonce, OK: ok, Text: text}
	}
	if s.Slack == nil || host != s.Slack.ConnectorHost {
		return ans(false, "refused: %s is not this installation's Slack connector", host)
	}
	if s.Slack.TeamID == "" || b.Team != s.Slack.TeamID {
		return ans(false, "refused: not the pinned Slack workspace")
	}
	name, ok := s.Slack.Users[b.User]
	if !ok {
		return ans(false, "refused: this Slack user is not pinned to a flotilla user")
	}
	users, err := s.Store.Users(ctx)
	if err != nil {
		return ans(false, "refused: %v", err)
	}
	var u *store.User
	for i := range users {
		if users[i].Name == name {
			u = &users[i]
		}
	}
	if u == nil || u.RevokedAt != nil || (u.Role != store.UserOperator && u.Role != store.UserAdmin) {
		return ans(false, "refused: %s is not an active operator", name)
	}
	ask, err := s.Store.SlackAskByNonce(ctx, b.Nonce)
	if errors.Is(err, sql.ErrNoRows) {
		return ans(false, "refused: this message was not made by flotilla")
	}
	if err != nil {
		return ans(false, "refused: %v", err)
	}
	if ask.DecidedAt != "" {
		return ans(false, "already answered: %s by %s", ask.Outcome, strings.TrimPrefix(ask.DecidedBy, "user:"))
	}
	// the click is checked against what this message offered, never against what the connector drew
	if !ask.Offers(b.Action) {
		return ans(false, "refused: unknown action %q: this message does not offer it", b.Action)
	}
	if ask.Kind == store.AskPlan {
		plans, err := s.Store.Plans(ctx, ask.CaseID)
		if err != nil {
			return ans(false, "refused: %v", err)
		}
		if len(plans) == 0 || plans[len(plans)-1].PlanHash != ask.PlanHash {
			return ans(false, "refused: the plan changed since this message; open the case")
		}
	}
	actor, err := s.resolveSlackActor(ctx, b.User, name)
	if err != nil {
		if pr := AsProblem(err); pr != nil {
			return ans(false, "refused: %s", pr.Msg)
		}
		return ans(false, "refused: %v", err)
	}
	c := Caller{Name: "user:" + actor, Role: u.Role, User: true}
	switch b.Action {
	case AnsApprove:
		cs, err := s.Store.Case(ctx, ask.CaseID)
		if err != nil {
			return ans(false, "refused: %v", err)
		}
		if ok, why, err := s.answerPolicy(ctx, cs, c, "slack", AnsApprove, AnswerIn{Plan: ask.PlanHash}); err != nil {
			return ans(false, "refused: %v", err)
		} else if !ok {
			return ans(false, "refused: %s", why)
		}
		// the cap is checked again as the ask is spent: two clicks at cap-1 must not both pass
		if err := s.Store.UseAskCapped(ctx, b.Nonce, c.Name, "approved", s.slackDayStart(), s.Slack.Cap()); errors.Is(err, store.ErrCapReached) {
			return ans(false, "refused: Slack's daily cap (%d) is used; answer in the UI or the CLI", s.Slack.Cap())
		} else if err != nil {
			return ans(false, "refused: %v", err)
		}
		if _, err := s.give(ctx, c, "slack", ask.CaseID, AnsApprove, AnswerIn{Plan: ask.PlanHash, Note: "from Slack"}); err != nil {
			_ = s.Store.ReopenAsk(ctx, b.Nonce)
			return ans(false, "not approved: %v", err)
		}
		return ans(true, "approved by %s in Slack; it runs now", u.Name)
	case AnsReject:
		if err := s.Store.UseAsk(ctx, b.Nonce, c.Name, "rejected"); err != nil {
			return ans(false, "refused: %v", err)
		}
		if _, err := s.give(ctx, c, "slack", ask.CaseID, AnsReject, AnswerIn{Reason: "rejected in Slack"}); err != nil {
			_ = s.Store.ReopenAsk(ctx, b.Nonce)
			return ans(false, "not rejected: %v", err)
		}
		return ans(true, "rejected by %s in Slack", u.Name)
	case AnsAcknowledge:
		if ask.Kind != store.AskNotice {
			return ans(false, "refused: unknown action %q", b.Action)
		}
		if err := s.Store.UseAsk(ctx, b.Nonce, c.Name, "acknowledged"); err != nil {
			return ans(false, "refused: %v", err)
		}
		if _, err := s.Answer(ctx, c, "slack", ask.CaseID, AnsAcknowledge, AnswerIn{}); err != nil {
			_ = s.Store.ReopenAsk(ctx, b.Nonce)
			if pr := AsProblem(err); pr != nil {
				return ans(false, "not acknowledged: %s", pr.Msg)
			}
			return ans(false, "not acknowledged: %v", err)
		}
		// the message clicked is spent, and so is any earlier message of the same case's notices
		if err := s.Store.CloseNoticeAsks(ctx, ask.CaseID, c.Name, "acknowledged"); err != nil {
			s.Log.Warn("slack notice asks not closed", "case", ask.CaseID, "err", err)
		}
		return ans(true, "acknowledged by %s in Slack; the case stays watching", u.Name)
	}
	return ans(false, "refused: unknown action %q", b.Action)
}
