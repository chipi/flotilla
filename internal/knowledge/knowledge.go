// Package knowledge is an installation's external knowledge (RFC-0002 §7, ADR-0004): the reference
// material flotilla reads and does not own. flotillad.yaml's `knowledge:` section declares each source;
// a routine's spec names the sources and queries it may read; flotillad validates both at load and hands
// each run only what its spec declares, as JSON in FLOTILLA_KNOWLEDGE. runtime/knowledge.py reads an
// entry from the knowledge volume by an exact key of a path map, and records it.
//
// One kind exists, `files`: the deploy copies the declared paths from a host checkout into a Docker
// volume (deploy/mini/knowledge.sh), so a run reads a version that is fixed until the next deploy.
package knowledge

import (
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
)

// Source is one declared source.
type Source struct {
	Kind   string `yaml:"kind" json:"kind"`
	Volume string `yaml:"volume" json:"-"`
	// Paths the deploy copies from the host checkout: files or folders (a folder holds everything under
	// it), relative, with no glob.
	Paths []string `yaml:"paths" json:"-"`
	// Maps are the queries: query name -> key -> path inside the source. A key must match exactly.
	Maps map[string]map[string]string `yaml:"maps" json:"maps"`
}

// Config is the installation's knowledge section: source name -> source.
type Config map[string]Source

var (
	NameRe   = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	QueryRe  = regexp.MustCompile(`^[a-z][a-z0-9_]{0,39}$`)
	keyRe    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)
	volumeRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)
	pathRe   = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)
)

// cleanPath says why p is not a relative path that stays inside the source, or "".
func cleanPath(p string) string {
	switch {
	case p == "" || !pathRe.MatchString(p):
		return "letters, digits and . _ - / only (no glob)"
	case path.IsAbs(p) || path.Clean(p) != p || p == "." || p == ".." || strings.HasPrefix(p, "../"):
		return "relative and clean, with no . or .. part"
	}
	return ""
}

func under(p string, roots []string) bool {
	for _, r := range roots {
		if p == r || strings.HasPrefix(p, r+"/") {
			return true
		}
	}
	return false
}

// Validate checks the section; all problems are reported together.
func (c Config) Validate() error {
	var errs []string
	for name, s := range c {
		bad := func(f string, a ...any) { errs = append(errs, fmt.Sprintf("source %s: ", name)+fmt.Sprintf(f, a...)) }
		if !NameRe.MatchString(name) {
			bad("name: lower-case letters, digits and dashes")
		}
		if s.Kind != "files" {
			bad("kind %q: files is the only kind", s.Kind)
		}
		if !volumeRe.MatchString(s.Volume) {
			bad("volume %q: a Docker volume name", s.Volume)
		}
		if len(s.Paths) == 0 {
			bad("paths: at least one path the deploy copies")
		}
		for _, p := range s.Paths {
			if why := cleanPath(p); why != "" {
				bad("paths: %q: %s", p, why)
			}
		}
		if len(s.Maps) == 0 {
			bad("maps: at least one query")
		}
		for q, m := range s.Maps {
			if !QueryRe.MatchString(q) {
				bad("maps: query %q: lower-case letters, digits and _", q)
			}
			if len(m) == 0 {
				bad("maps.%s: at least one key", q)
			}
			for k, p := range m {
				switch {
				case !keyRe.MatchString(k):
					bad("maps.%s: key %q: letters, digits and . _ -, at most 64", q, k)
				case cleanPath(p) != "":
					bad("maps.%s.%s: %q: %s", q, k, p, cleanPath(p))
				case !under(p, s.Paths):
					bad("maps.%s.%s: %q is not under a declared path, so the deploy would not copy it", q, k, p)
				}
			}
		}
	}
	if len(errs) == 0 {
		return nil
	}
	sort.Strings(errs)
	return errors.New("knowledge: " + fmt.Sprint(errs))
}

// Use is what a routine's spec says it may read.
type Use struct {
	Sources []string `yaml:"sources"`
	Queries []string `yaml:"queries"`
}

// Empty reports a routine that reads no knowledge.
func (u Use) Empty() bool { return len(u.Sources) == 0 && len(u.Queries) == 0 }

// Subset is what a run of a routine gets: the declared sources, each with only the declared queries it
// has. A source or a query the installation does not declare is an error, so a spec cannot read what its
// installation never put there.
func (c Config) Subset(u Use) (Config, error) {
	out := Config{}
	var errs []string
	for _, n := range u.Sources {
		s, ok := c[n]
		if !ok {
			errs = append(errs, fmt.Sprintf("no source %q in flotillad.yaml knowledge", n))
			continue
		}
		maps := map[string]map[string]string{}
		for _, q := range u.Queries {
			if m, ok := s.Maps[q]; ok {
				maps[q] = m
			}
		}
		if len(maps) == 0 {
			errs = append(errs, fmt.Sprintf("source %q has none of the queries %v", n, u.Queries))
			continue
		}
		out[n] = Source{Kind: s.Kind, Maps: maps}
	}
	for _, q := range u.Queries {
		found := false
		for _, s := range out {
			if _, ok := s.Maps[q]; ok {
				found = true
			}
		}
		if len(errs) == 0 && !found {
			errs = append(errs, fmt.Sprintf("query %q is in none of the sources %v", q, u.Sources))
		}
	}
	if len(errs) > 0 {
		return nil, errors.New("knowledge: " + strings.Join(errs, "; "))
	}
	return out, nil
}

// JSON is what a run gets in FLOTILLA_KNOWLEDGE: {"sources": {name: {kind, maps}}}. The volume and the
// paths are the deploy's and the worker's business, never the run's.
func (c Config) JSON() (string, error) {
	b, err := json.Marshal(map[string]any{"sources": c})
	return string(b), err
}
