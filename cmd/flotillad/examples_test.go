package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chipi/flotilla/internal/sched"
	"github.com/chipi/flotilla/internal/spec"
	"github.com/chipi/flotilla/internal/store"
	"github.com/chipi/flotilla/internal/telemetry"
	"github.com/chipi/flotilla/internal/versions"
)

func TestExamplesDirConfig(t *testing.T) {
	if c, err := load(t, ""); err != nil || c.ExamplesDir != "" {
		t.Fatalf("unset: %+v %v", c, err)
	}
	if c, err := load(t, "examples_dir: /opt/flotilla/examples\n"); err != nil || c.ExamplesDir != "/opt/flotilla/examples" {
		t.Fatalf("set: %+v %v", c, err)
	}
	if _, err := load(t, "examples_dir: /s/\n"); err == nil || !strings.Contains(err.Error(), "examples_dir is the specs_dir") {
		t.Fatalf("the catalogue is not the fleets it sits beside: %v", err)
	}
	if _, err := load(t, "example_dir: /x\n"); err == nil {
		t.Fatal("a misspelt key is refused (KnownFields): examples_dir is a deliberate change")
	}
}

// An installation with its own triage/cycle and an examples catalogue that holds the image's: both load, the routines are the installation's
// alone, the catalogue holds example-triage/cycle, the example needs no worker or alias of the installation's, and what the start writes to
// the store (every routine's first version) has no row for an example.
func TestACatalogueBesideAnInstallationsOwnFleets(t *testing.T) {
	own := t.TempDir()
	writeTree(t, own, map[string]string{"triage/cycle.yaml": "fleet: triage\nroutine: cycle\ndescription: mine\nenabled: true\nworker: only-worker\n" +
		"trigger: {every: 10m}\nrun: {command: \"true\", workdir: ., timeout: 1m}\nautonomy: record\nbudget: {day_usd: 1}\nfreshness: 35m\n"})
	cfg := &Config{SpecsDir: own, ExamplesDir: filepath.Join("..", "..", "fleets")}
	routines, err := spec.LoadVersions(cfg.SpecsDir)
	if err != nil {
		t.Fatal(err)
	}
	cat, err := loadExamples(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(routines) != 1 || routines[0].ID() != "triage/cycle" || routines[0].Worker != "only-worker" || !cat.Has("example-triage/cycle") {
		t.Fatalf("%d routines, catalogue %v", len(routines), cat.Has("example-triage/cycle"))
	}
	for _, e := range cat.Examples() {
		if e.Worker == "only-worker" {
			t.Fatalf("the example %s is the installation's", e.ID)
		}
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, _, err := versions.Reconcile(context.Background(), st, routines); err != nil {
		t.Fatal(err)
	}
	rows, err := st.AllVersions(context.Background())
	if err != nil || len(rows) != 1 || rows[0].Routine != "triage/cycle" {
		t.Fatalf("the store after a start: %+v %v", rows, err)
	}
	if line := examplesLine(cfg, cat); !strings.Contains(line, "never run") || !strings.Contains(line, "fleets from") {
		t.Errorf("-check's line: %s", line)
	}
	if line := examplesLine(&Config{}, nil); !strings.Contains(line, "examples_dir is not set") {
		t.Errorf("-check's line without a catalogue: %s", line)
	}
	if none, err := loadExamples(&Config{}); none != nil || err != nil {
		t.Errorf("unset: %v %v", none, err)
	}
	if _, err := loadExamples(&Config{ExamplesDir: t.TempDir()}); err == nil {
		t.Error("an empty examples folder is not a catalogue")
	}
}

// The image carries the examples twice from the same source, and the second copy is the one an installation's mounted fleets do not replace:
// two COPY lines of one folder, so the catalogue cannot drift from the examples the image runs by default.
func TestTheImageCarriesTheExamplesOutOfTheFleetsMountsReach(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "deploy", "docker", "flotillad.Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	var fleets, examples bool
	for _, l := range strings.Split(string(raw), "\n") {
		f := strings.Fields(l)
		if len(f) == 3 && f[0] == "COPY" && f[1] == "fleets" {
			fleets = fleets || f[2] == "/opt/flotilla/fleets"
			examples = examples || f[2] == "/opt/flotilla/examples"
		}
	}
	if !fleets || !examples {
		t.Errorf("flotillad.Dockerfile must COPY fleets to /opt/flotilla/fleets (%v) and to /opt/flotilla/examples (%v)", fleets, examples)
	}
	for _, f := range []string{"worker-triage", "worker-ops", "worker-bugfix", "worker-marketing"} {
		w, err := os.ReadFile(filepath.Join("..", "..", "deploy", "docker", f+".Dockerfile"))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(w), "/opt/flotilla/examples") {
			t.Errorf("%s carries the examples: an example never runs, so no worker image needs them", f)
		}
	}
}

// The heartbeat's metrics are the scheduler's routines' alone: with a catalogue loaded beside them, no sample names an example.
func TestTheFreshnessLoopPushesNoSeriesForAnExample(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
	}))
	defer ts.Close()
	st, err := store.Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	own := &spec.Routine{Fleet: "triage", Routine: "cycle", Enabled: true, Worker: "w", Autonomy: "record", Every: time.Minute, Limit: time.Minute, Fresh: time.Hour}
	s := &sched.Scheduler{Store: st, Workers: map[string]sched.Runner{}}
	s.SetRoutines([]*spec.Routine{own})
	cat, err := spec.LoadExamples(filepath.Join("..", "..", "fleets"))
	if err != nil || len(cat.Examples()) == 0 {
		t.Fatal(err)
	}
	push := &telemetry.Pusher{URL: ts.URL, Env: "test", Version: "t", Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	pushRoutineMetrics(context.Background(), push, s, st)
	mu.Lock()
	all := strings.Join(bodies, "\n")
	mu.Unlock()
	if !strings.Contains(all, `routine="triage/cycle"`) || strings.Contains(all, "example") {
		t.Fatalf("series: %s", all)
	}
}

// A real flotillad, started with examples_dir set: it serves the catalogue in the status, runs none of it, and its store holds rows for the
// installation's routine alone.
func TestARealStartWithExamplesDir(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and starts the binary")
	}
	d := t.TempDir()
	bin := filepath.Join(d, "flotillad")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	writeTree(t, d, map[string]string{"specs/triage/mine.yaml": "fleet: triage\nroutine: mine\ndescription: mine\nenabled: true\nworker: only\n" +
		"trigger: {every: 8760h}\nrun: {command: \"true\", workdir: ., timeout: 1m}\nautonomy: record\nbudget: {day_usd: 1}\nfreshness: 9000h\n"})
	tok := strings.Repeat("o", 40)
	sum := sha256.Sum256([]byte(tok))
	writeTree(t, d, map[string]string{"callers.yaml": "callers:\n  - {name: operator, role: operator, token_sha256: " + hex.EncodeToString(sum[:]) + "}\n", "w.token": strings.Repeat("w", 40)})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	cfgPath := filepath.Join(d, "flotillad.yaml")
	cfg := "listen: " + addr + "\nspecs_dir: " + d + "/specs\nexamples_dir: " + filepath.Join("..", "..", "fleets") + "\nstate_dir: " + d + "/state\nenvironment: test\n" +
		"callers_file: " + d + "/callers.yaml\nregistry_files: [" + filepath.Join("..", "..", "host", "registry.json") + "]\nworkers:\n  only: {url: \"http://127.0.0.1:1\", token_file: " + d + "/w.token}\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(d+"/state", 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "-config", cfgPath)
	var logs bytes.Buffer
	cmd.Stderr, cmd.Stdout = &logs, &logs
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	var status struct {
		Routines []struct{ ID string }
		Examples []struct{ ID string }
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		req, _ := http.NewRequest("GET", "http://"+addr+"/v1/status", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			err = json.NewDecoder(resp.Body).Decode(&status)
			resp.Body.Close()
			if err == nil && resp.StatusCode == 200 {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("flotillad did not serve its status:\n%s", logs.String())
		}
		time.Sleep(200 * time.Millisecond)
	}
	if len(status.Routines) != 1 || status.Routines[0].ID != "triage/mine" || len(status.Examples) < 30 {
		t.Fatalf("routines %v, %d examples", status.Routines, len(status.Examples))
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	read, err := store.ReadVersions(filepath.Join(d, "state", "flotilla.db"))
	if err != nil || !read.Exists {
		t.Fatalf("store: %+v %v", read, err)
	}
	for _, r := range read.Rows {
		if r.Routine != "triage/mine" {
			t.Errorf("a store row for %s: only the installation's routine has rows", r.Routine)
		}
	}
	if len(read.Rows) != 1 {
		t.Errorf("%d version rows", len(read.Rows))
	}
}

// main's own wiring: what loadSpecs returns as routines is what the scheduler and the metrics loop are given, and the examples catalogue is
// never among them, whatever examples_dir holds. A catalogue appended to the routines in loadSpecs turns this red (and so the metrics loop's
// series, which the scheduler's routines make).
func TestLoadSpecsHandsTheSchedulerNoExampleAndTheMetricsLoopNoSeriesForOne(t *testing.T) {
	own := t.TempDir()
	writeTree(t, own, map[string]string{"triage/cycle.yaml": "fleet: triage\nroutine: cycle\ndescription: mine\nenabled: true\nworker: w\n" +
		"trigger: {every: 10m}\nrun: {command: \"true\", workdir: ., timeout: 1m}\nautonomy: record\nbudget: {day_usd: 1}\nfreshness: 35m\n"})
	routines, _, cat, err := loadSpecs(&Config{SpecsDir: own, ExamplesDir: filepath.Join("..", "..", "fleets")})
	if err != nil || cat == nil || len(cat.Examples()) == 0 {
		t.Fatalf("%v %v", err, cat)
	}
	if len(routines) != 1 || routines[0].ID() != "triage/cycle" {
		t.Errorf("the routines main hands on: %d", len(routines))
	}
	var mu sync.Mutex
	var bodies []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
	}))
	defer ts.Close()
	st, err := store.Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := &sched.Scheduler{Store: st, Workers: map[string]sched.Runner{}}
	s.SetRoutines(routines)
	push := &telemetry.Pusher{URL: ts.URL, Env: "test", Version: "t", Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	pushRoutineMetrics(context.Background(), push, s, st)
	mu.Lock()
	all := strings.Join(bodies, "\n")
	mu.Unlock()
	if !strings.Contains(all, `routine="triage/cycle"`) || strings.Contains(all, "example") {
		t.Fatalf("series: %s", all)
	}
}
