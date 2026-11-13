// flotilla-host — the host agent (ADR-0006, ADR-0009): runs on a host as its own unprivileged user,
// dials flotillad, and runs only what this host's own config allows.
//
//	flotilla-host enrol   [flags] < token   once, at install: makes the key here, gets the certificate
//	flotilla-host run     [flags]           the service
//	flotilla-host sudoers -config FILE       the sudoers lines for this host's privileged actions
//
// Flags: -config (the host's config, root-owned), -state (the key, the certificate and the executions
// store), -audit (the audit log, in a root-owned directory), -addr (flotillad's host endpoint), -server-name
// (its name in flotillad's certificate), -root (the root CA's certificate, pinned). The enrolment token
// comes on stdin only: never a flag, never the environment.
package main

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/chipi/flotilla/internal/hostagent"
	"github.com/chipi/flotilla/internal/hostaudit"
	"github.com/chipi/flotilla/internal/hostca"
	"github.com/chipi/flotilla/internal/hostcfg"
	"github.com/chipi/flotilla/internal/hostexec"
)

// set at build time: -ldflags "-X main.version=sha-<7>"
var version = "dev"

type opts struct {
	config, state, audit, addr, serverName, root string
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: flotilla-host enrol|run|sudoers [flags]")
		os.Exit(2)
	}
	cmd := os.Args[1]
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	var o opts
	fs.StringVar(&o.config, "config", "/etc/flotilla-host/config.json", "this host's config")
	fs.StringVar(&o.state, "state", "/var/db/flotilla-host", "the key, the certificate and the executions store")
	fs.StringVar(&o.audit, "audit", "/var/log/flotilla-host/audit.jsonl", "the audit log")
	fs.StringVar(&o.addr, "addr", "", "flotillad's host endpoint, host:port")
	fs.StringVar(&o.serverName, "server-name", "", "flotillad's name in its certificate")
	fs.StringVar(&o.root, "root", "/etc/flotilla-host/root.crt", "the root CA's certificate")
	fs.Parse(os.Args[2:])
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil)).With("service", "flotilla-host", "version", version)
	var err error
	switch cmd {
	case "enrol":
		err = enrol(o, os.Stdin)
	case "run":
		err = run(o, log)
	case "sudoers":
		var cfg *hostcfg.Config
		if cfg, err = hostcfg.Load(o.config); err == nil {
			var s string
			if s, err = cfg.Sudoers(); err == nil {
				fmt.Print(s)
			}
		}
	default:
		err = fmt.Errorf("unknown command %q", cmd)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "flotilla-host:", err)
		os.Exit(1)
	}
}

func (o opts) need() error {
	if o.addr == "" || o.serverName == "" {
		return errors.New("-addr and -server-name are required")
	}
	return nil
}

func keyPath(state string) string   { return filepath.Join(state, "key.pem") }
func chainPath(state string) string { return filepath.Join(state, "cert.pem") }

// loadKey reads this host's key, or with create makes one (mode 600, never overwritten).
func loadKey(state string, create bool) (ed25519.PrivateKey, error) {
	b, err := os.ReadFile(keyPath(state))
	if err == nil {
		return hostca.ParseKey(b)
	}
	if !errors.Is(err, os.ErrNotExist) || !create {
		return nil, err
	}
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	pemKey, err := hostca.KeyPEM(key)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(keyPath(state), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	if _, err := f.Write(pemKey); err != nil {
		f.Close()
		return nil, err
	}
	return key, f.Close()
}

// saveChain replaces the certificate file whole (a renewal must never leave half a file).
func saveChain(state string, b []byte) error {
	tmp := chainPath(state) + ".new"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, chainPath(state))
}

func enrol(o opts, stdin *os.File) error {
	if err := o.need(); err != nil {
		return err
	}
	cfg, err := hostcfg.Load(o.config)
	if err != nil {
		return err
	}
	root, err := readRoot(o.root)
	if err != nil {
		return err
	}
	tok, _ := bufio.NewReader(stdin).ReadString('\n')
	tok = strings.TrimSpace(tok)
	if tok == "" {
		return errors.New("the enrolment token comes on stdin")
	}
	if err := os.MkdirAll(o.state, 0o700); err != nil {
		return err
	}
	key, err := loadKey(o.state, true)
	if err != nil {
		return err
	}
	chain, err := hostagent.Enrol(context.Background(), o.addr, o.serverName, root, cfg.Host, tok, key)
	if err != nil {
		return err
	}
	if err := saveChain(o.state, chain); err != nil {
		return err
	}
	fmt.Printf("enrolled %s with %s\n", cfg.Host, o.addr)
	return nil
}

func readRoot(path string) (*x509.Certificate, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return hostca.ParseCert(b)
}

func run(o opts, log *slog.Logger) error {
	if err := o.need(); err != nil {
		return err
	}
	cfg, err := hostcfg.Load(o.config)
	if err != nil {
		return err
	}
	root, err := readRoot(o.root)
	if err != nil {
		return err
	}
	key, err := loadKey(o.state, false)
	if err != nil {
		return fmt.Errorf("this host's key (enrol first): %w", err)
	}
	db, n, err := hostexec.Open(filepath.Join(o.state, "executions.db"))
	if err != nil {
		return err
	}
	defer db.Close()
	if n > 0 {
		log.Warn("executions left started by the last run are unknown now", "count", n)
	}
	audit, err := hostaudit.Open(o.audit)
	if err != nil {
		return err
	}
	defer audit.Close()
	x := hostexec.New(db, audit, cfg, hostexec.ExecRunner{}, hostexec.LocalChecker{Run: hostexec.ExecRunner{}})
	a := &hostagent.Agent{Addr: o.addr, ServerName: o.serverName, Root: root, Host: cfg.Host, Version: version, Key: key,
		LoadChain: func() ([]byte, error) { return os.ReadFile(chainPath(o.state)) },
		SaveChain: func(b []byte) error { return saveChain(o.state, b) },
		Exec:      x, Log: log}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	log.Info("flotilla-host starting", "host", cfg.Host, "addr", o.addr, "ceiling", cfg.Ceiling, "actions", len(cfg.Actions))
	if err := a.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	log.Info("flotilla-host stopped")
	return nil
}
