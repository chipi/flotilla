package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/chipi/flotilla/internal/store"
)

func TestParsePromote(t *testing.T) {
	routine, n, req, err := parsePromote([]string{"triage/cycle", "v2", "--autonomy", "propose", "--no-pairs", "--gate", "replay=pass@ci/12", "--gate", "lint=fail",
		"--eval", "run-7=pass", "--eval", "run-8=fail"}, "read the diff")
	if err != nil || routine != "triage/cycle" || n != 2 || req.Autonomy != "propose" || req.Reason != "read the diff" || !req.WithoutPairs {
		t.Fatalf("%q %d %+v %v", routine, n, req, err)
	}
	if len(req.Gates) != 2 || req.Gates[0] != (store.Gate{Name: "replay", Result: "pass", Ref: "ci/12"}) || req.Gates[1] != (store.Gate{Name: "lint", Result: "fail"}) {
		t.Fatalf("gates %+v", req.Gates)
	}
	if len(req.Evals) != 2 || req.Evals[1] != (store.EvalRun{RunID: "run-8", Verdict: "fail"}) {
		t.Fatalf("evals %+v", req.Evals)
	}
	for _, bad := range [][]string{{"triage/cycle"}, {"triage/cycle", "two"}, {"triage/cycle", "v2", "--gate", "x=maybe"}, {"triage/cycle", "v2", "--gate", "x"},
		{"triage/cycle", "v2", "--eval", "=pass"}, {"triage/cycle", "v2", "--eval", "r=ok"}} {
		if _, _, _, err := parsePromote(bad, ""); err == nil {
			t.Errorf("%v parsed", bad)
		}
	}
}

func TestVersionCommandsPostWhatTheAPITakes(t *testing.T) {
	var method, path, body string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		method, path, body = r.Method, r.URL.Path, string(b)
		w.Write([]byte(`{"routine":"triage/cycle","acceptance":["pairs"],"live":1,"versions":[{"version":1,"state":"live","hash":"abcdef0123456789","deployed_at":"t","live_at":"t","state_by":"flotillad","state_why":"first","loaded":true,"pairs":0}]}`))
	}))
	defer ts.Close()
	for _, c := range []struct {
		args       []string
		method     string
		path, body string
	}{
		{[]string{"promote", "triage/cycle", "v2", "--autonomy", "record", "--reason", "x", "--gate", "g=pass"}, "POST", "/v1/routines/triage/cycle/versions/2/promote",
			`{"autonomy":"record","reason":"x","without_pairs":false,"gates":[{"name":"g","result":"pass"}],"evals":null}`},
		{[]string{"rollback", "triage/cycle", "--autonomy", "record", "--reason", "bad"}, "POST", "/v1/routines/triage/cycle/rollback", `{"autonomy":"record","reason":"bad"}`},
		{[]string{"retire", "triage/cycle", "v3", "--reason", "not needed"}, "POST", "/v1/routines/triage/cycle/versions/3/retire", `{"reason":"not needed"}`},
	} {
		if _, errs, code := cli(t, ts.URL, c.args...); code != 0 || method != c.method || path != c.path || body != c.body {
			t.Fatalf("%v: exit %d (%s), sent %s %s %s", c.args, code, errs, method, path, body)
		}
	}
	out, errs, code := cli(t, ts.URL, "versions", "triage/cycle")
	if code != 0 || path != "/v1/routines/triage/cycle/versions" || method != "GET" || !strings.Contains(out, "live v1") || !strings.Contains(out, "abcdef0") || strings.Contains(out, "abcdef01") {
		t.Fatalf("versions: exit %d (%s), %q", code, errs, out)
	}
	if _, errs, code := cli(t, ts.URL, "promote", "triage/cycle", "v2", "--gate", "x=maybe"); code != 2 || !strings.Contains(errs, "NAME=pass|fail") {
		t.Fatalf("a bad gate: exit %d, %q", code, errs)
	}
}

// -json gives the API's answer as it is, for deploy_fleets.sh's before and after proof.
func TestVersionsJSON(t *testing.T) {
	var path string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		w.Write([]byte(`{"routine":"triage/cycle","versions":[]}`))
	}))
	defer ts.Close()
	out, errs, code := cli(t, ts.URL, "versions", "triage/cycle", "-json")
	if code != 0 || path != "/v1/routines/triage/cycle/versions" || out != `{"routine":"triage/cycle","versions":[]}` {
		t.Fatalf("exit %d (%s), %s, %q", code, errs, path, out)
	}
}
