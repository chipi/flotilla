package ui

import (
	"context"
	"fmt"
	"time"

	"github.com/chipi/flotilla/internal/store"
)

// Approvals in Slack (G9) as the pages show them: the day's use of the cap, and the guardrail.

func (u *Server) slackUsed(ctx context.Context) (int, error) {
	n := u.clockNow().In(u.loc())
	day := time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, u.loc())
	return u.API.Store.SlackApprovalsSince(ctx, day.UTC().Format(store.TimeFormat))
}

// slackToday is Approvals' line on Slack's daily cap; "" when Slack is not wired.
func (u *Server) slackToday(ctx context.Context) (string, error) {
	if u.API.Slack == nil {
		return "", nil
	}
	used, err := u.slackUsed(ctx)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Slack: %d of %d approvals used today.", used, u.API.Slack.Cap()), nil
}

func (u *Server) slackGuard(ctx context.Context) string {
	sl := u.API.Slack
	if sl == nil {
		return "not wired: approve here or with the CLI"
	}
	used, err := u.slackUsed(ctx)
	if err != nil {
		return "unknown: " + err.Error()
	}
	return fmt.Sprintf("through %s's connector, %s pinned; low or medium risk, reversible or regenerable; %d of %d a day used",
		sl.ConnectorHost, plural(len(sl.Users), "person", "people"), used, sl.Cap())
}
