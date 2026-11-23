// Package directory reads who people are (RFC-0014 §4.6): one reader per source kind, the same sync for each. The `file`
// source is a people file in the installation's host config, for an installation with no company directory: mode 600, each
// person's email, name and id in each tool. Who someone is, never what they may do.
package directory

import (
	"bytes"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/chipi/flotilla/internal/store"
	"gopkg.in/yaml.v3"
)

// Actor is the ledger actor of a sync from a source kind.
func Actor(kind string) string { return "directory:" + kind }

var toolRe = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

type file struct {
	People []struct {
		Email string            `yaml:"email"`
		Name  string            `yaml:"name"`
		IDs   map[string]string `yaml:"ids"`
	} `yaml:"people"`
}

// ReadFile reads a people file. It refuses a file readable by anyone but its owner, an unknown field, a person without an
// email or a name, an email twice, a tool name that is not a short lowercase word, and an id that two people share: an id
// names one person or none.
func ReadFile(path string) ([]store.Person, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s is mode %o: a people file is readable by its owner only (600)", path, fi.Mode().Perm())
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	var f file
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	var out []store.Person
	emails := map[string]bool{}
	owner := map[string]string{} // tool + " " + id to email
	for i, p := range f.People {
		email := strings.ToLower(strings.TrimSpace(p.Email))
		name := strings.TrimSpace(p.Name)
		if !strings.Contains(email, "@") || name == "" {
			return nil, fmt.Errorf("%s: person %d needs an email and a name", path, i+1)
		}
		if emails[email] {
			return nil, fmt.Errorf("%s: %s is listed twice", path, email)
		}
		emails[email] = true
		ids := map[string]string{}
		for tool, id := range p.IDs {
			id = strings.TrimSpace(id)
			if !toolRe.MatchString(tool) || id == "" {
				return nil, fmt.Errorf("%s: %s: ids %q: %q: a tool is a short lowercase word, its id not empty", path, email, tool, id)
			}
			key := tool + " " + id
			if other, ok := owner[key]; ok {
				return nil, fmt.Errorf("%s: the %s id %s is both %s's and %s's", path, tool, id, other, email)
			}
			owner[key] = email
			ids[tool] = id
		}
		out = append(out, store.Person{Email: email, Name: name, IDs: ids})
	}
	return out, nil
}

// Missing is the ids of tool in want that no person has, in want's order: `flotillad -check` warns on a pinned Slack user
// the people file does not name (RFC-0014 §4.6).
func Missing(people []store.Person, tool string, want []string) []string {
	have := map[string]bool{}
	for _, p := range people {
		if id, ok := p.IDs[tool]; ok {
			have[id] = true
		}
	}
	var out []string
	for _, id := range want {
		if !have[id] {
			out = append(out, id)
		}
	}
	return out
}
