package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chipi/flotilla/internal/hostalert"
	"github.com/chipi/flotilla/internal/hostca"
	"github.com/chipi/flotilla/internal/hostlink"
	"github.com/chipi/flotilla/internal/store"
)

// writeCA puts a CA's files where flotillad reads them, as `flotilla ca intermediate` leaves them.
func writeCA(t *testing.T, root, inter *hostca.CA) string {
	t.Helper()
	dir := t.TempDir()
	key, _ := hostca.KeyPEM(inter.Key)
	for name, b := range map[string][]byte{"intermediate.crt": hostca.CertPEM(inter.Cert), "intermediate.key": key, "root.crt": hostca.CertPEM(root.Cert)} {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestLoadHostCA(t *testing.T) {
	now := time.Now()
	root, _ := hostca.NewRoot(now)
	inter, _ := hostca.NewIntermediate(root, now)
	dir := writeCA(t, root, inter)
	got, gotRoot, err := loadHostCA(dir)
	if err != nil || !got.Cert.Equal(inter.Cert) || !gotRoot.Equal(root.Cert) {
		t.Fatalf("load: %v", err)
	}
	// an intermediate under another root
	other, _ := hostca.NewRoot(now)
	if _, _, err := loadHostCA(writeCA(t, other, inter)); err == nil {
		t.Fatal("an intermediate the root did not sign")
	}
	os.Remove(filepath.Join(dir, "root.crt"))
	if _, _, err := loadHostCA(dir); err == nil {
		t.Fatal("no root.crt")
	}
}

func TestServerCertRenews(t *testing.T) {
	now := time.Now()
	root, _ := hostca.NewRoot(now)
	inter, _ := hostca.NewIntermediate(root, now)
	clock := now
	sc := &serverCert{ca: inter, names: []string{"flotilla-hosts"}, now: func() time.Time { return clock }}
	a, err := sc.get(nil)
	if err != nil || a.Leaf.DNSNames[0] != "flotilla-hosts" {
		t.Fatalf("first: %v", err)
	}
	if b, _ := sc.get(nil); b != a {
		t.Fatal("a fresh certificate issued again")
	}
	clock = a.Leaf.NotAfter.Add(-renewServer - time.Minute)
	if b, _ := sc.get(nil); b != a {
		t.Fatal("renewed with more than 30 days left")
	}
	clock = a.Leaf.NotAfter.Add(-renewServer)
	b, err := sc.get(nil)
	if err != nil || b == a || !b.Leaf.NotAfter.After(a.Leaf.NotAfter) {
		t.Fatalf("not renewed at 30 days left: %v", err)
	}
}

func TestHostsConfig(t *testing.T) {
	for name, h := range map[string]HostsConfig{
		"no ca_dir":       {Listen: ":8486", ServerNames: []string{"x"}},
		"no server_names": {Listen: ":8486", CADir: "/ca"},
	} {
		if h.validate() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if (HostsConfig{}).validate() != nil || (HostsConfig{Listen: ":8486", CADir: "/ca", ServerNames: []string{"x"}}).validate() != nil {
		t.Error("a good or an absent hosts section refused")
	}
}

// A worker link's contact, recorded through recordContact, keeps it out of the not_connected alert; with
// no contact recorded the same enrolled identity alerts (the 2026-11-19 false alert on every start).
func TestWorkerLinkContactClearsNotConnected(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "flotilla.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	id := "w-bugfix-node"
	if _, err := st.AddEnrolment(ctx, id, "tok", "test", "test"); err != nil {
		t.Fatal(err)
	}
	if err := st.Enrol(ctx, id, "tok", func() (string, time.Time, error) { return "cert", time.Now().Add(90 * 24 * time.Hour), nil }); err != nil {
		t.Fatal(err)
	}
	check := func(now time.Time) []hostalert.Alert {
		hs, err := st.Hosts(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return hostalert.Check(hs, now, hostalert.Defaults)
	}
	if a := check(time.Now().Add(time.Hour)); len(a) != 1 || a[0].Kind != hostalert.NotConnected {
		t.Fatalf("no contact recorded: %+v", a)
	}
	at := time.Now().Add(time.Hour)
	recordContact(st, slog.New(slog.NewTextHandler(io.Discard, nil)))(hostlink.HostState{Host: id, Connected: true, Version: "v1", ConnectedAt: at, LastContact: at})
	if a := check(at.Add(time.Minute)); len(a) != 0 {
		t.Fatalf("contact recorded: %+v", a)
	}
}

// A host marked intermittent in flotillad.yaml is not logged at ERROR (GlitchTip's "host alert", ~20 a day from the laptop's links) for
// not_connected; an unmarked one still is.
func TestWatchHostsSkipsIntermittentNotConnected(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "flotilla.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, id := range []string{"w-bugfix-omarchy", "w-ops-server"} {
		if _, err := st.AddEnrolment(ctx, id, "tok-"+id, "test", "test"); err != nil {
			t.Fatal(err)
		}
		if err := st.Enrol(ctx, id, "tok-"+id, func() (string, time.Time, error) { return "cert", time.Now().Add(90 * 24 * time.Hour), nil }); err != nil {
			t.Fatal(err)
		}
	}
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	watchHosts(ctx, st, &hostalert.Watcher{}, time.Now().Add(time.Hour), map[string]bool{"w-bugfix-omarchy": true}, log)
	out := buf.String()
	if !strings.Contains(out, "host=w-ops-server kind=not_connected") || strings.Contains(out, "w-bugfix-omarchy") {
		t.Fatalf("alerts logged:\n%s", out)
	}
}
