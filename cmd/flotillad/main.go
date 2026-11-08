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
	"github.com/chipi/flotilla/internal/sched"
	"github.com/chipi/flotilla/internal/spec"
	"github.com/chipi/flotilla/internal/store"
	"github.com/chipi/flotilla/internal/telemetry"
	"github.com/chipi/flotilla/internal/worker"
)

// set at build time: -ldflags "-X main.version=sha-<7>"
var version = "dev"

// Config is one installation: where things are. Nothing here is in the public repo's specs.
type Config struct {
	Listen        string `yaml:"listen"`
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
	// jobs this installation does not run, whatever their spec says (e.g. an e2e stack that has no
	// real services for them). A listed id that no spec has is an error, so a typo cannot hide.
	DisabledJobs []string `yaml:"disabled_jobs"`
}

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
	for name, w := range c.Workers {
		if w.URL == "" || w.TokenFile == "" {
			errs = append(errs, fmt.Errorf("worker %s: url and token_file are required", name))
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
	flag.Parse()

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		os.Exit(2)
	}
	log := telemetry.Logger(cfg.Environment, version)
	jobs, err := spec.Load(cfg.SpecsDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "specs:", err)
		os.Exit(2)
	}
	callers, err := api.LoadCallers(cfg.CallersFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "callers:", err)
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
	for name, w := range cfg.Workers {
		tok, err := readToken(w.TokenFile)
		if err != nil {
			fmt.Fprintln(os.Stderr, "worker", name+":", err)
			os.Exit(2)
		}
		clients[name] = worker.New(w.URL, tok)
		runners[name] = clients[name]
	}
	for _, id := range cfg.DisabledJobs {
		found := false
		for _, j := range jobs {
			if j.ID() == id {
				j.Enabled, found = false, true
			}
		}
		if !found {
			fmt.Fprintf(os.Stderr, "config: disabled_jobs names %s, which no spec defines\n", id)
			os.Exit(2)
		}
	}
	for _, j := range jobs {
		if _, ok := runners[j.Worker]; j.Enabled && !ok {
			fmt.Fprintf(os.Stderr, "specs: %s needs worker %q, which the config does not have\n", j.ID(), j.Worker)
			os.Exit(2)
		}
	}
	if *check {
		for _, j := range jobs {
			fmt.Printf("%-24s enabled=%-5v worker=%-8s every=%-6s stage=%-7s spec=%s\n", j.ID(), j.Enabled, j.Worker, j.Every, j.Stage, j.Hash)
		}
		fmt.Printf("CHECK PASS: %d jobs, %d callers, %d workers\n", len(jobs), len(callers), len(clients))
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
	_ = st.Event(ctx, "flotillad", "flotillad", "start", version, fmt.Sprintf("%d jobs", len(jobs)))

	push := &telemetry.Pusher{URL: cfg.VMURL, Env: cfg.Environment, Version: version, Log: log}
	s := &sched.Scheduler{Store: st, Jobs: jobs, Workers: runners, Metrics: push, Location: loc, Log: log}
	srv := &http.Server{Addr: cfg.Listen, ReadHeaderTimeout: 10 * time.Second,
		Handler: (&api.Server{Sched: s, Store: st, Workers: clients, Callers: callers, Version: version, Log: log}).Handler()}
	go func() {
		log.Info("flotillad listening", "addr", cfg.Listen, "jobs", len(jobs))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("listen", "err", err)
			stop()
		}
	}()
	go heartbeat(ctx, push, s, st, cfg.StateDir, log)

	s.Loop(ctx)
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(sctx)
	_ = st.Event(context.Background(), "flotillad", "flotillad", "stop", version, "")
	log.Info("flotillad exit")
}

// heartbeat pushes flotilla_up and each job's freshness and last success every minute, and takes
// one backup of the store per day (state_dir/backup/flotilla-<day>.db, the last 7 kept).
func heartbeat(ctx context.Context, push *telemetry.Pusher, s *sched.Scheduler, st *store.Store, stateDir string, log *slog.Logger) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		push.Up()
		for _, j := range s.Jobs {
			if !j.Enabled {
				continue
			}
			push.Freshness(j.ID(), j.Fresh)
			if at, err := st.LastSuccess(ctx, j.ID()); err == nil {
				push.LastSuccess(j.ID(), at)
			}
		}
		backup(ctx, st, stateDir, s.Now().In(s.Location).Format("2006-01-02"), log)
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
	if err := st.Backup(ctx, path); err != nil {
		log.Error("backup", "err", err)
		return
	}
	log.Info("store backed up", "path", path)
	old, _ := filepath.Glob(filepath.Join(dir, "flotilla-*.db"))
	for len(old) > 7 {
		_ = os.Remove(old[0])
		old = old[1:]
	}
}
