package telemetry

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/getsentry/sentry-go"
)

// A stand-in GlitchTip: records what the SDK sends.
func TestErrorsReachGlitchTipWithAFixedFingerprint(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
		w.Write([]byte(`{"id":"1"}`))
	}))
	defer ts.Close()
	dsn := strings.Replace(ts.URL, "http://", "http://publickey@", 1) + "/42"
	f := filepath.Join(t.TempDir(), "dsn.env")
	os.WriteFile(f, []byte("SENTRY_DSN="+dsn+"\n"), 0o600)
	flush, err := Errors(f, "", "test", "sha-abc1234")
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(ErrorHandler{Handler: slog.NewJSONHandler(io.Discard, nil)})
	log.Info("run start", "routine", "x")                          // not sent
	log.Warn("run end", "status", "error")                         // not sent: a routine's failure is the routine's
	log.Error("store", "err", "unable to open database file (14)") // sent
	flush()
	sentry.Flush(2 * time.Second)
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 1 {
		t.Fatalf("want exactly the ERROR record sent, got %d payloads", len(bodies))
	}
	b := bodies[0]
	for _, want := range []string{`"message":"store"`, `"environment":"test"`, `"release":"sha-abc1234"`, `"fingerprint":["flotillad","store"]`, `unable to open database file`} {
		if !strings.Contains(b, want) {
			t.Fatalf("payload lacks %s:\n%s", want, b)
		}
	}
}

func TestErrorsOffWithoutADSNFile(t *testing.T) {
	flush, err := Errors("", "", "test", "dev")
	if err != nil {
		t.Fatal(err)
	}
	flush()
}

// glitchStandIn records what the SDK sends to one DSN.
type glitchStandIn struct {
	mu     sync.Mutex
	bodies []string
	dsn    string
}

func newGlitchStandIn(t *testing.T) *glitchStandIn {
	t.Helper()
	g := &glitchStandIn{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		g.mu.Lock()
		g.bodies = append(g.bodies, string(b))
		g.mu.Unlock()
		w.Write([]byte(`{"id":"1"}`))
	}))
	t.Cleanup(ts.Close)
	g.dsn = strings.Replace(ts.URL, "http://", "http://publickey@", 1) + "/42"
	return g
}

func (g *glitchStandIn) file(t *testing.T) string {
	f := filepath.Join(t.TempDir(), "dsn.env")
	os.WriteFile(f, []byte("SENTRY_DSN="+g.dsn+"\n"), 0o600)
	return f
}

func (g *glitchStandIn) seen() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.bodies...)
}

func logsOf(t *testing.T) (live, shadow *slog.Logger) {
	t.Helper()
	live = slog.New(ErrorHandler{Handler: slog.NewJSONHandler(io.Discard, nil)}).With("service", Service)
	return live, ShadowLogger(live, 2)
}

// With no DSN of their own, a shadow run's errors reach the main DSN tagged shadow=1 with a fingerprint that holds "shadow",
// so they never open or reopen the live issue of the same message; the environment is the installation's.
func TestAShadowsErrorsAreTaggedAndNeverShareALiveIssue(t *testing.T) {
	main := newGlitchStandIn(t)
	flush, err := Errors(main.file(t), "", "operations", "sha-abc1234")
	if err != nil {
		t.Fatal(err)
	}
	live, shadow := logsOf(t)
	live.Error("store", "err", "unable to open database file (14)")
	shadow.Error("store", "err", "unable to open database file (14)")
	shadow.Warn("run end", "status", "error") // not an ERROR: not sent
	flush()
	got := main.seen()
	if len(got) != 2 {
		t.Fatalf("one live and one shadow event: %d\n%v", len(got), got)
	}
	var liveEv, shadowEv string
	for _, b := range got {
		if strings.Contains(b, `"shadow"`) && strings.Contains(b, `"fingerprint":["flotillad","shadow","store"]`) {
			shadowEv = b
		} else {
			liveEv = b
		}
	}
	for _, want := range []string{`"fingerprint":["flotillad","shadow","store"]`, `"shadow":"1"`, `"routine_version":"2"`, `"environment":"operations"`, `"release":"sha-abc1234"`} {
		if !strings.Contains(shadowEv, want) {
			t.Errorf("shadow event lacks %s:\n%s", want, shadowEv)
		}
	}
	if !strings.Contains(liveEv, `"fingerprint":["flotillad","store"]`) || strings.Contains(liveEv, `"shadow"`) || strings.Contains(liveEv, "routine_version") {
		t.Errorf("the live event is as it was, unmarked:\n%s", liveEv)
	}
	if !strings.Contains(liveEv, `"environment":"operations"`) {
		t.Errorf("live environment: %s", liveEv)
	}
}

// With errors_shadow_dsn_file, shadow errors go to that project alone and live errors to the main one alone.
func TestAShadowsErrorsGoToTheirOwnDSN(t *testing.T) {
	main, shadowGT := newGlitchStandIn(t), newGlitchStandIn(t)
	flush, err := Errors(main.file(t), shadowGT.file(t), "operations", "sha-abc1234")
	if err != nil {
		t.Fatal(err)
	}
	live, shadow := logsOf(t)
	live.Error("store", "err", "a")
	shadow.Error("store", "err", "b")
	flush()
	if m := main.seen(); len(m) != 1 || strings.Contains(m[0], `"shadow"`) {
		t.Fatalf("the main DSN gets the live event only: %v", m)
	}
	s := shadowGT.seen()
	if len(s) != 1 {
		t.Fatalf("the shadow DSN gets the shadow event only: %v", s)
	}
	for _, want := range []string{`"shadow":"1"`, `"routine_version":"2"`, `"fingerprint":["flotillad","shadow","store"]`, `"environment":"operations"`} {
		if !strings.Contains(s[0], want) {
			t.Errorf("shadow event lacks %s:\n%s", want, s[0])
		}
	}
}

// The shadow project can exist without a live one (the live errors are then off).
func TestAShadowDSNWithoutAMainOne(t *testing.T) {
	shadowGT := newGlitchStandIn(t)
	flush, err := Errors("", shadowGT.file(t), "operations", "sha-abc1234")
	if err != nil {
		t.Fatal(err)
	}
	live, shadow := logsOf(t)
	live.Error("store", "err", "a") // no main client: dropped, as with no DSN at all
	shadow.Error("store", "err", "b")
	flush()
	if s := shadowGT.seen(); len(s) != 1 || !strings.Contains(s[0], `"shadow":"1"`) {
		t.Fatalf("the shadow DSN gets the shadow event: %v", s)
	}
	f2, err := Errors("", "", "operations", "x")
	if err != nil {
		t.Fatal(err)
	}
	f2()
	shadow.Error("store", "err", "c") // everything off: nothing to send to, no panic
	if n := len(shadowGT.seen()); n != 1 {
		t.Fatalf("a shadow event after errors were turned off: %d", n)
	}
}

func TestAnUnreadableShadowDSNFileIsRefused(t *testing.T) {
	if _, err := Errors("", filepath.Join(t.TempDir(), "missing"), "operations", "x"); err == nil {
		t.Fatal("a configured DSN file that cannot be read is an error, as the main one is")
	}
}
