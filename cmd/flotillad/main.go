// flotillad — the flotilla control plane: specs, scheduler, store, API.
//
//	flotillad -config /etc/flotilla/flotillad.yaml
//	flotillad -config ... -check      # load config, callers and specs, then exit (a deploy gate)
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/chipi/flotilla/internal/api"
	"github.com/chipi/flotilla/internal/casetimer"
	"github.com/chipi/flotilla/internal/dispatch"
	"github.com/chipi/flotilla/internal/models"
	"github.com/chipi/flotilla/internal/registry"
	"github.com/chipi/flotilla/internal/sched"
	"github.com/chipi/flotilla/internal/skills"
	"github.com/chipi/flotilla/internal/spec"
	"github.com/chipi/flotilla/internal/store"
	"github.com/chipi/flotilla/internal/telemetry"
	"github.com/chipi/flotilla/internal/ui"
	"github.com/chipi/flotilla/internal/worker"
)

// set at build time: -ldflags "-X main.version=sha-<7>"
var version = "dev"

// Config is one installation: where things are. Nothing here is in the public repo's specs.
type Config struct {
	Listen string `yaml:"listen"`
	// Optional: where workers' routines reach their ledger (RFC-0002 §9 step 5). It serves the ledger
	// routes and nothing else, so `listen` can stay on loopback.
	WorkerListen string `yaml:"worker_listen"`
	// Optional: the operator UI's listener and the one Host it answers (ADR-0014): the pages for people
	// with a session, and /v1 for people's tokens only. Only the UI's proxy reaches it.
	UIListen      string `yaml:"ui_listen"`
	UIHost        string `yaml:"ui_host"`
	SpecsDir      string `yaml:"specs_dir"`
	StateDir      string `yaml:"state_dir"`
	Environment   string `yaml:"environment"`
	Timezone      string `yaml:"timezone"`
	VMURL         string `yaml:"vm_url"`
	OTLPTracesURL string `yaml:"otlp_traces_url"`
	// a file holding the GlitchTip DSN (or SENTRY_DSN=...); empty = errors stay in the logs only
	ErrorsDSNFile string                  `yaml:"errors_dsn_file"`
	CallersFile   string                  `yaml:"callers_file"`
	Workers       map[string]WorkerConfig `yaml:"workers"`
	// routines this installation does not run, whatever their spec says (e.g. an e2e stack that has no
	// real services for them). A listed id that no spec has is an error, so a typo cannot hide.
	DisabledRoutines []string `yaml:"disabled_routines"`
	// the model aliases routines may name and their OpenAI-compatible endpoints (ADR-0004); every run gets
	// it as JSON in FLOTILLA_MODELS. Optional: without it, a routine that needs a model fails its run.
	Models *models.Config `yaml:"models"`
	// Optional: how long a case may stay detected (then an alert) or handed over (then escalated);
	// RFC-0004 §3.5. Unset: 30m and 24h.
	CaseTimers casetimer.Config `yaml:"case_timers"`
	// Optional: the registry's declarations (ADR-0011, RFC-0006 §10 step 4). Unset: the host agent's,
	// which the image carries. An installation's extension adds its own file here.
	RegistryFiles []string `yaml:"registry_files"`
}

// defaultRegistry is the declaration flotillad's image carries for its own tag.
var defaultRegistry = []string{"/opt/flotilla/host/registry.json"}

type WorkerConfig struct {
	URL       string `yaml:"url"`
	TokenFile string `yaml:"token_file"`
}

func loadConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	var c Config
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	var errs []error
	if c.Listen == "" {
		errs = append(errs, errors.New("listen is required"))
	}
	if c.SpecsDir == "" || c.StateDir == "" || c.CallersFile == "" {
		errs = append(errs, errors.New("specs_dir, state_dir and callers_file are required"))
	}
	if c.Environment == "" {
		errs = append(errs, errors.New("environment is required (the homelab label: operations)"))
	}
	if c.UIListen != "" && c.UIHost == "" {
		errs = append(errs, errors.New("ui_listen needs ui_host: the one Host the UI answers (flotilla.<tailnet>)"))
	}
	for name, w := range c.Workers {
		if w.URL == "" || w.TokenFile == "" {
			errs = append(errs, fmt.Errorf("worker %s: url and token_file are required", name))
		}
	}
	if c.CaseTimers.DetectedAlert < 0 || c.CaseTimers.HandedOverEscalate < 0 {
		errs = append(errs, errors.New("case_timers: a limit cannot be negative (leave it out for the default)"))
	}
	if c.Models != nil {
		if err := c.Models.Validate(); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("%s: %w", path, errors.Join(errs...))
	}
	return &c, nil
}

func readToken(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	t := strings.TrimSpace(string(b))
	if len(t) < 32 {
		return "", fmt.Errorf("%s: token shorter than 32 characters", path)
	}
	return t, nil
}

func main() {
	cfgPath := flag.String("config", "/etc/flotilla/flotillad.yaml", "config file")
	check := flag.Bool("check", false, "load config, callers and specs, print them, exit")
	testError := flag.Bool("send-test-error", false, "send one labelled ERROR to GlitchTip through the configured path, then exit")
	flag.Parse()

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		os.Exit(2)
	}
	log := telemetry.Logger(cfg.Environment, version)
	routines, err := spec.Load(cfg.SpecsDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "specs:", err)
		os.Exit(2)
	}
	if err := skills.Check(cfg.SpecsDir, routines); err != nil {
		fmt.Fprintln(os.Stderr, "skills:", err)
		os.Exit(2)
	}
	callers, err := api.LoadCallers(cfg.CallersFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "callers:", err)
		os.Exit(2)
	}
	regFiles := cfg.RegistryFiles
	if len(regFiles) == 0 {
		regFiles = defaultRegistry
	}
	reg, err := registry.Load(regFiles...)
	if err != nil {
		fmt.Fprintln(os.Stderr, "registry:", err)
		os.Exit(2)
	}
	loc := time.Local
	if cfg.Timezone != "" {
		if loc, err = time.LoadLocation(cfg.Timezone); err != nil {
			fmt.Fprintln(os.Stderr, "timezone:", err)
			os.Exit(2)
		}
	}
	clients := map[string]*worker.Client{}
	runners := map[string]sched.Runner{}
	workerTokens := map[string]string{}
	for name, w := range cfg.Workers {
		tok, err := readToken(w.TokenFile)
		if err != nil {
			fmt.Fprintln(os.Stderr, "worker", name+":", err)
			os.Exit(2)
		}
		clients[name] = worker.New(w.URL, tok)
		runners[name] = clients[name]
		workerTokens[name] = tok
	}
	callers = append(callers, api.WorkerCallers(workerTokens)...) // the ledger's callers (RFC-0002 §9 step 5)
	for _, id := range cfg.DisabledRoutines {
		found := false
		for _, j := range routines {
			if j.ID() == id {
				j.Enabled, found = false, true
			}
		}
		if !found {
			fmt.Fprintf(os.Stderr, "config: disabled_routines names %s, which no spec defines\n", id)
			os.Exit(2)
		}
	}
	for _, j := range routines {
		if _, ok := runners[j.Worker]; j.Enabled && !ok {
			fmt.Fprintf(os.Stderr, "specs: %s needs worker %q, which the config does not have\n", j.ID(), j.Worker)
			os.Exit(2)
		}
	}
	if *check {
		for _, j := range routines {
			fmt.Printf("%-24s enabled=%-5v worker=%-8s every=%-6s autonomy=%-7s spec=%s\n", j.ID(), j.Enabled, j.Worker, j.Every, j.Autonomy, j.Hash)
		}
		n := 0
		if cfg.Models != nil {
			n = len(cfg.Models.Aliases)
		}
		for _, a := range reg.Actions() {
			fmt.Printf("action %s\n", a)
		}
		fmt.Printf("CHECK PASS: %d routines, %d callers, %d workers, %d model aliases, %d actions\n", len(routines), len(callers), len(clients), n, len(reg.Actions()))
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	flushErrors, err := telemetry.Errors(cfg.ErrorsDSNFile, cfg.Environment, version)
	if err != nil {
		log.Error("errors to GlitchTip", "err", err)
		os.Exit(1)
	}
	defer flushErrors()
	if *testError {
		if cfg.ErrorsDSNFile == "" {
			fmt.Fprintln(os.Stderr, "errors_dsn_file is not set: nothing to send to")
			os.Exit(2)
		}
		log.Error("flotillad test error (send-test-error)", "probe", time.Now().UTC().Format(time.RFC3339))
		flushErrors()
		fmt.Println("TEST ERROR SENT")
		return
	}
	shutdown, err := telemetry.Tracing(ctx, cfg.OTLPTracesURL, cfg.Environment, version)
	if err != nil {
		log.Error("tracing", "err", err)
		os.Exit(1)
	}
	defer func() {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = shutdown(c)
	}()

	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		log.Error("state dir", "err", err)
		os.Exit(1)
	}
	st, err := store.Open(filepath.Join(cfg.StateDir, "flotilla.db"))
	if err != nil {
		log.Error("store", "err", err)
		os.Exit(1)
	}
	defer st.Close()
	if n, err := st.MarkInterrupted(ctx); err != nil {
		log.Error("store", "err", err)
		os.Exit(1)
	} else if n > 0 {
		log.Warn("runs left running by the previous flotillad marked interrupted", "count", n)
	}
	_ = st.Event(ctx, "flotillad", "flotillad", "start", version, fmt.Sprintf("%d routines", len(routines)))

	push := &telemetry.Pusher{URL: cfg.VMURL, Env: cfg.Environment, Version: version, Log: log}
	modelsJSON := ""
	if cfg.Models != nil {
		if modelsJSON, err = cfg.Models.JSON(); err != nil {
			log.Error("models: cannot encode", "err", err)
			os.Exit(2)
		}
	}
	s := &sched.Scheduler{Store: st, Routines: routines, Workers: runners, Metrics: push, Location: loc, Log: log, ModelsJSON: modelsJSON,
		OTLPTracesURL: cfg.OTLPTracesURL, Environment: cfg.Environment}
	apiSrv := &api.Server{Sched: s, Store: st, Workers: clients, Callers: callers, Version: version, Log: log, Registry: reg}
	// Cases: monitors' detections open them, a handed-over case starts its kind's acting routine (pipeline
	// v1 steps 5 and 6). Set before anything can start a run.
	disp := &dispatch.Dispatcher{API: apiSrv, Log: log}
	s.AfterRun = disp.Detected
	srv := &http.Server{Addr: cfg.Listen, ReadHeaderTimeout: 10 * time.Second, Handler: apiSrv.Handler()}
	go func() {
		log.Info("flotillad listening", "addr", cfg.Listen, "routines", len(routines))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("listen", "err", err)
			stop()
		}
	}()
	var wsrv *http.Server
	if cfg.WorkerListen != "" {
		wsrv = &http.Server{Addr: cfg.WorkerListen, ReadHeaderTimeout: 10 * time.Second, Handler: apiSrv.WorkerHandler()}
		go func() {
			log.Info("flotillad listening for workers (the ledger)", "addr", cfg.WorkerListen)
			if err := wsrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Error("worker listen", "err", err)
				stop()
			}
		}()
	}
	var usrv *http.Server
	if cfg.UIListen != "" {
		u := &ui.Server{API: apiSrv, Host: cfg.UIHost, Log: log}
		usrv = &http.Server{Addr: cfg.UIListen, Handler: u.Handler(), ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 2 * time.Minute, MaxHeaderBytes: 64 << 10}
		go func() {
			log.Info("flotillad listening for the UI", "addr", cfg.UIListen, "host", cfg.UIHost)
			if err := usrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Error("ui listen", "err", err)
				stop()
			}
		}()
	}
	hbDone := make(chan struct{})
	go func() { defer close(hbDone); heartbeat(ctx, push, s, st, cfg.StateDir, cfg.CaseTimers, log) }()
	dispDone := make(chan struct{})
	go func() { defer close(dispDone); disp.Loop(ctx, 15*time.Second) }()

	s.Loop(ctx)
	// The heartbeat and the dispatcher's runs must finish before the deferred st.Close: a backup cut off
	// by the close logged "sql: database is closed" (2026-11-11, a flotillad whose loop ended at once).
	stop()
	<-hbDone
	<-dispDone
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(sctx)
	if wsrv != nil {
		_ = wsrv.Shutdown(sctx)
	}
	if usrv != nil {
		_ = usrv.Shutdown(sctx)
	}
	_ = st.Event(context.Background(), "flotillad", "flotillad", "stop", version, "")
	log.Info("flotillad exit")
}

// heartbeat pushes flotilla_up and each routine's freshness and last success every minute, checks open
// cases against their stuck-state limits, and takes one backup of the store per day
// (state_dir/backup/flotilla-<day>.db, the last 7 kept).
func heartbeat(ctx context.Context, push *telemetry.Pusher, s *sched.Scheduler, st *store.Store, stateDir string, timers casetimer.Config, log *slog.Logger) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		push.Up()
		for _, j := range s.Routines {
			if !j.Enabled {
				continue
			}
			if j.Fresh > 0 { // a routine on cases has no freshness
				push.Freshness(j.ID(), j.Fresh)
			}
			if at, err := st.LastSuccess(ctx, j.ID()); err == nil {
				push.LastSuccess(j.ID(), at)
			}
		}
		if err := casetimer.Run(ctx, st, s.Clock(), timers, log); err != nil {
			log.Error("case timers", "err", err)
		}
		backup(ctx, st, stateDir, s.Clock().In(s.Location).Format("2006-01-02"), log)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func backup(ctx context.Context, st *store.Store, stateDir, day string, log *slog.Logger) {
	dir := filepath.Join(stateDir, "backup")
	path := filepath.Join(dir, "flotilla-"+day+".db")
	if _, err := os.Stat(path); err == nil {
		return
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		log.Error("backup", "err", err)
		return
	}
	// Written under a temporary name and renamed when complete: a backup cut off part way must not
	// leave a file under the day's name, which would count as today's backup.
	tmp := path + ".partial"
	_ = os.Remove(tmp)
	if err := st.Backup(ctx, tmp); err != nil {
		_ = os.Remove(tmp)
		log.Error("backup", "err", err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		log.Error("backup", "err", err)
		return
	}
	log.Info("store backed up", "path", path)
	if n, err := st.PruneSessions(ctx); err != nil { // once a day, with the backup (ADR-0014)
		log.Error("prune sessions", "err", err)
	} else if n > 0 {
		log.Info("sessions pruned", "n", n)
	}
	old, _ := filepath.Glob(filepath.Join(dir, "flotilla-*.db"))
	for len(old) > 7 {
		_ = os.Remove(old[0])
		old = old[1:]
	}
}
