package api

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chipi/flotilla/internal/cases"
	"github.com/chipi/flotilla/internal/hostwire"
	"github.com/chipi/flotilla/internal/registry"
	"github.com/chipi/flotilla/internal/spec"
	"github.com/chipi/flotilla/internal/store"
)

func slackServer(t *testing.T) *Server {
	t.Helper()
	s := newServer(t, func(*spec.Routine) {})
	s.Slack = &SlackConfig{ConnectorHost: "backup-01", TeamID: "T0001", Users: map[string]string{"U0OPER": "ada", "U0VIEW": "vic", "U0GONE": "gone"},
		DailyCap: 2, UIURL: "https://flotilla.example/"}
	ctx := context.Background()
	for _, u := range []struct{ name, role string }{{"ada", store.UserOperator}, {"vic", store.UserViewer}, {"gone", store.UserOperator}} {
		if err := s.Store.AddUser(ctx, u.name, u.role, HashToken("t-"+u.name), "operator", "test", false); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Store.RevokeUser(ctx, "gone", "operator", "test"); err != nil {
		t.Fatal(err)
	}
	return s
}

// The notice for a waiting plan: one ask per plan, a link to the case, approvable by policy.
func TestSlackNotices(t *testing.T) {
	ctx := context.Background()
	s := slackServer(t)
	id := slackCase(t, s, "n")
	ns, err := s.SlackNotices(ctx)
	if err != nil || len(ns) != 1 {
		t.Fatalf("notices: %v %v", ns, err)
	}
	n := ns[0]
	if n.Case != id || !n.Approvable || n.URL != "https://flotilla.example/cases/"+id || n.Title != "backup n" || len(n.Nonce) != 32 || n.PlanHash != "hash-n-01234" {
		t.Fatalf("the notice: %+v", n)
	}
	if again, _ := s.SlackNotices(ctx); len(again) != 1 || again[0].Nonce != n.Nonce {
		t.Fatal("a second pass made a second ask for the same plan")
	}
	_ = s.Store.MarkAskSent(ctx, n.Nonce)
	if again, _ := s.SlackNotices(ctx); len(again) != 0 {
		t.Fatal("a sent ask was sent again")
	}
}

// Every rule a relayed click must pass, then an approval and a rejection that go through.
func TestRelayedApproval(t *testing.T) {
	ctx := context.Background()
	s := slackServer(t)
	id := slackCase(t, s, "r")
	ns, _ := s.SlackNotices(ctx)
	nonce := ns[0].Nonce
	ok := hostwire.ApprovalBody{Nonce: nonce, Team: "T0001", User: "U0OPER", Action: "approve"}
	for _, c := range []struct {
		what string
		host string
		b    hostwire.ApprovalBody
		want string
	}{
		{"another host relays", "model-host", ok, "not this installation's Slack connector"},
		{"another workspace", "backup-01", hostwire.ApprovalBody{Nonce: nonce, Team: "T9999", User: "U0OPER", Action: "approve"}, "not the pinned Slack workspace"},
		{"an unpinned user", "backup-01", hostwire.ApprovalBody{Nonce: nonce, Team: "T0001", User: "U0OTHER", Action: "approve"}, "not pinned"},
		{"a viewer", "backup-01", hostwire.ApprovalBody{Nonce: nonce, Team: "T0001", User: "U0VIEW", Action: "approve"}, "not an active operator"},
		{"a revoked user", "backup-01", hostwire.ApprovalBody{Nonce: nonce, Team: "T0001", User: "U0GONE", Action: "approve"}, "not an active operator"},
		{"a nonce flotilla never made", "backup-01", hostwire.ApprovalBody{Nonce: "beef", Team: "T0001", User: "U0OPER", Action: "approve"}, "not made by flotilla"},
		{"an unknown action", "backup-01", hostwire.ApprovalBody{Nonce: nonce, Team: "T0001", User: "U0OPER", Action: "delete"}, "unknown action"},
	} {
		if a := s.RelayedApproval(ctx, c.host, c.b); a.OK || !strings.Contains(a.Text, c.want) {
			t.Errorf("%s: %+v", c.what, a)
		}
	}
	if c := caseStatus(t, s, id); c.Status != cases.AwaitingApproval {
		t.Fatalf("a refused click moved the case: %s", c.Status)
	}
	if a := s.RelayedApproval(ctx, "backup-01", ok); !a.OK || !strings.Contains(a.Text, "approved by ada") {
		t.Fatalf("the approval: %+v", a)
	}
	if c := caseStatus(t, s, id); c.Status != cases.Executing {
		t.Fatalf("after the approval: %s", c.Status)
	}
	plans, _ := s.Store.Plans(ctx, id)
	if who, err := s.approvedBy(ctx, caseStatus(t, s, id), plans[len(plans)-1]); err != nil || who != "user:ada" {
		t.Fatalf("approved by %q %v", who, err)
	}
	if a := s.RelayedApproval(ctx, "backup-01", ok); a.OK || !strings.Contains(a.Text, "already answered: approved by ada") {
		t.Fatalf("a second click: %+v", a)
	}

	// a rejection, and a plan that changed after its message
	rid := slackCase(t, s, "j")
	ns, _ = s.SlackNotices(ctx)
	if a := s.RelayedApproval(ctx, "backup-01", hostwire.ApprovalBody{Nonce: ns[0].Nonce, Team: "T0001", User: "U0OPER", Action: "reject"}); !a.OK {
		t.Fatalf("the rejection: %+v", a)
	}
	if c := caseStatus(t, s, rid); c.Status != cases.Escalated {
		t.Fatalf("after the rejection: %s", c.Status)
	}
	cid := slackCase(t, s, "c")
	old, err := s.Store.AskInSlack(ctx, cid, "an-earlier-plan-hash") // a message about a plan the case no longer has
	if err != nil {
		t.Fatal(err)
	}
	if a := s.RelayedApproval(ctx, "backup-01", hostwire.ApprovalBody{Nonce: old.Nonce, Team: "T0001", User: "U0OPER", Action: "approve"}); a.OK || !strings.Contains(a.Text, "the plan changed") {
		t.Fatalf("a changed plan: %+v", a)
	}
}

// The daily cap counts approvals from Slack; past it Slack answers nothing.
func TestSlackDailyCap(t *testing.T) {
	ctx := context.Background()
	s := slackServer(t)
	for i, key := range []string{"a", "b", "c"} {
		id := slackCase(t, s, key)
		var nonce string
		ns, _ := s.SlackNotices(ctx)
		for _, n := range ns {
			if n.Case == id {
				nonce = n.Nonce
			}
		}
		a := s.RelayedApproval(ctx, "backup-01", hostwire.ApprovalBody{Nonce: nonce, Team: "T0001", User: "U0OPER", Action: "approve"})
		if i < 2 && !a.OK {
			t.Fatalf("approval %d under the cap: %+v", i+1, a)
		}
		if i == 2 && (a.OK || !strings.Contains(a.Text, "daily cap (2)")) {
			t.Fatalf("the third approval, over the cap: %+v", a)
		}
	}
}

// High risk or irreversible is never approvable from Slack.
func TestSlackApprovable(t *testing.T) {
	dir := t.TempDir()
	reg := `{"component": "host:actions", "actions": [
	  {"name": "a.low", "for_kinds": ["k"], "params": {"host": {"kind": "target_host"}}, "risk": "low", "reversible": "yes", "pre": ["not_running"], "pre_shadow": ["not_running"], "check": {"verify": "v", "within": "5m"}, "limits": {"per_target_per_day": 1}, "timeout": "1m"},
	  {"name": "a.high", "for_kinds": ["k"], "params": {"host": {"kind": "target_host"}}, "risk": "high", "reversible": "yes", "pre": ["not_running"], "pre_shadow": ["not_running"], "check": {"verify": "v", "within": "5m"}, "limits": {"per_target_per_day": 1}, "timeout": "1m"},
	  {"name": "a.gone", "for_kinds": ["k"], "params": {"host": {"kind": "target_host"}}, "risk": "medium", "reversible": "no", "pre": ["not_running"], "pre_shadow": ["not_running"], "check": {"verify": "v", "within": "5m"}, "limits": {"per_target_per_day": 1}, "timeout": "1m"}]}`
	if err := os.WriteFile(filepath.Join(dir, "r.json"), []byte(reg), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := registry.Load(filepath.Join(dir, "r.json"))
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{Registry: r}
	for name, want := range map[string]bool{"a.low": true, "a.high": false, "a.gone": false, "a.none": false} {
		if ok, why := s.SlackApprovable(name); ok != want {
			t.Errorf("%s: approvable %v (%s), want %v", name, ok, why, want)
		}
	}
}

// slackCase is proposedCase on its own host (host:<key>), so approving one leaves the others free.
func slackCase(t *testing.T, s *Server, key string) string {
	t.Helper()
	ctx := context.Background()
	k := store.Key{Kind: "target", Value: "failed_backup/host:" + key}
	id, _, err := s.Store.OpenCase(ctx, store.NewCase{Kind: "failed_backup", Target: "host:" + key, Keys: []store.Key{k}, Canonical: k, Title: "backup " + key}, "m", "")
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Store.MoveCase(ctx, id, cases.HandedOver, cases.ByDetector, "", "m", "")
	_ = s.Store.MoveCase(ctx, id, cases.Planned, cases.ByAct, "", "a", "")
	if _, err := s.Store.ApplyPlan(ctx, store.PlanRecord{CaseID: id, Action: rerun, Params: map[string]string{"host": key},
		PlanHash: "hash-" + key + "-0123456789", Outcome: "propose", Autonomy: "propose"}, cases.AwaitingApproval, "", "host:"+key, true); err != nil {
		t.Fatal(err)
	}
	return id
}

// A relayed approval of a plan Slack may not approve is refused, even with a message flotilla made.
func TestRelayedApprovalOfAHighRiskPlan(t *testing.T) {
	ctx := context.Background()
	s := slackServer(t)
	dir := t.TempDir()
	reg := `{"component": "host:actions", "actions": [{"name": "backup.rerun", "for_kinds": ["failed_backup"],
	  "params": {"host": {"kind": "target_host"}}, "risk": "high", "reversible": "regenerable", "pre": ["not_running"], "pre_shadow": ["not_running"],
	  "check": {"verify": "v", "within": "5m"}, "limits": {"per_target_per_day": 1}, "timeout": "1m"}]}`
	if err := os.WriteFile(filepath.Join(dir, "r.json"), []byte(reg), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := registry.Load(filepath.Join(dir, "r.json"))
	if err != nil {
		t.Fatal(err)
	}
	s.Registry = r
	id := slackCase(t, s, "h")
	ns, _ := s.SlackNotices(ctx)
	if len(ns) != 1 || ns[0].Approvable || !strings.Contains(ns[0].Why, "high risk") {
		t.Fatalf("the notice of a high-risk plan: %+v", ns)
	}
	if a := s.RelayedApproval(ctx, "backup-01", hostwire.ApprovalBody{Nonce: ns[0].Nonce, Team: "T0001", User: "U0OPER", Action: "approve"}); a.OK || !strings.Contains(a.Text, "high risk") {
		t.Fatalf("approving a high-risk plan from Slack: %+v", a)
	}
	if c := caseStatus(t, s, id); c.Status != cases.AwaitingApproval {
		t.Fatalf("the case moved: %s", c.Status)
	}
}
