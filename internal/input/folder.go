package input

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// An export holds production data (case contents, ledger rows, a routine's state), so it goes to a folder the operator names and
// never into a git work tree: one `git add .` away from a public repository is the failure this prevents.

// Names of the files an export writes.
const (
	EnvelopeFile = "input.json"
	StateFile    = "state.tar.gz"
	ReadmeFile   = "README.txt"
)

// ErrInsideRepo: the folder is inside a git work tree.
var ErrInsideRepo = errors.New("the export folder is inside a git work tree: it holds production data and must stay outside any repository")

// nearestExisting resolves symlinks of the deepest part of dir that exists, so a link into a repository is seen for what it is.
func nearestExisting(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	rest := ""
	for p := abs; ; p = filepath.Dir(p) {
		if real, err := filepath.EvalSymlinks(p); err == nil {
			return filepath.Join(real, rest), nil
		}
		if filepath.Dir(p) == p {
			return abs, nil
		}
		rest = filepath.Join(filepath.Base(p), rest)
	}
}

// CheckFolder returns the folder's resolved absolute path, or says why an export must not go there: inside a git work tree (any
// folder on the way up holds a .git, a directory or a worktree's file), or already holding something.
func CheckFolder(dir string) (string, error) {
	if dir == "" {
		return "", errors.New("an export needs a folder: --to DIR")
	}
	real, err := nearestExisting(dir)
	if err != nil {
		return "", err
	}
	for p := real; ; p = filepath.Dir(p) {
		if _, err := os.Lstat(filepath.Join(p, ".git")); err == nil {
			return "", fmt.Errorf("%w (%s)", ErrInsideRepo, p)
		}
		if filepath.Dir(p) == p {
			break
		}
	}
	if entries, err := os.ReadDir(real); err == nil && len(entries) > 0 {
		return "", fmt.Errorf("the export folder %s is not empty: name a new one", real)
	}
	return real, nil
}

// WriteFolder writes an export: the envelope (input.json), the state archive when there is one (state.tar.gz, named in the
// envelope's state_snapshot.tar), and a README that says what the folder is. The folder is mode 0700 and every file 0600. The
// envelope is checked first; nothing is written for an invalid one.
func WriteFolder(dir string, e Envelope, state io.Reader, now time.Time) (err error) {
	real, err := CheckFolder(dir)
	if err != nil {
		return err
	}
	if state != nil {
		if e.StateSnapshot == nil {
			return errors.New("a state archive without state_snapshot.paths in the envelope")
		}
		snap := *e.StateSnapshot
		snap.Tar = StateFile
		e.StateSnapshot = &snap
	}
	if err := e.Validate(); err != nil {
		return fmt.Errorf("the input is not valid: %w", err)
	}
	_, statErr := os.Stat(real)
	created := os.IsNotExist(statErr)
	if err := os.MkdirAll(real, 0o700); err != nil {
		return err
	}
	// a failed export leaves nothing of production data behind: the files it wrote, and the folder when it made it
	defer func() {
		if err == nil {
			return
		}
		for _, f := range []string{EnvelopeFile, StateFile, ReadmeFile} {
			os.Remove(filepath.Join(real, f))
		}
		if created {
			os.Remove(real)
		}
	}()
	if err := os.Chmod(real, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return err
	}
	if err := writeNew(filepath.Join(real, EnvelopeFile), append(b, '\n')); err != nil {
		return err
	}
	if state != nil {
		f, err := os.OpenFile(filepath.Join(real, StateFile), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		if _, err := io.Copy(f, state); err != nil {
			f.Close()
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
	}
	readme := fmt.Sprintf("This folder is PRODUCTION DATA exported from flotilla on %s: the frozen input %s of %s (kind %s).\n"+
		"It is the envelope of runtime/input.schema.json (input.json)%s. Keep it outside any repository and scrub it before it joins any dataset.\n",
		now.UTC().Format(time.RFC3339), e.InputKey, e.Routine, e.Kind, map[bool]string{true: " and the state snapshot beside it (state.tar.gz)", false: ""}[state != nil])
	return writeNew(filepath.Join(real, ReadmeFile), []byte(readme))
}

func writeNew(path string, b []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// WriteJSON writes one JSON document (a pairs export) to a new folder under the same rules as an input's: never inside a git work tree, never
// a folder that holds something, 0700 and 0600, a README that says it is production data; a failure leaves nothing behind.
func WriteJSON(dir, name string, raw []byte, what string, now time.Time) (err error) {
	real, err := CheckFolder(dir)
	if err != nil {
		return err
	}
	if !json.Valid(raw) {
		return errors.New("not JSON")
	}
	_, statErr := os.Stat(real)
	created := os.IsNotExist(statErr)
	if err := os.MkdirAll(real, 0o700); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			os.Remove(filepath.Join(real, name))
			os.Remove(filepath.Join(real, ReadmeFile))
			if created {
				os.Remove(real)
			}
		}
	}()
	if err := os.Chmod(real, 0o700); err != nil {
		return err
	}
	if err := writeNew(filepath.Join(real, name), append(append([]byte(nil), raw...), '\n')); err != nil {
		return err
	}
	readme := fmt.Sprintf("This folder is PRODUCTION DATA exported from flotilla on %s: %s (%s). Keep it outside any repository and scrub it before it joins any dataset.\n",
		now.UTC().Format(time.RFC3339), what, name)
	return writeNew(filepath.Join(real, ReadmeFile), []byte(readme))
}
