// Package prompts checks, before flotillad starts or a deploy goes ahead, that every prompt a routine's
// spec declares can be loaded (ADR-0018): a regular file inside the fleet's folder, no symlink anywhere
// on its path, UTF-8, not empty, at most MaxBytes. runtime/prompts.py applies the same rules, and the
// hash flotillad hands the run, when a run loads it.
package prompts

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/chipi/flotilla/internal/spec"
)

const MaxBytes = 64 * 1024

// Check validates every prompt every routine declares; all problems are reported together.
func Check(specsDir string, routines []*spec.Routine) error {
	var errs []error
	for _, r := range routines {
		for _, role := range r.PromptRoles() {
			if err := checkOne(specsDir, r.Fleet, role, r.Prompts[role]); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", r.ID(), err))
			}
		}
	}
	return errors.Join(errs...)
}

func checkOne(specsDir, fleet, role, rel string) error {
	base := filepath.Join(specsDir, fleet)
	full := filepath.Join(base, filepath.FromSlash(rel))
	if r, err := filepath.Rel(base, full); err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return fmt.Errorf("prompt %s: %s is outside the fleet's folder", role, rel)
	}
	// the fleet's folder, every directory under it on the way, and the file: none may be a symlink
	walk := []string{base}
	cur := base
	for _, part := range strings.Split(filepath.ToSlash(rel), "/") {
		cur = filepath.Join(cur, part)
		walk = append(walk, cur)
	}
	for _, x := range walk {
		fi, err := os.Lstat(x)
		if err != nil {
			return fmt.Errorf("prompt %s: %s: %w", role, rel, err)
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("prompt %s: %s has a symlink (%s); refused", role, rel, x)
		}
	}
	if fi, err := os.Stat(full); err != nil || !fi.Mode().IsRegular() {
		return fmt.Errorf("prompt %s: %s is not a regular file", role, rel)
	}
	f, err := os.Open(full)
	if err != nil {
		return fmt.Errorf("prompt %s: %w", role, err)
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, MaxBytes+1))
	if err != nil {
		return fmt.Errorf("prompt %s: %w", role, err)
	}
	switch {
	case len(raw) > MaxBytes:
		return fmt.Errorf("prompt %s: %s is over %d bytes (a prompt is never trimmed)", role, rel, MaxBytes)
	case !utf8.Valid(raw):
		return fmt.Errorf("prompt %s: %s is not UTF-8", role, rel)
	case strings.TrimSpace(string(raw)) == "":
		return fmt.Errorf("prompt %s: %s is empty", role, rel)
	}
	return nil
}
