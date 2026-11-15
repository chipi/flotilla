package policy

import "testing"

// The bugfix fleet's executing routine recomputes the plan hash in Python (bugfix/open_pr.py
// plan_hash) to refuse a plan changed on the way. This vector, with the characters Go's JSON escapes
// (<, >, &, U+2028) and others it keeps (UTF-8, an astral emoji), must give the same hash on both
// sides; bugfix/test_open_pr.py asserts the same value.
func TestPlanHashVector(t *testing.T) {
	p := Plan{CaseID: "case-20261115-abc", Action: "github.open_pr", Params: map[string]string{
		"repo":  "chipi/orrery",
		"base":  "abc1234",
		"title": "fix #12: a < b && c > d — été",
		"body":  "line 1\nline \"2\"\t\\   \U0001F680",
		"diff":  "--- a/x\n+++ b/x\n@@ -1 +1 @@\n-<a href=\"x\">&amp;</a>\n+ok\n",
	}}
	if got, want := p.Hash(), "9a41a15da7f89b53fda5fe9ac8777d8f798af5011944cd4eb4c6e8295d63b7f1"; got != want {
		t.Fatalf("Hash() = %s, want %s (bugfix/open_pr.py's plan_hash)", got, want)
	}
}
