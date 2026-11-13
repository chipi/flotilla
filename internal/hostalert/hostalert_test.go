package hostalert

import (
	"strings"
	"testing"
	"time"

	"github.com/chipi/flotilla/internal/store"
)

var now = time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)

func at(d time.Duration) string { return now.Add(d).Format(store.TimeFormat) }

func ptr(s string) *string { return &s }

func host(name string, mod func(*store.Host)) store.Host {
	h := store.Host{Name: name, EnrolledAt: ptr(at(-48 * time.Hour)), Connected: true, LastContact: at(-10 * time.Second),
		CertNotAfter: at(20 * 24 * time.Hour)}
	mod(&h)
	return h
}

func kinds(as []Alert) string {
	var out []string
	for _, a := range as {
		out = append(out, a.Host+":"+a.Kind)
	}
	return strings.Join(out, " ")
}

func TestCheck(t *testing.T) {
	hosts := []store.Host{
		host("ok", func(h *store.Host) {}),
		host("gone", func(h *store.Host) { h.Connected = false; h.LastContact = at(-6 * time.Minute) }),
		host("blip", func(h *store.Host) { h.Connected = false; h.LastContact = at(-4 * time.Minute) }),
		host("never", func(h *store.Host) {
			h.Connected = false
			h.LastContact = ""
			h.EnrolledAt = ptr(at(-10 * time.Minute))
		}),
		host("ahead", func(h *store.Host) { h.ClockOffsetMS = 31000 }),
		host("behind", func(h *store.Host) { h.ClockOffsetMS = -31000 }),
		host("within", func(h *store.Host) { h.ClockOffsetMS = 30000 }),
		host("skew-but-down", func(h *store.Host) { h.Connected = false; h.LastContact = at(-time.Minute); h.ClockOffsetMS = 99000 }),
		host("expiring", func(h *store.Host) { h.CertNotAfter = at(6 * 24 * time.Hour) }),
		host("revoked", func(h *store.Host) {
			h.Connected = false
			h.RevokedAt = ptr(at(-time.Hour))
			h.LastContact = at(-time.Hour)
		}),
		host("unenrolled", func(h *store.Host) { h.EnrolledAt = nil; h.Connected = false }),
	}
	got := kinds(Check(hosts, now, Defaults))
	want := "gone:not_connected never:not_connected ahead:clock_skew behind:clock_skew expiring:certificate_expiring"
	if got != want {
		t.Fatalf("alerts:\n %s\nwant:\n %s", got, want)
	}
}

func TestWatcher(t *testing.T) {
	var w Watcher
	a, b := Alert{"mini", NotConnected, "x"}, Alert{"mini", ClockSkew, "y"}
	if s, c := w.Update([]Alert{a}); kinds(s) != "mini:not_connected" || len(c) != 0 {
		t.Fatalf("first: %v %v", s, c)
	}
	if s, c := w.Update([]Alert{a, b}); kinds(s) != "mini:clock_skew" || len(c) != 0 {
		t.Fatalf("still on is not started again: %v %v", s, c)
	}
	if s, c := w.Update([]Alert{b}); len(s) != 0 || kinds(c) != "mini:not_connected" {
		t.Fatalf("cleared: %v %v", s, c)
	}
	if s, c := w.Update(nil); len(s) != 0 || kinds(c) != "mini:clock_skew" {
		t.Fatalf("all cleared: %v %v", s, c)
	}
}
