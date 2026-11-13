// Package hostalert is flotillad's watch on its host agents (ADR-0006 §10): an enrolled host not
// connected for a while, a clock skewed past what signed executions tolerate, a certificate close to
// its expiry (the agent renews at 10 days; under 7 means renewal is failing). Check is pure; Watcher
// turns its answers into changes, so an alert logs once when it starts and once when it clears.
package hostalert

import (
	"fmt"
	"sort"
	"time"

	"github.com/chipi/flotilla/internal/store"
)

// Kinds of alert.
const (
	NotConnected        = "not_connected"
	ClockSkew           = "clock_skew"
	CertificateExpiring = "certificate_expiring"
)

type Limits struct {
	NotConnected time.Duration
	ClockSkew    time.Duration
	CertExpiry   time.Duration
}

// Defaults: 5 minutes unreachable; the 30 seconds of ADR-0006 §4; 7 days of certificate left.
var Defaults = Limits{NotConnected: 5 * time.Minute, ClockSkew: 30 * time.Second, CertExpiry: 7 * 24 * time.Hour}

type Alert struct {
	Host   string `json:"host"`
	Kind   string `json:"kind"`
	Detail string `json:"detail"`
}

func parse(s string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339Nano, s)
	return t, err == nil
}

// Check returns the alerts for every enrolled, unrevoked host at now.
func Check(hosts []store.Host, now time.Time, l Limits) []Alert {
	var out []Alert
	for _, h := range hosts {
		if h.EnrolledAt == nil || h.RevokedAt != nil {
			continue
		}
		if !h.Connected {
			since, ok := parse(h.LastContact)
			what := "last contact"
			if !ok { // enrolled, never connected
				since, _ = parse(*h.EnrolledAt)
				what = "enrolled"
			}
			if d := now.Sub(since); d > l.NotConnected {
				out = append(out, Alert{h.Name, NotConnected, fmt.Sprintf("not connected: %s %s ago", what, d.Round(time.Second))})
			}
		}
		if off := time.Duration(h.ClockOffsetMS) * time.Millisecond; h.Connected && (off > l.ClockSkew || off < -l.ClockSkew) {
			out = append(out, Alert{h.Name, ClockSkew, fmt.Sprintf("its clock is %s off flotillad's (more than %s)", off, l.ClockSkew)})
		}
		if until, ok := parse(h.CertNotAfter); ok && until.Sub(now) < l.CertExpiry {
			out = append(out, Alert{h.Name, CertificateExpiring, fmt.Sprintf("its certificate expires %s (renewal should have come at 10 days)", until.UTC().Format(time.RFC3339))})
		}
	}
	return out
}

// Watcher remembers which alerts are on.
type Watcher struct {
	on map[[2]string]Alert
}

// Update takes Check's answer and returns the alerts that started and the ones that cleared.
func (w *Watcher) Update(alerts []Alert) (started, cleared []Alert) {
	now := map[[2]string]Alert{}
	for _, a := range alerts {
		k := [2]string{a.Host, a.Kind}
		now[k] = a
		if _, was := w.on[k]; !was {
			started = append(started, a)
		}
	}
	for k, a := range w.on {
		if _, still := now[k]; !still {
			cleared = append(cleared, a)
		}
	}
	sort.Slice(cleared, func(i, j int) bool { return cleared[i].Host+cleared[i].Kind < cleared[j].Host+cleared[j].Kind })
	w.on = now
	return started, cleared
}
