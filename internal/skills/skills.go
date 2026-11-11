// Package skills checks, before flotillad starts or a deploy goes ahead, that every skill a routine's
// spec names can be loaded (ADR-0012, RFC-0005 §6): in exactly one layer, a SKILL.md that is not a
// symlink, valid frontmatter, instructions under the size cap. runtime/skills.py applies the same rules
// when a run loads it.
//
// Layers, each inside the images and pinned by the image tag:
//
//	flotilla  <specs dir>/../skills/<name>/SKILL.md      flotilla's default skills
//	fleet     <specs dir>/<fleet>/skills/<name>/SKILL.md the fleet's own skills
package skills

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/chipi/flotilla/internal/spec"
)

const (
	MaxBytes       = 16384
	MaxDescription = 1024
)

// Check validates every skill every routine names; all problems are reported together.
func Check(specsDir string, routines []*spec.Routine) error {
	var errs []error
	for _, r := range routines {
		for _, name := range r.SkillNames() {
			if err := checkOne(specsDir, r.Fleet, name); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", r.ID(), err))
			}
		}
	}
	return errors.Join(errs...)
}

func checkOne(specsDir, fleet, name string) error {
	layers := []struct{ name, dir string }{
		{"flotilla", filepath.Join(filepath.Dir(filepath.Clean(specsDir)), "skills", name)},
		{"fleet", filepath.Join(specsDir, fleet, "skills", name)},
	}
	var found []string
	var path string
	for _, l := range layers {
		p := filepath.Join(l.dir, "SKILL.md")
		for _, x := range []string{l.dir, p} {
			if fi, err := os.Lstat(x); err == nil && fi.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("skill %s: %s layer has a symlink (%s); refused", name, l.name, x)
			}
		}
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
			found, path = append(found, l.name), p
		}
	}
	switch len(found) {
	case 0:
		return fmt.Errorf("skill %s: in no layer (fleet %s)", name, fleet)
	case 1:
	default:
		return fmt.Errorf("skill %s: in more than one layer (%s)", name, strings.Join(found, ", "))
	}
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("skill %s: %w", name, err)
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, MaxBytes+1))
	if err != nil {
		return fmt.Errorf("skill %s: %w", name, err)
	}
	if len(raw) > MaxBytes {
		return fmt.Errorf("skill %s: SKILL.md is over %d bytes (a skill is never trimmed)", name, MaxBytes)
	}
	return Parse(raw, name)
}

// Parse checks a SKILL.md: frontmatter with name equal to the folder's name and a description of 1 to
// 1024 characters, then instructions. Other frontmatter fields are ignored and grant nothing.
func Parse(raw []byte, name string) error {
	if !bytes.HasPrefix(raw, []byte("---\n")) {
		return fmt.Errorf("skill %s: SKILL.md must start with a --- frontmatter line", name)
	}
	end := bytes.Index(raw[3:], []byte("\n---\n"))
	if end < 0 {
		return fmt.Errorf("skill %s: frontmatter is not closed by a --- line", name)
	}
	end += 3
	fields := map[string]string{}
	for _, line := range strings.Split(string(raw[4:end]), "\n") {
		k, v, ok := strings.Cut(line, ":")
		if k = strings.TrimSpace(k); ok && (k == "name" || k == "description") {
			fields[k] = strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	if fields["name"] != name {
		return fmt.Errorf("skill %s: frontmatter name is %q, not the folder's name", name, fields["name"])
	}
	if n := len([]rune(fields["description"])); n < 1 || n > MaxDescription {
		return fmt.Errorf("skill %s: description must be 1 to %d characters", name, MaxDescription)
	}
	if strings.TrimSpace(string(raw[end+5:])) == "" {
		return fmt.Errorf("skill %s: no instructions after the frontmatter", name)
	}
	return nil
}
