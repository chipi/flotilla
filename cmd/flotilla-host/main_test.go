package main

import (
	"os"
	"testing"
)

func TestKeyAndChainFiles(t *testing.T) {
	dir := t.TempDir()
	if _, err := loadKey(dir, false); err == nil {
		t.Fatal("run made a key: only enrol may")
	}
	k1, err := loadKey(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(keyPath(dir)); st.Mode().Perm() != 0o600 {
		t.Fatalf("the key is %v", st.Mode().Perm())
	}
	k2, err := loadKey(dir, true) // enrolling again keeps the key
	if err != nil || !k1.Equal(k2) {
		t.Fatalf("a second enrol replaced the key: %v", err)
	}
	if err := saveChain(dir, []byte("one")); err != nil {
		t.Fatal(err)
	}
	if err := saveChain(dir, []byte("two")); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(chainPath(dir))
	if string(b) != "two" {
		t.Fatalf("the chain file: %q", b)
	}
	if _, err := os.Stat(chainPath(dir) + ".new"); err == nil {
		t.Fatal("a temp file left behind")
	}
	if (opts{addr: "x:1"}).need() == nil || (opts{serverName: "x"}).need() == nil {
		t.Fatal("ran without -addr or -server-name")
	}
}
