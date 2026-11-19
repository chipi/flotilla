package telemetry

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/getsentry/sentry-go"
)

// shadowHub is where a shadow run's errors go when the installation gave them a DSN of their own (errors_shadow_dsn_file); nil:
// they go to the main DSN, told apart by their tag and fingerprint.
var shadowHub *sentry.Hub

func readDSN(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	dsn := strings.TrimSpace(string(raw))
	if i := strings.Index(dsn, "="); i >= 0 && !strings.HasPrefix(dsn, "http") { // KEY=value file
		dsn = strings.Trim(strings.TrimSpace(dsn[i+1:]), `"'`)
	}
	return dsn, nil
}

// Errors sends flotillad's own ERROR log records to GlitchTip (Sentry protocol). A routine's failed run
// is the routine's business (the stale alert and the run record carry it), so only records flotillad
// logs at ERROR are sent: its store, its listener, its config. dsnFile empty = off.
//
// One issue per message: the fingerprint is the log message alone, never its values (the installation
// GlitchTip rule "one error = one issue").
//
// A shadow run's records (logged through ShadowLogger: shadow=true) never share an issue with a live one: each carries the tag
// shadow=1 and routine_version, and its fingerprint holds "shadow". With shadowDSNFile they go to that DSN (a project of their
// own, so the installation's readers of the live project never see them); without it, to the main DSN. The environment is the
// installation's either way.
func Errors(dsnFile, shadowDSNFile, env, release string) (func(), error) {
	shadowHub = nil
	sentry.CurrentHub().BindClient(nil)
	if dsnFile == "" && shadowDSNFile == "" {
		return func() {}, nil
	}
	opts := func(dsn string) sentry.ClientOptions {
		return sentry.ClientOptions{Dsn: dsn, Environment: env, Release: release, ServerName: Service, AttachStacktrace: true}
	}
	var flush []func()
	if dsnFile != "" {
		dsn, err := readDSN(dsnFile)
		if err != nil {
			return nil, err
		}
		if err := sentry.Init(opts(dsn)); err != nil {
			return nil, fmt.Errorf("sentry init: %w", err)
		}
		flush = append(flush, func() { sentry.Flush(5 * time.Second) })
	}
	if shadowDSNFile != "" {
		dsn, err := readDSN(shadowDSNFile)
		if err != nil {
			return nil, err
		}
		c, err := sentry.NewClient(opts(dsn))
		if err != nil {
			return nil, fmt.Errorf("sentry init (shadow): %w", err)
		}
		shadowHub = sentry.NewHub(c, sentry.NewScope())
		flush = append(flush, func() { c.Flush(5 * time.Second) })
	}
	return func() {
		for _, f := range flush {
			f()
		}
	}, nil
}

// ErrorHandler wraps a slog handler: every record at ERROR or above also goes to GlitchTip.
type ErrorHandler struct {
	slog.Handler
	attrs []slog.Attr // what With added: a shadow run's mark is there, not on the record
}

func isTrue(v slog.Value) bool {
	switch v.Kind() {
	case slog.KindBool:
		return v.Bool()
	case slog.KindString:
		return v.String() == "true" || v.String() == "1"
	}
	return false
}

func (h ErrorHandler) Handle(ctx context.Context, r slog.Record) error {
	if r.Level >= slog.LevelError {
		fields := sentry.Context{}
		shadow, version := false, ""
		note := func(a slog.Attr) {
			switch a.Key {
			case "shadow":
				shadow = isTrue(a.Value)
			case "routine_version":
				version = a.Value.String()
			}
		}
		for _, a := range h.attrs {
			note(a)
		}
		r.Attrs(func(a slog.Attr) bool { fields[a.Key] = a.Value.String(); note(a); return true })
		hub := sentry.CurrentHub()
		ev := sentry.NewEvent()
		ev.Level = sentry.LevelError
		ev.Message = r.Message
		ev.Fingerprint = []string{"flotillad", r.Message}
		if shadow {
			for _, a := range h.attrs {
				fields[a.Key] = a.Value.String()
			}
			ev.Fingerprint = []string{"flotillad", "shadow", r.Message}
			ev.Tags = map[string]string{"shadow": "1"}
			if version != "" {
				ev.Tags["routine_version"] = version
			}
			if shadowHub != nil {
				hub = shadowHub
			}
		}
		if hub.Client() != nil {
			ev.Contexts = map[string]sentry.Context{"log": fields}
			hub.CaptureEvent(ev)
		}
	}
	return h.Handler.Handle(ctx, r)
}

func (h ErrorHandler) WithAttrs(as []slog.Attr) slog.Handler {
	return ErrorHandler{Handler: h.Handler.WithAttrs(as), attrs: append(append([]slog.Attr(nil), h.attrs...), as...)}
}

func (h ErrorHandler) WithGroup(n string) slog.Handler {
	return ErrorHandler{Handler: h.Handler.WithGroup(n), attrs: h.attrs}
}
