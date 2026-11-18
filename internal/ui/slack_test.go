package ui

import (
	"strings"
	"testing"

	"github.com/chipi/flotilla/internal/api"
)

// G9: without a slack section the pages say Slack is not wired; with one, Approvals counts the day's
// use of the cap and Autonomy's guardrail names the connector, the pinned people and the rule.
func TestSlackOnThePages(t *testing.T) {
	f := setup(t)
	ck := f.signin(f.vic)
	if _, page := f.do("GET", "/autonomy", ck, nil, nil); !strings.Contains(page, "not wired: approve here or with the CLI") {
		t.Error("without Slack, the guardrail does not say so")
	}
	f.ui.API.Slack = &api.SlackConfig{ConnectorHost: "backup-01", TeamID: "T0001", Users: map[string]string{"U0OPER": "ada"}, DailyCap: 5, UIURL: "https://f"}
	if _, page := f.do("GET", "/autonomy", ck, nil, nil); !strings.Contains(page, "through backup-01&#39;s connector, 1 person pinned; low or medium risk, reversible or regenerable; 0 of 5 a day used") {
		t.Error("the Slack guardrail")
	}
	if _, page := f.do("GET", "/approvals", ck, nil, nil); !strings.Contains(page, "Slack: 0 of 5 approvals used today.") {
		t.Error("Approvals does not count Slack's cap")
	}
}
