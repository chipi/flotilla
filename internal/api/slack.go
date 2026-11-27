package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

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

// SlackNotices makes an ask for every case waiting on its newest plan and returns the asks the connector
// has not posted yet.
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
		title := c.Title
		if title == "" {
			title = c.Kind + " on " + c.Target
		}
		out = append(out, hostwire.NotifyBody{Nonce: a.Nonce, Case: c.ID, Title: title, Plan: p.Action + " on " + c.Target,
			PlanHash: shortHash(p.PlanHash), URL: strings.TrimRight(s.Slack.UIURL, "/") + "/cases/" + c.ID, Approvable: ok, Why: why})
	}
	return out, nil
}

// RelayedApproval answers a click the connector relayed.
func (s *Server) RelayedApproval(ctx context.Context, host string, b hostwire.ApprovalBody) hostwire.AnsweredBody {
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
	plans, err := s.Store.Plans(ctx, ask.CaseID)
	if err != nil {
		return ans(false, "refused: %v", err)
	}
	if len(plans) == 0 || plans[len(plans)-1].PlanHash != ask.PlanHash {
		return ans(false, "refused: the plan changed since this message; open the case")
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
	}
	return ans(false, "refused: unknown action %q", b.Action)
}
