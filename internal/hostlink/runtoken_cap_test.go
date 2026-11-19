package hostlink

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/chipi/flotilla/internal/hostwire"
)

func hello(t *testing.T, require bool, caps []string) (hostwire.Frame, *Server) {
	t.Helper()
	s := &Server{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Auth: okAuth{}, Version: "v1", Heartbeat: time.Minute, RequireRunToken: require}
	srv, cli := tlsPair(t)
	go s.handleWorker(context.Background(), srv, "cert", "w-remote-w-node", "remote-w")
	if err := hostwire.Write(cli, hostwire.WorkerHello, hostwire.WorkerHelloBody{Version: "v1", Worker: "remote-w", Caps: caps}); err != nil {
		t.Fatal(err)
	}
	cli.SetReadDeadline(time.Now().Add(5 * time.Second))
	f, err := hostwire.Read(cli)
	if err != nil {
		t.Fatal(err)
	}
	return f, s
}

// Under run_tokens: required a remote worker whose agent does not list the run_token capability is refused at link time, with the way out; under
// accept it connects as before. An agent that lists it connects either way.
func TestARemoteAgentWithoutRunTokensIsRefusedWhenRequired(t *testing.T) {
	f, s := hello(t, true, nil)
	if f.Type == hostwire.Welcome {
		t.Fatal("an agent without the capability was welcomed under required")
	}
	if sts := s.WorkerStates(); len(sts) != 1 || sts[0].Refused == "" || sts[0].Connected {
		t.Errorf("state: %+v", sts)
	}
	if f, _ := hello(t, true, []string{hostwire.CapRunToken}); f.Type != hostwire.Welcome {
		t.Errorf("a capable agent: %s", f.Type)
	}
	if f, _ := hello(t, false, nil); f.Type != hostwire.Welcome {
		t.Errorf("accept: an older agent: %s", f.Type)
	}
}

// The hello grew an added field: an older agent's hello (no caps) still decodes, and a newer one's reads as listing the capability.
func TestTheHelloIsBackwardReadable(t *testing.T) {
	var old hostwire.WorkerHelloBody
	if err := json.Unmarshal([]byte(`{"version":"v1","worker":"w"}`), &old); err != nil || old.HasCap(hostwire.CapRunToken) {
		t.Fatalf("%+v %v", old, err)
	}
	b, _ := json.Marshal(hostwire.WorkerHelloBody{Version: "v1", Worker: "w", Caps: []string{hostwire.CapRunToken}})
	var now hostwire.WorkerHelloBody
	if err := json.Unmarshal(b, &now); err != nil || !now.HasCap(hostwire.CapRunToken) {
		t.Fatalf("%s %+v %v", b, now, err)
	}
}
