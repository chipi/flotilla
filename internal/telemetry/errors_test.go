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
	flush, err := Errors(f, "test", "sha-abc1234")
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(ErrorHandler{slog.NewJSONHandler(io.Discard, nil)})
	log.Info("run start", "job", "x")                              // not sent
	log.Warn("run end", "status", "error")                         // not sent: a job's failure is the job's
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
	flush, err := Errors("", "test", "dev")
	if err != nil {
		t.Fatal(err)
	}
	flush()
}
