package spec

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

// A fleet brings its own knowledge (operator fleet plan, 2026-11-26): fleet.yaml's `knowledge:` names folders
// inside the fleet's own folder that flotilla shows while the fleet is installed, and checks at load. The core
// knows nothing of what an item means (an SOP, a playbook): it reads the front matter's text and the page.
//
//	knowledge:
//	  - name: sops
//	    title: Standard operating procedures
//	    path: sops                       # a folder inside fleets/<fleet>/
//	    format: skill                    # skill: <id>/SKILL.md per folder | markdown: <id>.md per page
//	    fields: [applies_to, fix, rung]  # front-matter keys the UI shows as columns (skill format)
//	    proposals: true                  # the fleet may propose items; a person accepts them on the tab

// Knowledge formats.
const (
	KnowledgeSkill    = "skill"
	KnowledgeMarkdown = "markdown"
)

const (
	maxKnowledgeItem  = 16384 // as a skill: a page is never trimmed
	maxKnowledgeTitle = 200
	maxKnowledgeDesc  = 1024
)

var (
	knowledgeNameRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	knowledgeKeyRe  = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)
	knowledgeIDRe   = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
)

// Knowledge is one declared collection and, once loaded, what is in it.
type Knowledge struct {
	Name      string   `yaml:"name"`
	Title     string   `yaml:"title"`
	Path      string   `yaml:"path"`
	Format    string   `yaml:"format"`
	Fields    []string `yaml:"fields"`
	Proposals bool     `yaml:"proposals"`

	Items []KnowledgeItem `yaml:"-"`
	Hash  string          `yaml:"-"` // sha256 over every file under Path (sorted paths and contents): a changed item is a new fleet version
}

// KnowledgeItem is one page of a collection.
type KnowledgeItem struct {
	ID          string            // the skill's folder name, or the page's file name without .md
	Title       string            // a markdown page's first line; a skill's name
	Description string            // a skill's description
	Fields      map[string]string // the declared front-matter keys, as the text the file holds (a JSON value stays JSON text)
	Body        string            // what follows the front matter; a markdown page whole
	File        string            // the page's file, slash-separated, relative to the fleet's folder
	Hash        string            // sha256 of the page's bytes (a skill's is what its `loaded` record carries)
}

// validate checks the declaration alone (no file is read).
func (k Knowledge) validate() error {
	var errs []error
	bad := func(f string, a ...any) { errs = append(errs, fmt.Errorf(f, a...)) }
	if !knowledgeNameRe.MatchString(k.Name) {
		bad("name %q: lower-case letters, digits and hyphens, starting with a letter", k.Name)
	}
	if strings.TrimSpace(k.Title) == "" || utf8.RuneCountInString(k.Title) > 80 {
		bad("title is required, at most 80 characters")
	}
	if !validKnowledgePath(k.Path) {
		bad("path %q: a folder inside the fleet's own folder (relative, no .., not absolute)", k.Path)
	}
	switch k.Format {
	case KnowledgeSkill:
	case KnowledgeMarkdown:
		if len(k.Fields) > 0 {
			bad("fields: a markdown page has no front matter, so there is nothing to show as a column")
		}
	default:
		bad("format %q: skill or markdown", k.Format)
	}
	seen := map[string]bool{}
	for _, f := range k.Fields {
		if !knowledgeKeyRe.MatchString(f) {
			bad("field %q: a front-matter key (lower-case letters, digits and underscores)", f)
		}
		if seen[f] {
			bad("field %q is listed twice", f)
		}
		seen[f] = true
	}
	return errors.Join(errs...)
}

func validKnowledgePath(p string) bool {
	if p == "" || strings.ContainsAny(p, "\\\x00") || strings.HasPrefix(p, "/") || filepath.IsAbs(p) {
		return false
	}
	c := path.Clean(p)
	return c != "." && c != ".." && !strings.HasPrefix(c, "../") && c == strings.TrimSuffix(p, "/")
}

// loadKnowledge reads and checks every collection a fleet declares, under its folder. All problems are reported together.
func loadKnowledge(fleetDir string, ks []Knowledge) ([]Knowledge, error) {
	var errs []error
	seen := map[string]bool{}
	out := make([]Knowledge, 0, len(ks))
	for i, k := range ks {
		k.Path = strings.TrimSuffix(k.Path, "/")
		wrap := func(err error) { errs = append(errs, fmt.Errorf("knowledge[%d] %q: %w", i, k.Name, err)) }
		if err := k.validate(); err != nil {
			wrap(err)
			continue
		}
		if seen[k.Name] {
			wrap(errors.New("declared twice"))
			continue
		}
		seen[k.Name] = true
		if err := k.read(fleetDir); err != nil {
			wrap(err)
			continue
		}
		out = append(out, k)
	}
	return out, errors.Join(errs...)
}

func (k *Knowledge) read(fleetDir string) error {
	root := filepath.Join(fleetDir, filepath.FromSlash(k.Path))
	// a symlink anywhere on the way would let the path leave the folder
	cur := fleetDir
	for _, part := range strings.Split(k.Path, "/") {
		cur = filepath.Join(cur, part)
		fi, err := os.Lstat(cur)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("path %s: no such folder in the fleet", k.Path)
			}
			return err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("path %s: %s is a symlink; refused", k.Path, part)
		}
		if !fi.IsDir() {
			return fmt.Errorf("path %s: not a folder", k.Path)
		}
	}
	var errs []error
	h := sha256.New()
	werr := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%s is a symlink; refused", p)
		}
		if d.IsDir() {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(fleetDir, p)
		fmt.Fprintf(h, "%s\x00%d\x00", filepath.ToSlash(rel), len(b))
		h.Write(b)
		return nil
	})
	if werr != nil {
		return werr
	}
	k.Hash = hex.EncodeToString(h.Sum(nil))
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, e := range entries {
		var it KnowledgeItem
		var err error
		switch k.Format {
		case KnowledgeSkill:
			if !e.IsDir() {
				continue // a loose file beside the skill folders (a rules table) is not an item
			}
			it, err = readSkillItem(fleetDir, k, e.Name())
		default:
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
				continue
			}
			it, err = readPageItem(fleetDir, k, e.Name())
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		k.Items = append(k.Items, it)
	}
	if len(errs) == 0 && len(k.Items) > 0 {
		for _, f := range k.Fields {
			held := false
			for _, it := range k.Items {
				if _, ok := it.Fields[f]; ok {
					held = true
					break
				}
			}
			if !held {
				errs = append(errs, fmt.Errorf("field %q: no item has it in its front matter", f))
			}
		}
	}
	sort.Slice(k.Items, func(a, b int) bool { return k.Items[a].ID < k.Items[b].ID })
	return errors.Join(errs...)
}

func readCapped(p string) ([]byte, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	if len(b) > maxKnowledgeItem {
		return nil, fmt.Errorf("%d bytes, over %d (a page is never trimmed)", len(b), maxKnowledgeItem)
	}
	return b, nil
}

// readSkillItem is a folder with a SKILL.md as runtime/skills.py and internal/skills read it (name equal to the folder, a
// description, instructions), plus the flat `key: value` front matter a fleet's own items add (an SOP's JSON values stay text).
func readSkillItem(fleetDir string, k *Knowledge, id string) (KnowledgeItem, error) {
	file := path.Join(k.Path, id, "SKILL.md")
	it := KnowledgeItem{ID: id, File: file}
	fail := func(f string, a ...any) (KnowledgeItem, error) {
		return it, fmt.Errorf("%s: %s", file, fmt.Sprintf(f, a...))
	}
	if !knowledgeIDRe.MatchString(id) {
		return fail("the folder name %q is not a skill name", id)
	}
	raw, err := readCapped(filepath.Join(fleetDir, filepath.FromSlash(file)))
	if err != nil {
		return fail("%v", err)
	}
	if !bytes.HasPrefix(raw, []byte("---\n")) {
		return fail("must start with a --- frontmatter line")
	}
	end := bytes.Index(raw[3:], []byte("\n---\n"))
	if end < 0 {
		return fail("frontmatter is not closed by a --- line")
	}
	end += 3
	front := map[string]string{}
	for _, line := range strings.Split(string(raw[4:end]), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		key, val, ok := strings.Cut(line, ":")
		key = strings.TrimSpace(key)
		if !ok || !knowledgeKeyRe.MatchString(key) {
			return fail("frontmatter line %q is not `key: value`", clip(line, 60))
		}
		if _, dup := front[key]; dup {
			return fail("%s is given twice", key)
		}
		front[key] = strings.TrimSpace(val)
	}
	unq := func(s string) string {
		if len(s) >= 2 && s[0] == s[len(s)-1] && (s[0] == '"' || s[0] == '\'') {
			return s[1 : len(s)-1]
		}
		return s
	}
	if got := unq(front["name"]); got != id {
		return fail("frontmatter name is %q, not the folder's name", got)
	}
	desc := unq(front["description"])
	if n := utf8.RuneCountInString(desc); n < 1 || n > maxKnowledgeDesc {
		return fail("description must be 1 to %d characters", maxKnowledgeDesc)
	}
	body := strings.TrimSpace(string(raw[end+5:]))
	if body == "" {
		return fail("no instructions after the frontmatter")
	}
	sum := sha256.Sum256(raw)
	it.Title, it.Description, it.Body, it.Hash = id, desc, body, hex.EncodeToString(sum[:])
	it.Fields = map[string]string{}
	for _, f := range k.Fields {
		if v, ok := front[f]; ok {
			it.Fields[f] = v
		}
	}
	return it, nil
}

// readPageItem is a markdown page: its first line is its title (a leading # is dropped), the page is its body.
func readPageItem(fleetDir string, k *Knowledge, name string) (KnowledgeItem, error) {
	file := path.Join(k.Path, name)
	id := strings.TrimSuffix(name, ".md")
	it := KnowledgeItem{ID: id, File: file}
	fail := func(f string, a ...any) (KnowledgeItem, error) {
		return it, fmt.Errorf("%s: %s", file, fmt.Sprintf(f, a...))
	}
	if !knowledgeIDRe.MatchString(id) {
		return fail("the file name %q is not a page name (lower-case letters, digits, hyphens)", name)
	}
	raw, err := readCapped(filepath.Join(fleetDir, filepath.FromSlash(file)))
	if err != nil {
		return fail("%v", err)
	}
	if !utf8.Valid(raw) {
		return fail("not UTF-8 text")
	}
	text := string(raw)
	first, _, _ := strings.Cut(text, "\n")
	title := strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(first), "#"))
	if title == "" {
		return fail("the first line is the page's title and it is empty")
	}
	if utf8.RuneCountInString(title) > maxKnowledgeTitle {
		return fail("the first line (the title) is over %d characters", maxKnowledgeTitle)
	}
	sum := sha256.Sum256(raw)
	it.Title, it.Body, it.Hash = title, strings.TrimSpace(text), hex.EncodeToString(sum[:])
	return it, nil
}

func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// knowledgePack is what a fleet's knowledge adds to its pack hash: nothing for a fleet that declares none, so
// every existing fleet keeps its hash byte for byte.
func knowledgePack(ks []Knowledge) string {
	if len(ks) == 0 {
		return ""
	}
	parts := make([]string, 0, len(ks))
	for _, k := range ks {
		parts = append(parts, "knowledge "+k.Name+"="+k.Hash)
	}
	sort.Strings(parts)
	return "\n" + strings.Join(parts, "\n")
}
