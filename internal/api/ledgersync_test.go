package api

import (
	"context"
	"testing"

	"github.com/chipi/flotilla/internal/runtoken"
)

const importBody = `{"filings":[{"fingerprint":"a","repo":"o/r","issue":"1"},{"fingerprint":"b","repo":"o/r","issue":"2"}],` +
	`"entries":[{"kind":"decision","row":{"fingerprint":"a","disposition":"file"}},{"kind":"recurrence","row":{"fingerprint":"a"}}]}`

// S3: with run tokens required a worker's own token opens nothing, and the operator still loads and checks a fleet's ledger: the
// import, and the reads ledger_sync check makes, with an operator's token.
func TestTheOperatorImportsAndChecksALedgerWhenWorkerTokensOpenNothing(t *testing.T) {
	ts, srv, reg := runServer(t)
	srv.Callers = srv.Callers[:2] // the operator and the agent: what main.go assembles when run_tokens is required
	if c, _ := call(t, ts, "POST", "/v1/ledger/import?ns=triage/cycle", workerTok, importBody); c != 401 {
		t.Fatalf("the worker's own token opens nothing: %d", c)
	}
	if c, out := call(t, ts, "POST", "/v1/ledger/import?ns=triage/cycle", opTok, importBody); c != 200 || out["filings"] != float64(2) || out["decisions"] != float64(1) || out["recurrences"] != float64(1) {
		t.Fatalf("the operator imports: %d %v", c, out)
	}
	ev, _ := srv.Store.Events(context.Background(), 5)
	if len(ev) == 0 || ev[0].Kind != "ledger.import" || ev[0].Actor != "operator" || ev[0].Subject != "triage/cycle" {
		t.Fatalf("the import is an event naming who: %+v", ev)
	}
	// the reads a check makes
	c, out := call(t, ts, "GET", "/v1/ledger/filings?ns=triage/cycle", opTok, "")
	if c != 200 || len(out["filings"].([]any)) != 2 {
		t.Fatalf("%d %v", c, out)
	}
	if c, out := call(t, ts, "GET", "/v1/ledger/entries?ns=triage/cycle", opTok, ""); c != 200 || len(out["entries"].([]any)) != 2 {
		t.Fatalf("%d %v", c, out)
	}
	if c, out := call(t, ts, "GET", "/v1/ledger/counts?ns=triage/cycle", opTok, ""); c != 200 || out["decisions"] != float64(1) {
		t.Fatalf("%d %v", c, out)
	}
	// an import is once, as ever
	if c, _ := call(t, ts, "POST", "/v1/ledger/import?ns=triage/cycle", opTok, importBody); c != 409 {
		t.Fatalf("a second import: %d", c)
	}
	// an operator names the namespace: without ns it is a 400, not a guess
	if c, _ := call(t, ts, "GET", "/v1/ledger/counts", opTok, ""); c != 400 {
		t.Fatalf("%d", c)
	}
	// a run still reaches only its own, and imports nothing
	run := issue(t, reg, runtoken.Run{ID: "run-1"})
	if c, _ := call(t, ts, "POST", "/v1/ledger/import?ns=triage/cycle", run, importBody); c != 403 {
		t.Fatalf("a run's import: %d", c)
	}
	// an agent reads nothing of it
	if c, _ := call(t, ts, "GET", "/v1/ledger/counts?ns=triage/cycle", agentTok, ""); c != 403 {
		t.Fatalf("an agent: %d", c)
	}
}

// With an admin in the store the callers-file operator token is retired, and so is its ledger access: a person's token is used.
func TestTheRetiredOperatorTokenOpensNoLedgerButAnAdminsDoes(t *testing.T) {
	ts, srv, _ := runServer(t)
	const adaTok = "ada-token-0123456789abcdef0123456789abcdef"
	if err := srv.Store.AddUser(context.Background(), "ada", RoleAdmin, HashToken(adaTok), "operator", "test", true); err != nil {
		t.Fatal(err)
	}
	if c, _ := call(t, ts, "GET", "/v1/ledger/counts?ns=triage/cycle", opTok, ""); c != 403 {
		t.Fatalf("the retired operator token: %d", c)
	}
	if c, out := call(t, ts, "POST", "/v1/ledger/import?ns=triage/cycle", adaTok, importBody); c != 200 {
		t.Fatalf("an admin imports: %d %v", c, out)
	}
	if c, _ := call(t, ts, "GET", "/v1/ledger/counts?ns=triage/cycle", adaTok, ""); c != 200 {
		t.Fatalf("%d", c)
	}
}
