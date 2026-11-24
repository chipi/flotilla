package hostlink

import (
	"bytes"
	"context"
	"crypto/tls"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chipi/flotilla/internal/hostwire"
)

// A connection's last OnContact (its drop) writes flotillad's store; at a restart the store was closed under it ("host contact: the store":
// sql: database is closed, 24 ERRORs, one burst per deploy). Close returns only when every handler is done, so nothing calls OnContact
// after it, and the caller can close the store next.
func TestCloseEndsHandlersBeforeItReturns(t *testing.T) {
	var buf bytes.Buffer
	var closed atomic.Bool
	var late atomic.Int32
	s := &Server{Log: slog.New(slog.NewTextHandler(&buf, nil)), Auth: okAuth{}, Version: "v1", Heartbeat: time.Minute,
		OnContact: func(h HostState) {
			if !h.Connected {
				time.Sleep(100 * time.Millisecond) // the store write of the drop
			}
			if closed.Load() {
				late.Add(1)
			}
		}}
	srv, cli := tlsPair(t)
	if !s.spawn(srv, func(tc *tls.Conn) { s.handleWorker(context.Background(), tc, "cert", "w-remote-w-node", "remote-w") }) {
		t.Fatal("not accepted")
	}
	if err := hostwire.Write(cli, hostwire.WorkerHello, hostwire.WorkerHelloBody{Version: "v1", Worker: "remote-w"}); err != nil {
		t.Fatal(err)
	}
	cli.SetReadDeadline(time.Now().Add(5 * time.Second))
	if f, err := hostwire.Read(cli); err != nil || f.Type != hostwire.Welcome {
		t.Fatalf("welcome: %v %v", f, err)
	}
	s.Close()
	closed.Store(true)
	time.Sleep(300 * time.Millisecond)
	if n := late.Load(); n != 0 {
		t.Fatalf("%d OnContact calls after Close returned: the store would be closed under them", n)
	}
	if s.spawn(srv, func(*tls.Conn) { t.Error("a handler started after Close") }) {
		t.Fatal("a connection was accepted after Close")
	}
}
