package api

import (
	"log/slog"
	"net/http/httptest"
	"os"
	"testing"
)

func TestLedgerRoles(t *testing.T) {
	ts, _ := server(t)
	for _, tok := range []string{agentTok, opTok} {
		if c, _ := call(t, ts, "GET", "/v1/ledger/filings?ns=triage/cycle", tok, ""); c != 403 {
			t.Errorf("the ledger is a worker's: an agent or the operator got %d", c)
		}
	}
	for _, path := range []string{"/v1/status", "/v1/runs", "/v1/events"} {
		if c, _ := call(t, ts, "GET", path, workerTok, ""); c != 403 {
			t.Errorf("a worker reaches only the ledger: %s gave %d", path, c)
		}
	}
	if c, _ := call(t, ts, "POST", "/v1/routines/triage/cycle/run", workerTok, ""); c != 403 {
		t.Errorf("a worker must not start a run: %d", c)
	}
	if c, _ := call(t, ts, "GET", "/v1/ledger/filings", workerTok, ""); c != 400 {
		t.Errorf("ns is required: %d", c)
	}
}

func TestLedgerRoundTrip(t *testing.T) {
	ts, _ := server(t)
	ns := "?ns=triage/cycle"
	w := func(body string) {
		t.Helper()
		if c, out := call(t, ts, "POST", "/v1/ledger/filings"+ns, workerTok, body); c != 200 {
			t.Fatalf("write filing: %d %v", c, out)
		}
	}
	w(`{"row":{"fingerprint":"a","repo":"o/r","issue":"1","norm_key":"n"},"run_id":"r1"}`)
	w(`{"row":{"fingerprint":"b","repo":"o/r","issue":"2","norm_key":"n"},"run_id":"r1"}`)
	w(`{"row":{"fingerprint":"a","repo":"o/r","issue":"1","norm_key":"m"},"run_id":"r2"}`)
	c, out := call(t, ts, "GET", "/v1/ledger/filings"+ns+"&norm_key=n", workerTok, "")
	fs, _ := out["filings"].([]any)
	if c != 200 || len(fs) != 1 || fs[0].(map[string]any)["row"].(map[string]any)["fingerprint"] != "b" {
		t.Fatalf("lookup by norm_key: %d %v", c, out)
	}
	if c, out := call(t, ts, "GET", "/v1/ledger/filings"+ns+"&title=x", workerTok, ""); c != 400 {
		t.Fatalf("an unknown key: %d %v", c, out)
	}
	if c, out := call(t, ts, "POST", "/v1/ledger/entries"+ns, workerTok, `{"kind":"decision","row":{"occurrence_id":"o1"},"run_id":"r1"}`); c != 200 {
		t.Fatalf("append entry: %d %v", c, out)
	}
	if c, _ := call(t, ts, "POST", "/v1/ledger/entries"+ns, workerTok, `{"kind":"opinion","row":{}}`); c != 400 {
		t.Fatalf("an unknown kind must be refused: %d", c)
	}
	c, out = call(t, ts, "GET", "/v1/ledger/counts"+ns, workerTok, "")
	if c != 200 || out["filings"] != float64(2) || out["decisions"] != float64(1) {
		t.Fatalf("counts: %d %v", c, out)
	}
	if c, out := call(t, ts, "GET", "/v1/ledger/filings?ns=triage/cycle-shadow", workerTok, ""); c != 200 || len(out["filings"].([]any)) != 0 {
		t.Fatalf("namespaces must not share: %d %v", c, out)
	}
}

func TestLedgerImportOnce(t *testing.T) {
	ts, _ := server(t)
	body := `{"filings":[{"fingerprint":"a","issue":"1"}],"entries":[{"kind":"decision","row":{"occurrence_id":"o"}},{"kind":"recurrence","row":{"occurrence_id":"o"}}]}`
	c, out := call(t, ts, "POST", "/v1/ledger/import?ns=x", workerTok, body)
	if c != 200 || out["filings"] != float64(1) || out["decisions"] != float64(1) || out["recurrences"] != float64(1) {
		t.Fatalf("import: %d %v", c, out)
	}
	if c, _ := call(t, ts, "POST", "/v1/ledger/import?ns=x", workerTok, body); c != 409 {
		t.Fatalf("a second import must be refused: %d", c)
	}
	if c, _ := call(t, ts, "POST", "/v1/ledger/import?ns=y", workerTok, `{}`); c != 400 {
		t.Fatalf("an empty import must be refused: %d", c)
	}
}

// The workers' listener serves the ledger and /healthz, and nothing else: the full API stays on loopback.
func TestWorkerHandler(t *testing.T) {
	_, st := server(t)
	srv := &Server{Store: st, Version: "test", Log: slog.New(slog.NewTextHandler(os.Stderr, nil)),
		Callers: append([]Caller{{Name: "operator", Role: RoleOperator, TokenSHA256: HashToken(opTok)}},
			WorkerCallers(map[string]string{"triage": workerTok})...)}
	ts := httptest.NewServer(srv.WorkerHandler())
	t.Cleanup(ts.Close)
	if c, _ := call(t, ts, "GET", "/v1/ledger/counts?ns=x", workerTok, ""); c != 200 {
		t.Fatalf("the ledger on the workers' listener: %d", c)
	}
	for _, p := range []string{"/v1/status", "/v1/runs", "/v1/events"} {
		if c, _ := call(t, ts, "GET", p, opTok, ""); c != 404 {
			t.Errorf("%s must not be served on the workers' listener, even to the operator: %d", p, c)
		}
	}
}
