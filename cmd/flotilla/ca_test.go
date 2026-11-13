package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/chipi/flotilla/internal/hostca"
)

// The operator's two commands: a root, then an intermediate that issues a host certificate verifying
// up to that root. Keys are mode 600 and nothing is overwritten.
func TestCA(t *testing.T) {
	now := time.Now()
	dir := t.TempDir()
	rootDir, outDir := filepath.Join(dir, "root"), filepath.Join(dir, "flotillad")
	if err := caCmd([]string{"root", rootDir}, now); err != nil {
		t.Fatal(err)
	}
	if err := caCmd([]string{"root", rootDir}, now); err == nil {
		t.Fatal("a second root overwrote the first")
	}
	if err := caCmd([]string{"intermediate", rootDir, outDir}, now); err != nil {
		t.Fatal(err)
	}
	if err := caCmd([]string{"intermediate", rootDir, outDir}, now); err == nil {
		t.Fatal("a second intermediate overwrote the first")
	}
	for _, f := range []string{filepath.Join(rootDir, "root.key"), filepath.Join(outDir, "intermediate.key")} {
		if st, err := os.Stat(f); err != nil || st.Mode().Perm() != 0o600 {
			t.Errorf("%s: %v %v", f, st.Mode(), err)
		}
	}
	if _, err := os.Stat(filepath.Join(outDir, "root.key")); err == nil {
		t.Error("the root's key reached flotillad's files")
	}
	read := func(p string) []byte {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	inter, err := hostca.Load(read(filepath.Join(outDir, "intermediate.crt")), read(filepath.Join(outDir, "intermediate.key")))
	if err != nil {
		t.Fatal(err)
	}
	root, err := hostca.ParseCert(read(filepath.Join(outDir, "root.crt")))
	if err != nil {
		t.Fatal(err)
	}
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	csr, _ := hostca.HostRequest(key, "mini")
	host, err := inter.IssueHost(csr, "mini", now)
	if err != nil {
		t.Fatal(err)
	}
	if name, _, err := hostca.VerifyHost([]*x509.Certificate{host, inter.Cert}, root, now); err != nil || name != "mini" {
		t.Fatalf("verify: %q %v", name, err)
	}
	if err := caCmd([]string{"intermediate", filepath.Join(dir, "nowhere"), filepath.Join(dir, "x")}, now); err == nil {
		t.Error("an intermediate without a root")
	}
	if err := caCmd([]string{"bogus"}, now); err == nil {
		t.Error("a bad command")
	}
}
