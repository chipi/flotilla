package sched

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

// A routine that names a spend file and left no number in it: its spend counts 0 by default, not by report,
// and flotillad says so (the Fable advisor's review, 2026-11-16).
func TestUnreportedSpendIsLogged(t *testing.T) {
	for _, c := range []struct {
		reported *bool
		warn     bool
	}{{ptr(false), true}, {ptr(true), false}, {nil, false}} {
		s, fr, _ := setup(t, 2)
		var buf bytes.Buffer
		s.Log = slog.New(slog.NewTextHandler(&buf, nil))
		fr.res.SpendReported, fr.res.SpendUSD = c.reported, 0
		if _, err := s.RunNow(context.Background(), "triage/cycle", "manual", "operator"); err != nil {
			t.Fatal(err)
		}
		if got := strings.Contains(buf.String(), "run reported no spend"); got != c.warn {
			t.Fatalf("reported %v: warned %v\n%s", c.reported, got, buf.String())
		}
	}
}

func ptr(b bool) *bool { return &b }
