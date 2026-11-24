package hostlink

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/chipi/flotilla/internal/hostwire"
)

// dial runs one connection of the remote worker with hello's version through handleWorker, and returns
// its first reply's type once it has been answered (a refusal ends the handler; a welcome ends it when the
// client closes).
func dialWorker(t *testing.T, s *Server, version string) string {
	t.Helper()
	srv, cli := tlsPair(t)
	done := make(chan struct{})
	go func() { s.handleWorker(context.Background(), srv, "cert", "w-remote-w-node", "remote-w"); close(done) }()
	if err := hostwire.Write(cli, hostwire.WorkerHello, hostwire.WorkerHelloBody{Version: version, Worker: "remote-w"}); err != nil {
		t.Fatal(err)
	}
	cli.SetReadDeadline(time.Now().Add(5 * time.Second))
	f, err := hostwire.Read(cli)
	if err != nil {
		t.Fatal(err)
	}
	cli.Close()
	<-done
	return f.Type
}

// A remote worker at another version retries every few seconds until it is updated (2026-11-24: 3,592 ERROR
// events in bursts of ~1,600 between a server deploy and the laptop's workers being updated). The refusal is
// right; one ERROR per (worker version, flotillad version) says so, and the acceptance after it is one INFO.
func TestVersionRefusalIsLoggedOnceAtError(t *testing.T) {
	var buf bytes.Buffer
	s := &Server{Log: slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})), Auth: okAuth{}, Version: "v2", Heartbeat: time.Minute}
	count := func(level, msg string) int { return strings.Count(buf.String(), "level="+level+" msg=\""+msg) }
	for i := 0; i < 5; i++ {
		if got := dialWorker(t, s, "v1"); got != hostwire.Refusal {
			t.Fatalf("attempt %d: %q, want a refusal", i, got)
		}
	}
	if n := count("ERROR", "remote worker refused: its version"); n != 1 {
		t.Fatalf("%d ERROR lines for five refused attempts, want 1:\n%s", n, buf.String())
	}
	if got := s.WorkerStates()[0].Refused; !strings.Contains(got, "version v1, not flotillad's v2") {
		t.Fatalf("the worker's status lost the mismatch: %q", got)
	}
	if got := dialWorker(t, s, "v3"); got != hostwire.Refusal {
		t.Fatalf("v3: %q", got)
	}
	if n := count("ERROR", "remote worker refused: its version"); n != 2 {
		t.Fatalf("a new version pair is a new ERROR: %d lines, want 2", n)
	}
	if got := dialWorker(t, s, "v2"); got != hostwire.Welcome {
		t.Fatalf("matching version: %q", got)
	}
	if n := count("INFO", "remote worker accepted again"); n != 1 {
		t.Fatalf("%d INFO lines for the acceptance, want 1:\n%s", n, buf.String())
	}
	dialWorker(t, s, "v2")
	if n := count("INFO", "remote worker accepted again"); n != 1 {
		t.Fatalf("a second acceptance repeats the INFO: %d", n)
	}
}
