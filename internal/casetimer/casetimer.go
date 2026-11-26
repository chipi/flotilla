// Package casetimer is RFC-0004 §3.5's timers for stuck states: a case left in detected too long is
// alerted (the diagnosing worker may be down); one left in handed_over too long is escalated; a watching
// case (ADR-0023) is due its notices on a bounded schedule, which only records that they are due. The
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
	// WatchNotify is when a watched case's notices after the first fall due, from the day it began to be watched (ADR-0023);
	// WatchAckSnooze is how long an acknowledged notice holds the next one back.
	WatchNotify    []time.Duration `yaml:"watch_notify"`
	WatchAckSnooze time.Duration   `yaml:"watch_ack_snooze"`
}

const (
	DefaultDetectedAlert      = 30 * time.Minute
	DefaultHandedOverEscalate = 24 * time.Hour
	DefaultWatchAckSnooze     = 24 * time.Hour
	// A watched case is told at once, then at the steps, then every WatchRepeat, at most MaxNotices in all.
	WatchRepeat = 24 * time.Hour
	MaxNotices  = 7
	stuckEvent  = "timer.stuck"
	actor       = "flotillad"
)

// DefaultWatchNotify is the notice schedule after the first one: 4 hours, then 24 hours (ADR-0023).
var DefaultWatchNotify = []time.Duration{4 * time.Hour, 24 * time.Hour}

func (c Config) withDefaults() Config {
	if c.DetectedAlert <= 0 {
		c.DetectedAlert = DefaultDetectedAlert
	}
	if c.HandedOverEscalate <= 0 {
		c.HandedOverEscalate = DefaultHandedOverEscalate
	}
	if len(c.WatchNotify) == 0 {
		c.WatchNotify = DefaultWatchNotify
	}
	if c.WatchAckSnooze <= 0 {
		c.WatchAckSnooze = DefaultWatchAckSnooze
	}
	return c
}

// Hold says whether a handed-over case of this kind and stage cannot run now because its acting routine is
// held (its fleet STOPped or the routine paused), and when it was last released ("" for never held). A held
// case is not escalated, and its hand-over clock starts again at the release: a STOP longer than the limit
// does not turn the whole queue into escalations. nil holds nothing.
type Hold func(ctx context.Context, kind, stage string) (held bool, releasedAt string, err error)

// Run checks every open case once against its status's limit.
func Run(ctx context.Context, st *store.Store, now time.Time, c Config, log *slog.Logger, hold Hold) error {
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
		if !ok {
			continue
		}
		if hold != nil {
			held, released, err := hold(ctx, x.Kind, x.Stage)
			if err != nil {
				return err
			}
			if held {
				continue
			}
			if t, err := time.Parse(time.RFC3339Nano, released); err == nil && t.After(start) {
				start = t
			}
		}
		if now.Sub(start) < c.HandedOverEscalate {
			continue
		}
		detail := fmt.Sprintf("handed over since %s, longer than %s", start.Format(time.RFC3339), c.HandedOverEscalate)
		if err := st.MoveCase(ctx, x.ID, cases.Escalated, cases.ByTimer, "", actor, detail); err != nil {
			return err
		}
		log.Warn("case escalated: handed over too long", "case", x.ID, "kind", x.Kind, "target", x.Target, "since", start)
	}
	watching, err := st.Cases(ctx, store.CaseFilter{Status: cases.Watching, Limit: 500})
	if err != nil {
		return err
	}
	for _, x := range watching {
		if err := watchNotice(ctx, st, x, now, c, log); err != nil {
			return err
		}
	}
	return nil
}

// NoticeDue is when notice n (1 for the first) of a watching period that began at start falls due: at once, then
// at each of steps from the start, then every WatchRepeat after the last step.
func NoticeDue(start time.Time, n int, steps []time.Duration) time.Time {
	switch {
	case n <= 1:
		return start
	case n-2 < len(steps):
		return start.Add(steps[n-2])
	}
	return start.Add(steps[len(steps)-1] + time.Duration(n-1-len(steps))*WatchRepeat)
}

// watchNotice writes the next notice a watching case is due, at most one per pass. A timer that was down skips to the
// latest notice that is due rather than writing the ones it missed, so a restart is one notice and not a burst. Nothing
// here changes the case's status: only its monitor or the operator closes a watched case.
func watchNotice(ctx context.Context, st *store.Store, x store.Case, now time.Time, c Config, log *slog.Logger) error {
	start, err := time.Parse(time.RFC3339Nano, x.StatusAt)
	if err != nil {
		return nil
	}
	all, err := st.Notices(ctx, x.ID)
	if err != nil {
		return err
	}
	var last *store.Notice // the newest notice of this watching period (a case handed back and watched again starts anew)
	for i := range all {
		if t, err := time.Parse(time.RFC3339Nano, all[i].DueAt); err == nil && !t.Before(start) && all[i].Kind == store.NoticeWatching {
			last = &all[i]
		}
	}
	n := nextNotice(start, now, last, c)
	if n == 0 {
		return nil
	}
	due := NoticeDue(start, n, c.WatchNotify).UTC().Format(store.TimeFormat)
	if err := st.DueNotice(ctx, x.ID, store.NoticeWatching, n, due); err != nil {
		return err
	}
	log.Info("a watched case is due a notice", "case", x.ID, "kind", x.Kind, "target", x.Target, "n", n, "repeats", x.Repeats)
	return nil
}

// nextNotice is the number of the notice a watching case is due at now, 0 for none. last is the newest notice of the
// period, nil for none: the first is due at once, whatever the clocks say.
func nextNotice(start, now time.Time, last *store.Notice, c Config) int {
	if last == nil {
		return 1
	}
	if last.N >= MaxNotices {
		return 0
	}
	// Acknowledging snoozes: nothing falls due until the snooze has passed.
	if t, err := time.Parse(time.RFC3339Nano, last.AckedAt); err == nil && now.Before(t.Add(c.WatchAckSnooze)) {
		return 0
	}
	n := 0
	for k := last.N + 1; k <= MaxNotices && !NoticeDue(start, k, c.WatchNotify).After(now); k++ {
		n = k
	}
	return n
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
