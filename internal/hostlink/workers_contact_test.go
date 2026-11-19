package hostlink

import (
	"bytes"
	"context"
	"crypto/x509"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/chipi/flotilla/internal/hostwire"
)

type okAuth struct{}

func (okAuth) AuthenticateHost(context.Context, []*x509.Certificate) (string, error) {
	return "w-remote-w-node", nil
}
func (okAuth) EnrolHost(context.Context, string, string, []byte) ([]byte, error) { return nil, nil }
func (okAuth) RenewHost(context.Context, string, string, []byte) ([]byte, error) { return nil, nil }

// A remote worker's link is a host in the store's sense (enrolled, listed, alerted on): its connection,
// heartbeat and drop reach OnContact under its identity, or `host list` shows it never connected and
// hostalert logs not_connected for a link that works.
func TestWorkerLinkContactIsReported(t *testing.T) {
	var buf bytes.Buffer
	var mu sync.Mutex
	var got []HostState
	s := &Server{Log: slog.New(slog.NewTextHandler(&buf, nil)), Auth: okAuth{}, Version: "v1", Heartbeat: time.Minute,
		OnContact: func(h HostState) { mu.Lock(); got = append(got, h); mu.Unlock() }}
	last := func() (HostState, int) {
		mu.Lock()
		defer mu.Unlock()
		if len(got) == 0 {
			return HostState{}, 0
		}
		return got[len(got)-1], len(got)
	}
	srv, cli := tlsPair(t)
	done := make(chan struct{})
	go func() { s.handleWorker(context.Background(), srv, "cert", "w-remote-w-node", "remote-w"); close(done) }()
	if err := hostwire.Write(cli, hostwire.WorkerHello, hostwire.WorkerHelloBody{Version: "v1", Worker: "remote-w"}); err != nil {
		t.Fatal(err)
	}
	cli.SetReadDeadline(time.Now().Add(5 * time.Second))
	if f, err := hostwire.Read(cli); err != nil || f.Type != hostwire.Welcome {
		t.Fatalf("welcome: %v %v", f, err)
	}
	if err := hostwire.Write(cli, hostwire.Heartbeat, hostwire.HeartbeatBody{}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if h, n := last(); n >= 2 {
			if h.Host != "w-remote-w-node" || !h.Connected || h.Version != "v1" || h.LastContact.IsZero() {
				t.Fatalf("after connect and heartbeat: %+v", h)
			}
			break
		}
		if time.Now().After(deadline) {
			h, n := last()
			t.Fatalf("OnContact heard %d times, last %+v: the link's connection is not recorded", n, h)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cli.Close()
	<-done
	if h, _ := last(); h.Host != "w-remote-w-node" || h.Connected {
		t.Fatalf("after the drop: %+v", h)
	}
}
