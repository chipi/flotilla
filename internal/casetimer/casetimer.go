// Package casetimer is RFC-0004 §3.5's timers for stuck states: a case left in detected too long is
// alerted (the diagnosing worker may be down); one left in handed_over too long is escalated. The
// checking deadline is each action's check window, kept with the check runs in internal/dispatch; the
// executing deadline (the action's timeout) is not built: a host's execution ends with its result or
// as unknown (internal/api), which covers the host actions there are.
package casetimer

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/chipi/flotilla/internal/cases"
	"github.com/chipi/flotilla/internal/store"
)

// Config is how long a case may stay. Zero means the default (operator, 2026-11-13).
type Config struct {
	DetectedAlert      time.Duration `yaml:"detected_alert"`
	HandedOverEscalate time.Duration `yaml:"handed_over_escalate"`
}

const (
	DefaultDetectedAlert      = 30 * time.Minute
	DefaultHandedOverEscalate = 24 * time.Hour
	stuckEvent                = "timer.stuck"
	actor                     = "flotillad"
)

func (c Config) withDefaults() Config {
	if c.DetectedAlert <= 0 {
		c.DetectedAlert = DefaultDetectedAlert
	}
	if c.HandedOverEscalate <= 0 {
		c.HandedOverEscalate = DefaultHandedOverEscalate
	}
	return c
}

// Run checks every open case once against its status's limit.
func Run(ctx context.Context, st *store.Store, now time.Time, c Config, log *slog.Logger) error {
	c = c.withDefaults()
	detected, err := st.Cases(ctx, store.CaseFilter{Status: cases.Detected, Limit: 500})
	if err != nil {
		return err
	}
	for _, x := range detected {
		since, err := time.Parse(time.RFC3339Nano, x.StatusAt)
		if err != nil || now.Sub(since) < c.DetectedAlert {
			continue
		}
		ev, err := st.CaseEvents(ctx, x.ID)
		if err != nil {
			return err
		}
		if alerted(ev) {
			continue
		}
		// One message for every case, so GlitchTip groups them as one issue; the case is in the attributes.
		log.Error("a case is stuck in detected: is the diagnosing worker running?", "case", x.ID, "kind", x.Kind,
			"target", x.Target, "since", x.StatusAt, "limit", c.DetectedAlert.String())
		if err := st.AddCaseEvent(ctx, x.ID, stuckEvent, actor, fmt.Sprintf("detected since %s, longer than %s", x.StatusAt, c.DetectedAlert)); err != nil {
			return err
		}
	}
	handed, err := st.Cases(ctx, store.CaseFilter{Status: cases.HandedOver, Limit: 500})
	if err != nil {
		return err
	}
	for _, x := range handed {
		ev, err := st.CaseEvents(ctx, x.ID)
		if err != nil {
			return err
		}
		start, ok := handedOverSince(ev)
		if !ok || now.Sub(start) < c.HandedOverEscalate {
			continue
		}
		detail := fmt.Sprintf("handed over since %s, longer than %s", start.Format(time.RFC3339), c.HandedOverEscalate)
		if err := st.MoveCase(ctx, x.ID, cases.Escalated, cases.ByTimer, "", actor, detail); err != nil {
			return err
		}
		log.Warn("case escalated: handed over too long", "case", x.ID, "kind", x.Kind, "target", x.Target, "since", start)
	}
	return nil
}

func alerted(ev []store.CaseEvent) bool {
	for _, e := range ev {
		if e.Kind == stuckEvent {
			return true
		}
	}
	return false
}

// handedOverSince is when the hand-over clock started: the latest move into handed_over that was not a
// deferral (planned -> handed_over). A deferral keeps the clock, so a long hold surfaces as an
// escalation; an operator's reassignment (escalated -> handed_over) starts a new one.
func handedOverSince(ev []store.CaseEvent) (time.Time, bool) {
	var start string
	for _, e := range ev {
		if e.To == cases.HandedOver && e.From != cases.Planned && e.Kind == "case."+cases.HandedOver {
			start = e.At
		}
	}
	t, err := time.Parse(time.RFC3339Nano, start)
	return t, err == nil
}
