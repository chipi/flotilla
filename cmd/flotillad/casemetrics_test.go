package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chipi/flotilla/internal/store"
	"github.com/chipi/flotilla/internal/telemetry"
)

// The heartbeat's case series, from a real store: an opening has no from label and a move that does not close has no resolution.
func TestTheHeartbeatPushesTheCasesSeries(t *testing.T) {
	var bodies []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
	}))
	defer ts.Close()
	st, err := store.Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	k := store.Key{Kind: "target", Value: "seo/page:home"}
	if _, _, err := st.OpenCase(context.Background(), store.NewCase{Kind: "seo_live_regression", Target: "page:home", Project: "seo",
		Keys: []store.Key{k}, Canonical: k}, "seo/site-check", ""); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	pushCaseMetrics(context.Background(), &telemetry.Pusher{URL: ts.URL, Env: "test", Log: log}, st, log)
	all := strings.Join(bodies, "\n")
	for _, want := range []string{
		`flotilla_cases_open{environment="test",fleet="seo",kind="seo_live_regression",service="flotillad",status="detected"} 1`,
		`flotilla_case_moves_total{environment="test",fleet="seo",kind="seo_live_regression",service="flotillad",to="detected"} 1`,
	} {
		if !strings.Contains(all, want) {
			t.Fatalf("want %s in %s", want, all)
		}
	}
}
