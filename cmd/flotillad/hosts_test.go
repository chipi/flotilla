package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/chipi/flotilla/internal/hostca"
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
