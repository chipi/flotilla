package hostlink

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/chipi/flotilla/internal/hostwire"
)

// A host's log lines: logged as "host log" with the host's name, at their level, cut to size, at most
// hostLogPerMinute a minute; what is over is counted and logged when the minute ends.
func TestHostLog(t *testing.T) {
	var buf bytes.Buffer
	clock := time.Date(2026, 11, 15, 8, 0, 0, 0, time.UTC)
	s := &Server{Log: slog.New(slog.NewJSONHandler(&buf, nil)), Now: func() time.Time { return clock }}
	b := budget{}
	s.hostLog(context.Background(), "mini", &b, hostwire.LogBody{At: clock, Level: "WARN", Msg: strings.Repeat("x", 3000),
		Attrs: map[string]string{"execution": "e1", "host": "dgx"}})
	var first map[string]any
	if err := json.Unmarshal([]byte(strings.SplitN(buf.String(), "\n", 2)[0]), &first); err != nil {
		t.Fatal(err)
	}
	if first["msg"] != "host log" || first["host"] != "mini" || first["level"] != "WARN" || len(first["line"].(string)) != hostLogText ||
		first["a_execution"] != "e1" {
		t.Fatalf("the line as logged: %v", first)
	}
	if strings.Contains(buf.String(), `"a_host"`) {
		t.Error("an agent's attribute may not stand in for the host's name")
	}
	for i := 0; i < hostLogPerMinute+5; i++ {
		s.hostLog(context.Background(), "mini", &b, hostwire.LogBody{Level: "INFO", Msg: "line"})
	}
	if n := strings.Count(buf.String(), `"msg":"host log"`); n != hostLogPerMinute {
		t.Fatalf("%d lines logged in a minute, want %d", n, hostLogPerMinute)
	}
	clock = clock.Add(time.Minute)
	s.hostLog(context.Background(), "mini", &b, hostwire.LogBody{Level: "INFO", Msg: "next minute"})
	if !strings.Contains(buf.String(), `"msg":"host log lines dropped","host":"mini","dropped":6`) || !strings.Contains(buf.String(), `"line":"next minute"`) {
		t.Fatalf("the dropped count and the next minute's line: %s", buf.String()[len(buf.String())-400:])
	}
}
