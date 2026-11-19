package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/chipi/flotilla/internal/input"
)

const inputView = `{"envelope":{"schema_version":1,"input_key":"live-1","routine":"triage/cycle","kind":"tick","version":1,"trigger":"schedule",` +
	`"started_at":"2026-11-19T10:00:00.000000000Z","tick_time":"2026-11-19T10:00:00.000000000Z","ledger":{"mark_filings":3,"mark_entries":4},` +
	`"live_case_keys":[],"state_snapshot":{"paths":["state"]}},"snapshot_state":"held","pinned":false,"shadow_runs":[]}`

// inputAPI stands in for flotillad's /v1/inputs: the envelope, the same with ledger rows when asked, and a state archive.
func inputAPI(t *testing.T, requests *atomic.Int32, stateStatus int) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("Authorization") != "Bearer op-token" {
			w.WriteHeader(401)
			return
		}
		switch {
		case r.URL.Path == "/v1/inputs/live-1" && r.Method == "GET":
			v := inputView
			if r.URL.Query().Get("ledger") == "1" {
				v = strings.Replace(v, `"mark_entries":4}`, `"mark_entries":4,"filings":[{"fingerprint":"a","issue":"1"}],"entries":[{"kind":"decision","row":{"fingerprint":"a"}}]}`, 1)
			}
			w.Write([]byte(v))
		case r.URL.Path == "/v1/inputs/live-1/state":
			if stateStatus != 200 {
				w.WriteHeader(stateStatus)
				w.Write([]byte(`{"error":"the input's state snapshot is deleted: pin an input before its shadows end to keep it"}`))
				return
			}
			w.Header().Set("Content-Type", "application/gzip")
			w.Write([]byte("tar.gz bytes"))
		case strings.HasSuffix(r.URL.Path, "/pin") || strings.HasSuffix(r.URL.Path, "/unpin"):
			w.Write([]byte(`{"input":"live-1","pinned":"true"}`))
		default:
			w.WriteHeader(404)
			w.Write([]byte(`{"error":"no such input"}`))
		}
	}))
	t.Cleanup(ts.Close)
	return ts
}

func TestInputExportWritesAPrivateFolder(t *testing.T) {
	var n atomic.Int32
	ts := inputAPI(t, &n, 200)
	dir := filepath.Join(t.TempDir(), "exports", "live-1")
	out, errs, code := cli(t, ts.URL, "input", "export", "live-1", "--to", dir, "--with-ledger", "--with-state")
	if code != 0 || !strings.Contains(out, "exported input live-1") || !strings.Contains(out, "production data") {
		t.Fatalf("exit %d, %q, %q", code, out, errs)
	}
	for name, want := range map[string]os.FileMode{"": 0o700, input.EnvelopeFile: 0o600, input.StateFile: 0o600, input.ReadmeFile: 0o600} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil || info.Mode().Perm() != want {
			t.Fatalf("%q: %v %v", name, info, err)
		}
	}
	raw, _ := os.ReadFile(filepath.Join(dir, input.EnvelopeFile))
	e, err := input.Decode(raw)
	if err != nil || e.InputKey != "live-1" || len(e.Ledger.Filings) != 1 || len(e.Ledger.Entries) != 1 || e.StateSnapshot.Tar != input.StateFile {
		t.Fatalf("the envelope with its ledger rows and its archive: %+v %v", e, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, input.StateFile)); string(b) != "tar.gz bytes" {
		t.Fatalf("%q", b)
	}
}

func TestInputExportWithoutTheOptionalPartsHasNeither(t *testing.T) {
	var n atomic.Int32
	ts := inputAPI(t, &n, 200)
	dir := filepath.Join(t.TempDir(), "x")
	// flags may come before the key
	if _, errs, code := cli(t, ts.URL, "input", "export", "--to", dir, "live-1"); code != 0 {
		t.Fatalf("exit %d, %q", code, errs)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, input.EnvelopeFile))
	e, err := input.Decode(raw)
	if err != nil || e.Ledger.Filings != nil || e.StateSnapshot.Tar != "" {
		t.Fatalf("%+v %v", e, err)
	}
	if _, err := os.Stat(filepath.Join(dir, input.StateFile)); err == nil {
		t.Fatal("a state archive nobody asked for")
	}
}

// A folder inside a git work tree is refused before anything is fetched from flotillad.
func TestInputExportRefusesAFolderInARepoBeforeFetchingAnything(t *testing.T) {
	var n atomic.Int32
	ts := inputAPI(t, &n, 200)
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(repo, "exports")
	_, errs, code := cli(t, ts.URL, "input", "export", "live-1", "--to", dir)
	if code != 2 || !strings.Contains(errs, "inside a git work tree") {
		t.Fatalf("exit %d, %q", code, errs)
	}
	if n.Load() != 0 {
		t.Fatalf("flotillad was asked for production data (%d requests) for a folder that was going to be refused", n.Load())
	}
	if _, err := os.Stat(dir); err == nil {
		t.Fatal("a folder was made in the repository")
	}
}

func TestInputExportFailuresLeaveNothing(t *testing.T) {
	var n atomic.Int32
	ts := inputAPI(t, &n, 409)
	dir := filepath.Join(t.TempDir(), "x")
	_, errs, code := cli(t, ts.URL, "input", "export", "live-1", "--to", dir, "--with-state")
	if code != 2 || !strings.Contains(errs, "HTTP 409") || !strings.Contains(errs, "pin an input") {
		t.Fatalf("a deleted snapshot says how to keep one: exit %d, %q", code, errs)
	}
	if _, err := os.Stat(dir); err == nil {
		t.Fatal("a refused export left a folder")
	}
	_, errs, code = cli(t, ts.URL, "input", "export", "nope", "--to", dir)
	if code != 2 || !strings.Contains(errs, "HTTP 404") {
		t.Fatalf("an unknown input: exit %d, %q", code, errs)
	}
	if _, err := os.Stat(dir); err == nil {
		t.Fatal("a refused export left a folder")
	}
	if _, errs, code := cli(t, ts.URL, "input", "export", "live-1"); code != 2 || !strings.Contains(errs, "usage") {
		t.Fatalf("no --to: exit %d, %q", code, errs)
	}
}

func TestInputShowPinUnpin(t *testing.T) {
	var n atomic.Int32
	var seen last
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.record(r)
		n.Add(1)
		if r.URL.Path == "/v1/inputs/live-1" {
			w.Write([]byte(inputView))
			return
		}
		w.Write([]byte(`{"input":"live-1","pinned":"true"}`))
	}))
	defer ts.Close()
	out, errs, code := cli(t, ts.URL, "input", "show", "live-1")
	var v struct {
		Envelope map[string]any `json:"envelope"`
	}
	if code != 0 || json.Unmarshal([]byte(out), &v) != nil || v.Envelope["input_key"] != "live-1" {
		t.Fatalf("show: exit %d, %q, %q", code, out, errs)
	}
	if _, _, code := cli(t, ts.URL, "input", "show", "live-1", "--ledger"); code != 0 {
		t.Fatal(code)
	}
	for _, sub := range []string{"pin", "unpin"} {
		if _, errs, code := cli(t, ts.URL, "input", sub, "live-1"); code != 0 {
			t.Fatalf("%s: %d %q", sub, code, errs)
		}
		if method, path, auth, _ := seen.get(); method != "POST" || path != "/v1/inputs/live-1/"+sub || auth != "Bearer op-token" {
			t.Fatalf("%s sent %s %s %q", sub, method, path, auth)
		}
	}
	if _, errs, code := cli(t, ts.URL, "input", "show"); code != 2 || !strings.Contains(errs, "usage") {
		t.Fatalf("no key: %d %q", code, errs)
	}
	if _, errs, code := cli(t, ts.URL, "input", "bogus", "live-1"); code != 2 || !strings.Contains(errs, "usage") {
		t.Fatalf("an unknown subcommand: %d %q", code, errs)
	}
}
