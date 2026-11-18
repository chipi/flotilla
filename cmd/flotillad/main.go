// flotillad — the flotilla control plane: specs, scheduler, store, API.
//
//	flotillad -config /etc/flotilla/flotillad.yaml
//	flotillad -config ... -check      # load config, callers and specs, read the store's schema, then exit (a deploy gate)
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/chipi/flotilla/internal/api"
	"github.com/chipi/flotilla/internal/casetimer"
	"github.com/chipi/flotilla/internal/dispatch"
	"github.com/chipi/flotilla/internal/hostalert"
	"github.com/chipi/flotilla/internal/hostca"
	"github.com/chipi/flotilla/internal/hostlink"
	"github.com/chipi/flotilla/internal/mcpserver"
	"github.com/chipi/flotilla/internal/models"
	"github.com/chipi/flotilla/internal/prompts"
	"github.com/chipi/flotilla/internal/registry"
	"github.com/chipi/flotilla/internal/sched"
	"github.com/chipi/flotilla/internal/skills"
	"github.com/chipi/flotilla/internal/spec"
	"github.com/chipi/flotilla/internal/store"
	"github.com/chipi/flotilla/internal/telemetry"
	"github.com/chipi/flotilla/internal/ui"
	"github.com/chipi/flotilla/internal/versions"
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
	// VictoriaLogs, which the UI reads for a run's and a fleet's log lines; empty = the UI shows none
	VLogsURL string `yaml:"vlogs_url"`
	// each host's metrics instance in VictoriaMetrics (node exporter), for its load on the UI: {host-01: example}
	HostMetrics map[string]string `yaml:"host_metrics"`
	// when the UI's morning brief is written each day, HH:MM in timezone (default 07:00)
	BriefAt string `yaml:"brief_at"`
	// approvals in Slack through one host's connector (G9, internal/api/slack.go); empty = none
	Slack *api.SlackConfig `yaml:"slack"`
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
	// Optional: the installation's values for an action's enum parameter, replacing the registry's
	// examples ({github.open_pr: {repo: [owner/repo, ...]}}). Unset: the registry's own.
	ActionEnums map[string]map[string][]string `yaml:"action_enums"`
	// Optional: the host agents' endpoint (ADR-0006). Unset: no host can enrol or connect.
	Hosts HostsConfig `yaml:"hosts"`
}

// defaultRegistry is the declaration flotillad's image carries for its own tag.
var defaultRegistry = []string{"/opt/flotilla/host/registry.json"}

// applyActionEnums puts the installation's enum values (action_enums) into the loaded registry, in
// sorted order so an error names the same entry every time.
func applyActionEnums(reg *registry.Registry, enums map[string]map[string][]string) error {
	actions := make([]string, 0, len(enums))
	for a := range enums {
		actions = append(actions, a)
	}
	sort.Strings(actions)
	for _, a := range actions {
		params := make([]string, 0, len(enums[a]))
		for p := range enums[a] {
			params = append(params, p)
		}
		sort.Strings(params)
		for _, p := range params {
			if err := reg.SetEnum(a, p, enums[a][p]); err != nil {
				return err
			}
		}
	}
	return nil
}

type WorkerConfig struct {
	URL       string `yaml:"url"`
	TokenFile string `yaml:"token_file"`
	// Remote is a remote worker's identity (RFC-0011): the certificate name its link connects under,
	// w-<worker>-<node>. A remote worker has no url or token: it dials the host endpoint.
	Remote string `yaml:"remote"`
}

var remoteIdentityRe = hostca.WorkerIdentityRe

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
		errs = append(errs, errors.New("environment is required (the installation label: operations)"))
	}
	if c.UIListen != "" && c.UIHost == "" {
		errs = append(errs, errors.New("ui_listen needs ui_host: the one Host the UI answers (flotilla.<tailnet>)"))
	}
	if sl := c.Slack; sl != nil {
		if c.Hosts.Listen == "" {
			errs = append(errs, errors.New("slack needs hosts: its connector is a host agent"))
		}
		if sl.ConnectorHost == "" || sl.TeamID == "" || len(sl.Users) == 0 || sl.UIURL == "" {
			errs = append(errs, errors.New("slack needs connector_host, team_id, ui_url and at least one pinned user"))
		}
		for id, name := range sl.Users {
			if !slackUserRe.MatchString(id) || name == "" {
				errs = append(errs, fmt.Errorf("slack users: %q -> %q: a Slack user id (U…) and a flotilla user name", id, name))
			}
		}
		if sl.DailyCap < 0 || sl.DailyCap > 100 {
			errs = append(errs, fmt.Errorf("slack daily_cap %d: 0 (the default, 10) to 100", sl.DailyCap))
		}
	}
	if c.BriefAt != "" {
		if _, err := time.Parse("15:04", c.BriefAt); err != nil {
			errs = append(errs, fmt.Errorf("brief_at %q: HH:MM, the time of day the brief is written", c.BriefAt))
		}
	}
	seenIdentity := map[string]string{}
	for name, w := range c.Workers {
		switch {
		case w.Remote != "" && (w.URL != "" || w.TokenFile != ""):
			errs = append(errs, fmt.Errorf("worker %s: remote, or url and token_file, not both", name))
		case w.Remote != "":
			if !remoteIdentityRe.MatchString(w.Remote) {
				errs = append(errs, fmt.Errorf("worker %s: remote %q: an identity w-<worker>-<node>", name, w.Remote))
			}
			if c.Hosts.Listen == "" {
				errs = append(errs, fmt.Errorf("worker %s: a remote worker needs hosts: its link dials the host endpoint", name))
			}
			if other, dup := seenIdentity[w.Remote]; dup {
				errs = append(errs, fmt.Errorf("workers %s and %s: one identity %s", other, name, w.Remote))
			}
			seenIdentity[w.Remote] = name
		case w.URL == "" || w.TokenFile == "":
			errs = append(errs, fmt.Errorf("worker %s: url and token_file are required (or remote)", name))
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
	if err := c.Hosts.validate(); err != nil {
		errs = append(errs, err)
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

// checkStore is -check's look at the store: read-only, so a rollback to a binary older than the
// store dies in the check, before anything is stopped (deploy/server/deploy.sh).
func checkStore(stateDir string) error {
	return store.CheckSchema(filepath.Join(stateDir, "flotilla.db"))
}

// checkVersions is -check's look at the versions: the loaded ones against the store's recorded ones, read-only.
// A store that does not exist yet (a first deploy) or is older than the version table has nothing to freeze
// against: the check says so and goes on, and the start records what -check found.
func checkVersions(stateDir string, routines []*spec.Routine) (versions.Plan, string, error) {
	read, err := store.ReadVersions(filepath.Join(stateDir, "flotilla.db"))
	if err != nil {
		return versions.Plan{}, "", fmt.Errorf("store: %w", err)
	}
	plan, err := versions.Check(routines, read.Rows)
	if err != nil {
		return plan, "", err
	}
	switch {
	case !read.Exists:
		return plan, "versions: freeze not checked: no store yet, a first deploy (every routine's first version goes live)", nil
	case read.Schema < store.VersionsSchema:
		return plan, fmt.Sprintf("versions: freeze not checked: the store is at schema %d, older than %d (the start migrates it and records each routine's first version)", read.Schema, store.VersionsSchema), nil
	}
	return plan, fmt.Sprintf("versions: freeze checked against %d recorded versions", len(read.Rows)), nil
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
	routines, err := spec.LoadVersions(cfg.SpecsDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "specs:", err)
		os.Exit(2)
	}
	fleets, err := spec.LoadFleets(cfg.SpecsDir, routines)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fleets:", err)
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
	if err := applyActionEnums(reg, cfg.ActionEnums); err != nil {
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
	var link *hostlink.Server // the host endpoint, made here so remote workers can be its runners
	remotes := map[string]string{}
	if cfg.Hosts.Listen != "" {
		link = &hostlink.Server{}
	}
	for name, w := range cfg.Workers {
		if w.Remote != "" {
			runners[name] = hostlink.RemoteRunner{S: link, Worker: name}
			remotes[w.Remote] = name
			continue
		}
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
		if len(j.Models) > 0 {
			if cfg.Models == nil {
				fmt.Fprintf(os.Stderr, "specs: %s names models %v, and the config has no models section\n", j.ID(), j.Models)
				os.Exit(2)
			}
			if _, err := cfg.Models.Subset(j.Models); err != nil {
				fmt.Fprintf(os.Stderr, "specs: %s: %v\n", j.ID(), err)
				os.Exit(2)
			}
		}
	}
	if *check {
		if err := checkStore(cfg.StateDir); err != nil {
			fmt.Fprintln(os.Stderr, "store:", err)
			os.Exit(1)
		}
		plan, freeze, err := checkVersions(cfg.StateDir, routines)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		for _, j := range versions.Bound(routines, plan) {
			fmt.Printf("%-24s enabled=%-5v worker=%-8s every=%-6s autonomy=%-7s spec=%s version=v%d versions=%s\n", j.ID(), j.Enabled, j.Worker, j.Every, j.Autonomy, j.Hash,
				j.Version, plan.States(j.ID()))
			for _, n := range j.Notes {
				fmt.Printf("note %s: %s\n", j.ID(), n)
			}
		}
		fmt.Println(freeze)
		for _, n := range plan.Notes {
			fmt.Println("note versions:", n)
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
	orphans, err := st.RunningRuns(ctx) // read before MarkInterrupted closes them
	if err != nil {
		log.Error("store", "err", err)
		os.Exit(1)
	}
	if n, err := st.MarkInterrupted(ctx); err != nil {
		log.Error("store", "err", err)
		os.Exit(1)
	} else if n > 0 {
		log.Warn("runs left running by the previous flotillad marked interrupted", "count", n)
	}
	// The versions: refuse a live or previous version whose folder changed, record the new ones, and run each routine
	// as its live version. Before anything can start a run.
	src := routines
	routines, plan, err := versions.Reconcile(ctx, st, routines)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		log.Error("versions", "err", err)
		os.Exit(2)
	}
	for _, n := range plan.Notes {
		log.Warn("versions: tolerated", "note", n)
	}
	textNotes, err := checkTexts(cfg.SpecsDir, src, plan)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		log.Error("versions", "err", err)
		os.Exit(2)
	}
	for _, n := range textNotes {
		log.Warn("versions: tolerated", "note", n)
	}
	for _, j := range routines {
		for _, n := range j.Notes {
			log.Warn("spec: deprecated", "routine", j.ID(), "note", n)
		}
	}
	if fleets, err = spec.LoadFleets(cfg.SpecsDir, routines); err != nil {
		fmt.Fprintln(os.Stderr, "fleets:", err)
		os.Exit(2)
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
	s := &sched.Scheduler{Store: st, Workers: runners, Metrics: push, Location: loc, Log: log, ModelsJSON: modelsJSON, Models: cfg.Models,
		OTLPTracesURL: cfg.OTLPTracesURL, Environment: cfg.Environment}
	s.SetRoutines(routines)
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
	var hostLn net.Listener
	var hostWatch *hostalert.Watcher
	if cfg.Hosts.Listen != "" {
		inter, root, err := loadHostCA(cfg.Hosts.CADir)
		if err != nil {
			log.Error("the host CA", "dir", cfg.Hosts.CADir, "err", err)
			os.Exit(1)
		}
		apiSrv.HostCA, apiSrv.HostRoot = inter, root
		if err := st.DisconnectHosts(ctx); err != nil { // no connection outlives the last flotillad
			log.Error("hosts: the store", "err", err)
			os.Exit(1)
		}
		hostWatch = &hostalert.Watcher{}
		sc := &serverCert{ca: inter, names: cfg.Hosts.ServerNames, now: time.Now}
		link.Auth, link.Log, link.TLS, link.Heartbeat = apiSrv, log, &tls.Config{GetCertificate: sc.get}, cfg.Hosts.Heartbeat
		link.OnContact, link.Workers, link.Version = recordContact(st, log), remotes, version
		for id, name := range remotes {
			log.Info("remote worker expected", "worker", name, "identity", id)
		}
		if cfg.Slack != nil {
			apiSrv.Slack = cfg.Slack
			link.OnApproval = apiSrv.RelayedApproval
			go slackNotices(ctx, apiSrv, link, cfg.Slack.ConnectorHost, log)
			log.Info("approvals in Slack", "connector", cfg.Slack.ConnectorHost, "users", len(cfg.Slack.Users), "daily_cap", cfg.Slack.Cap())
		}
		if cfg.Hosts.SigningKeyFile != "" {
			key, err := loadSigningKey(cfg.Hosts.SigningKeyFile)
			if err != nil {
				log.Error("the execution-signing key", "file", cfg.Hosts.SigningKeyFile, "err", err)
				os.Exit(1)
			}
			apiSrv.Exec = &api.Executions{Link: link, Key: key, KeyID: cfg.Hosts.SigningKeyID}
			if err := apiSrv.RecoverExecutions(ctx); err != nil {
				log.Error("host executions: recovery", "err", err)
				os.Exit(1)
			}
			log.Info("host executions are signed", "key_id", cfg.Hosts.SigningKeyID)
		}
		hostLn, err = net.Listen("tcp", cfg.Hosts.Listen)
		if err != nil {
			log.Error("host endpoint listen", "addr", cfg.Hosts.Listen, "err", err)
			os.Exit(1)
		}
		go func() {
			log.Info("flotillad listening for host agents", "addr", cfg.Hosts.Listen, "names", cfg.Hosts.ServerNames,
				"intermediate_until", inter.Cert.NotAfter)
			if err := link.Serve(hostLn); err != nil && !errors.Is(err, net.ErrClosed) {
				log.Error("host endpoint", "err", err)
				stop()
			}
		}()
		defer link.CloseAll()
	}
	var usrv *http.Server
	briefDone := make(chan struct{})
	if cfg.UIListen == "" {
		close(briefDone)
	}
	if cfg.UIListen != "" {
		u := &ui.Server{API: apiSrv, Host: cfg.UIHost, Log: log, LogsURL: cfg.VLogsURL, Fleets: fleets, SpecsDir: cfg.SpecsDir,
			MetricsURL: cfg.VMURL, HostMetrics: cfg.HostMetrics, BriefAt: cfg.BriefAt,
			MCP: (&mcpserver.Server{API: apiSrv, Version: version}).Handler()}
		go func() {
			defer close(briefDone)
			t := time.NewTicker(time.Minute)
			defer t.Stop()
			for {
				if err := u.WriteBriefIfDue(ctx); err != nil {
					log.Error("writing the brief", "err", err)
				}
				select {
				case <-ctx.Done():
					return
				case <-t.C:
				}
			}
		}()
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
	// The workers and the links are set up: stop what the previous flotillad left running and free its
	// cases, before anything can dispatch.
	if err := s.RecoverInterrupted(ctx, orphans); err != nil {
		log.Error("recovering the interrupted runs", "err", err)
		os.Exit(1)
	}
	hbDone := make(chan struct{})
	go func() { defer close(hbDone); heartbeat(ctx, push, s, st, cfg.StateDir, cfg.CaseTimers, hostWatch, log) }()
	dispDone := make(chan struct{})
	go func() { defer close(dispDone); disp.Loop(ctx, 15*time.Second) }()

	s.Loop(ctx)
	// The heartbeat and the dispatcher's runs must finish before the deferred st.Close: a backup cut off
	// by the close logged "sql: database is closed" (2026-11-11, a flotillad whose loop ended at once).
	stop()
	<-hbDone
	<-dispDone
	<-briefDone
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(sctx)
	if wsrv != nil {
		_ = wsrv.Shutdown(sctx)
	}
	if usrv != nil {
		_ = usrv.Shutdown(sctx)
	}
	if hostLn != nil {
		hostLn.Close()
	}
	_ = st.Event(context.Background(), "flotillad", "flotillad", "stop", version, "")
	log.Info("flotillad exit")
}

// heartbeat pushes flotilla_up and each routine's freshness and last success every minute, checks open
// cases against their stuck-state limits, and takes one backup of the store per day
// (state_dir/backup/flotilla-<day>.db, the last 7 kept).
func heartbeat(ctx context.Context, push *telemetry.Pusher, s *sched.Scheduler, st *store.Store, stateDir string, timers casetimer.Config,
	hostWatch *hostalert.Watcher, log *slog.Logger) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		if hostWatch != nil {
			watchHosts(ctx, st, hostWatch, s.Clock(), log)
		}
		push.Up()
		for _, j := range s.Routines() {
			if !j.Enabled {
				continue
			}
			// a routine on cases has no freshness; a STOPped or paused one is not stale, it is held: no freshness
			// pushed, so the stale alert (which needs it) stays quiet for it and a real stale routine stands out
			if held, err := s.Held(ctx, j); j.Fresh > 0 && err == nil && !held {
				push.Freshness(j.ID(), j.Fresh)
			}
			if at, err := st.LastSuccess(ctx, j.ID()); err == nil {
				push.LastSuccess(j.ID(), at)
			}
		}
		if err := casetimer.Run(ctx, st, s.Clock(), timers, log, s.CaseHold); err != nil {
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

var slackUserRe = regexp.MustCompile(`^[UW][A-Z0-9]{6,20}$`)

// slackNotices sends each plan waiting for a person to the Slack connector, every 15 seconds; a plan is
// marked sent only once its host took it.
func slackNotices(ctx context.Context, a *api.Server, link *hostlink.Server, host string, log *slog.Logger) {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		ns, err := a.SlackNotices(ctx)
		if err != nil {
			log.Error("slack notices", "err", err)
			continue
		}
		for _, n := range ns {
			if err := link.Notify(host, n); err != nil {
				break // the connector is not connected: the next tick tries again
			}
			if err := a.Store.MarkAskSent(ctx, n.Nonce); err != nil {
				log.Error("slack notices", "err", err)
			}
		}
	}
}

// checkTexts checks the skills and prompts of every version that can run (live, previous, shadow): a missing
// or refused one stops a start or a deploy. A retired version keeps history only, so what is wrong with its
// texts is a note.
func checkTexts(specsDir string, routines []*spec.Routine, plan versions.Plan) ([]string, error) {
	run := plan.Runnable(routines)
	var errs []error
	if err := skills.Check(specsDir, run); err != nil {
		errs = append(errs, fmt.Errorf("skills: %w", err))
	}
	if err := prompts.Check(specsDir, run); err != nil {
		errs = append(errs, fmt.Errorf("prompts: %w", err))
	}
	var notes []string
	for _, r := range plan.RetiredLoaded(routines) {
		if err := skills.Check(specsDir, []*spec.Routine{r}); err != nil {
			notes = append(notes, fmt.Sprintf("%s v%d is retired and its skills do not resolve: %v", r.ID(), r.Version, err))
		}
		if err := prompts.Check(specsDir, []*spec.Routine{r}); err != nil {
			notes = append(notes, fmt.Sprintf("%s v%d is retired and its prompts do not resolve: %v", r.ID(), r.Version, err))
		}
	}
	return notes, errors.Join(errs...)
}
