package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

const pairsJSON = `{"routine":"triage/cycle","version":2,"live_version":1,"paired":2,"differ":1,"unpaired":1,"texts":"x","pairs":[` +
	`{"input_key":"live-2","subject":"fp-a","at":"t","live":{"version":1,"outcome":"file","text":"file"},"shadow":{"version":2,"outcome":"dismiss","text":"dismiss"},"same":false},` +
	`{"input_key":"live-1","subject":"fp-b","at":"t","live":{"version":1,"outcome":"file","text":"file"},"shadow":{"version":2,"outcome":"file","text":"file"},"same":true},` +
	`{"input_key":"live-1","subject":"fp-c","at":"t","shadow":{"version":2,"outcome":"escalate","text":"escalate"},"same":false}]}`

func pairsAPI(t *testing.T, seen *last, n *atomic.Int32) *httptest.Server {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.record(r)
		n.Add(1)
		w.Write([]byte(pairsJSON))
	}))
	t.Cleanup(ts.Close)
	return ts
}

func TestPairsCommandPrintsThePairs(t *testing.T) {
	var seen last
	var n atomic.Int32
	ts := pairsAPI(t, &seen, &n)
	out, errs, code := cli(t, ts.URL, "pairs", "triage/cycle", "v2", "--since", "2026-11-19T00:00:00Z", "--show", "differ")
	if code != 0 {
		t.Fatalf("exit %d, %q", code, errs)
	}
	for _, want := range []string{"triage/cycle · v2 shadow vs v1 live: 2 pairs · 1 differ · 1 unpaired", "differ", "same", "unpaired", "fp-a", "dismiss", "fp-c"} {
		if !strings.Contains(out, want) {
			t.Errorf("the table lacks %q: %s", want, out)
		}
	}
	method, path, auth, _ := seen.get()
	if method != "GET" || path != "/v1/routines/triage/cycle/pairs" || auth != "Bearer op-token" {
		t.Fatalf("%s %s %q", method, path, auth)
	}
	if out, _, _ := cli(t, ts.URL, "pairs", "triage/cycle", "v2", "--json"); !strings.HasPrefix(out, `{"routine":"triage/cycle"`) {
		t.Fatalf("--json is the API's answer: %s", out)
	}
	if _, errs, code := cli(t, ts.URL, "pairs", "triage/cycle", "two"); code != 2 || !strings.Contains(errs, "vN") {
		t.Fatalf("a bad version: %d %q", code, errs)
	}
	if _, errs, code := cli(t, ts.URL, "pairs"); code != 2 || !strings.Contains(errs, "usage") {
		t.Fatalf("%d %q", code, errs)
	}
}

func TestPairsExportWritesAPrivateFolderOutsideARepo(t *testing.T) {
	var seen last
	var n atomic.Int32
	ts := pairsAPI(t, &seen, &n)
	dir := filepath.Join(t.TempDir(), "pairs")
	out, errs, code := cli(t, ts.URL, "pairs", "export", "triage/cycle", "v2", "--to", dir, "--until", "2026-11-20T00:00:00Z")
	if code != 0 || !strings.Contains(out, "exported the pairs of triage/cycle v2") {
		t.Fatalf("exit %d, %q, %q", code, out, errs)
	}
	for name, want := range map[string]os.FileMode{"": 0o700, "pairs.json": 0o600, "README.txt": 0o600} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil || info.Mode().Perm() != want {
			t.Fatalf("%q: %v %v", name, info, err)
		}
	}
	b, _ := os.ReadFile(filepath.Join(dir, "pairs.json"))
	if !strings.Contains(string(b), `"paired": 2`) {
		t.Fatalf("%s", b)
	}
	// a repository's folder is refused before anything is fetched
	repo := t.TempDir()
	os.Mkdir(filepath.Join(repo, ".git"), 0o700)
	before := n.Load()
	if _, errs, code := cli(t, ts.URL, "pairs", "export", "triage/cycle", "v2", "--to", filepath.Join(repo, "x")); code != 2 || !strings.Contains(errs, "git work tree") || n.Load() != before {
		t.Fatalf("exit %d %q, requests %d -> %d", code, errs, before, n.Load())
	}
	if _, errs, code := cli(t, ts.URL, "pairs", "export", "triage/cycle", "v2"); code != 2 || !strings.Contains(errs, "usage") {
		t.Fatalf("no --to: %d %q", code, errs)
	}
}
