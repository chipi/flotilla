package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/chipi/flotilla/internal/hostca"
)

// caCmd makes the host agents' CA on the operator's machine (ADR-0006 §11, decided 2):
//
//	flotilla ca root DIR                     root.crt, root.key: the root stays with the operator, offline
//	flotilla ca intermediate ROOTDIR OUTDIR  intermediate.crt, intermediate.key and a copy of root.crt,
//	                                         for flotillad's secrets volume
//	flotilla ca signing-key DIR ID           signing.key: the execution-signing key for flotillad's secrets volume;
//	                                         prints its public key for each host's signing_keys
//
// Nothing is ever overwritten, and keys are written mode 600.
func caCmd(args []string, now time.Time) error {
	switch {
	case len(args) == 2 && args[0] == "root":
		root, err := hostca.NewRoot(now)
		if err != nil {
			return err
		}
		if err := writeCA(args[1], "root", root); err != nil {
			return err
		}
		fmt.Printf("root CA in %s, valid until %s. Keep root.key offline, as the backup key is.\n",
			args[1], root.Cert.NotAfter.Format(time.DateOnly))
		return nil
	case len(args) == 3 && args[0] == "intermediate":
		certPEM, err := os.ReadFile(filepath.Join(args[1], "root.crt"))
		if err != nil {
			return err
		}
		keyPEM, err := os.ReadFile(filepath.Join(args[1], "root.key"))
		if err != nil {
			return err
		}
		root, err := hostca.Load(certPEM, keyPEM)
		if err != nil {
			return fmt.Errorf("the root: %w", err)
		}
		inter, err := hostca.NewIntermediate(root, now)
		if err != nil {
			return err
		}
		if err := writeCA(args[2], "intermediate", inter); err != nil {
			return err
		}
		if err := writeNew(filepath.Join(args[2], "root.crt"), certPEM, 0o644); err != nil {
			return err
		}
		fmt.Printf("intermediate CA in %s, valid until %s; root.crt beside it. These three files go to flotillad's secrets volume.\n",
			args[2], inter.Cert.NotAfter.Format(time.DateOnly))
		return nil
	case len(args) == 3 && args[0] == "signing-key":
		if !keyIDRe.MatchString(args[2]) {
			return fmt.Errorf("a key id is a-z 0-9 -, up to 32: %q", args[2])
		}
		if err := os.MkdirAll(args[1], 0o700); err != nil {
			return err
		}
		pub, key, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return err
		}
		pemKey, err := hostca.KeyPEM(key)
		if err != nil {
			return err
		}
		if err := writeNew(filepath.Join(args[1], "signing.key"), pemKey, 0o600); err != nil {
			return err
		}
		fmt.Printf("signing key %s in %s/signing.key: it goes to flotillad's secrets volume (hosts.signing_key_file, signing_key_id: %s).\n",
			args[2], args[1], args[2])
		fmt.Printf("each host's config, signing_keys:\n  %q: %q\n", args[2], base64.StdEncoding.EncodeToString(pub))
		return nil
	}
	return errors.New("usage: flotilla ca root DIR | flotilla ca intermediate ROOTDIR OUTDIR | flotilla ca signing-key DIR ID")
}

var keyIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

func writeCA(dir, name string, ca *hostca.CA) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	key, err := hostca.KeyPEM(ca.Key)
	if err != nil {
		return err
	}
	// the key first: a failure leaves no certificate without its key
	if err := writeNew(filepath.Join(dir, name+".key"), key, 0o600); err != nil {
		return err
	}
	return writeNew(filepath.Join(dir, name+".crt"), hostca.CertPEM(ca.Cert), 0o644)
}

// writeNew writes a file that must not exist yet.
func writeNew(path string, b []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
