package hostagent

import (
	"context"
	"log/slog"
	"sync/atomic"

	"github.com/chipi/flotilla/internal/hostwire"
)

// Forwarder sends the agent's own log lines to flotillad over its connection, so they reach the
// homelab's logs with flotillad's (G5, G4: on the mini the agent's log file is outside the containers
// that are collected). It wraps the agent's handler: every record still goes to the file; records at
// Info and above are also queued, and a connected session drains the queue. A full queue drops the
// newest line and counts it, so logging never blocks the agent.
type Forwarder struct {
	slog.Handler
	C       chan hostwire.LogBody
	Dropped *atomic.Int64
	attrs   []slog.Attr
}

// ForwardQueue is how many lines wait while the agent is disconnected.
const ForwardQueue = 500

func NewForwarder(h slog.Handler) *Forwarder {
	return &Forwarder{Handler: h, C: make(chan hostwire.LogBody, ForwardQueue), Dropped: new(atomic.Int64)}
}

func (f *Forwarder) Handle(ctx context.Context, r slog.Record) error {
	err := f.Handler.Handle(ctx, r)
	if r.Level < slog.LevelInfo {
		return err
	}
	b := hostwire.LogBody{At: r.Time.UTC(), Level: r.Level.String(), Msg: r.Message, Attrs: map[string]string{}}
	for _, a := range f.attrs {
		b.Attrs[a.Key] = a.Value.String()
	}
	r.Attrs(func(a slog.Attr) bool {
		b.Attrs[a.Key] = a.Value.String()
		return true
	})
	select {
	case f.C <- b:
	default:
		f.Dropped.Add(1)
	}
	return err
}

func (f *Forwarder) WithAttrs(as []slog.Attr) slog.Handler {
	return &Forwarder{Handler: f.Handler.WithAttrs(as), C: f.C, Dropped: f.Dropped, attrs: append(append([]slog.Attr{}, f.attrs...), as...)}
}

func (f *Forwarder) WithGroup(name string) slog.Handler {
	return &Forwarder{Handler: f.Handler.WithGroup(name), C: f.C, Dropped: f.Dropped, attrs: f.attrs}
}
