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

// Errors sends flotillad's own ERROR log records to GlitchTip (Sentry protocol). A routine's failed run
// is the routine's business (the stale alert and the run record carry it), so only records flotillad
// logs at ERROR are sent: its store, its listener, its config. dsnFile empty = off.
//
// One issue per message: the fingerprint is the log message alone, never its values (homelab
// GlitchTip rule "one error = one issue").
func Errors(dsnFile, env, release string) (func(), error) {
	if dsnFile == "" {
		return func() {}, nil
	}
	raw, err := os.ReadFile(dsnFile)
	if err != nil {
		return nil, err
	}
	dsn := strings.TrimSpace(string(raw))
	if i := strings.Index(dsn, "="); i >= 0 && !strings.HasPrefix(dsn, "http") { // KEY=value file
		dsn = strings.Trim(strings.TrimSpace(dsn[i+1:]), `"'`)
	}
	if err := sentry.Init(sentry.ClientOptions{Dsn: dsn, Environment: env, Release: release,
		ServerName: Service, AttachStacktrace: true}); err != nil {
		return nil, fmt.Errorf("sentry init: %w", err)
	}
	return func() { sentry.Flush(5 * time.Second) }, nil
}

// ErrorHandler wraps a slog handler: every record at ERROR or above also goes to GlitchTip.
type ErrorHandler struct{ slog.Handler }

func (h ErrorHandler) Handle(ctx context.Context, r slog.Record) error {
	if r.Level >= slog.LevelError && sentry.CurrentHub().Client() != nil {
		ev := sentry.NewEvent()
		ev.Level = sentry.LevelError
		ev.Message = r.Message
		ev.Fingerprint = []string{"flotillad", r.Message}
		fields := sentry.Context{}
		r.Attrs(func(a slog.Attr) bool { fields[a.Key] = a.Value.String(); return true })
		ev.Contexts = map[string]sentry.Context{"log": fields}
		sentry.CaptureEvent(ev)
	}
	return h.Handler.Handle(ctx, r)
}

func (h ErrorHandler) WithAttrs(as []slog.Attr) slog.Handler {
	return ErrorHandler{h.Handler.WithAttrs(as)}
}

func (h ErrorHandler) WithGroup(n string) slog.Handler { return ErrorHandler{h.Handler.WithGroup(n)} }
