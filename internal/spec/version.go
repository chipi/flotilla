package spec

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Routine versions (docs/wip/v2-design.md section 1). A routine is a folder, each version a folder in
// it:
//
//	fleets/<fleet>/<routine>/routine.yaml     the routine: identity, shared by every version
//	fleets/<fleet>/<routine>/v<n>/spec.yaml   a version, with the texts it declares beside it
//
// A flat fleets/<fleet>/<routine>.yaml is the same routine with one version, v1 ("legacy"): its fields
// are the routine's and the version's together, and what a version owns is hashed from them in place.
// Which version is live is the store's business (internal/versions); this package loads, validates and
// hashes.

const (
	RoutineFile = "routine.yaml"
	VersionFile = "spec.yaml"
)

var versionDirRe = regexp.MustCompile(`^v([1-9][0-9]*)$`)

// body is what a version owns: its run, its explanations and its model-driven step with the texts
// and skills that step reads. The routine's own fields never move with the version.
type body struct {
	Run       Run
	Explain   map[string]string
	Steps     []Step
	Skills    Skills
	Harness   *Harness
	Prompts   map[string]string
	Models    []string
	PromptSHA map[string]string
	Limit     time.Duration
}

// Version is one version of a routine. Hash is its frozen identity: the sha256 of its manifest, which
// maps each file (folder layout: every file under v<n>/; legacy: each version-owned field and each
// prompt and skill file read in place) to its sha256, so a freeze failure can name what changed.
type Version struct {
	Number      int
	Legacy      bool
	Path        string // the version's spec.yaml; the flat file for a legacy v1
	Dir         string // the version's folder; "" for a legacy v1
	Hash        string
	Manifest    map[string]string
	RoutineHash string // the Routine.Hash of the routine bound to this version
	Err         error  // why it does not load; the other fields are then incomplete
	body
}

// Valid is whether the version loaded and validated.
func (v *Version) Valid() bool { return v.Err == nil }

func shaHex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// manifestHash is the version's hash: over the sorted (path, sha256) pairs of its manifest.
func manifestHash(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	for _, k := range keys {
		fmt.Fprintf(h, "%s\x00%s\n", k, m[k])
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ChangedFiles lists what differs between two manifests, sorted: a path in one only, or with another sha.
func ChangedFiles(recorded, found map[string]string) []string {
	var out []string
	for k, v := range recorded {
		if f, ok := found[k]; !ok {
			out = append(out, k+" (missing)")
		} else if f != v {
			out = append(out, k+" (changed)")
		}
	}
	for k := range found {
		if _, ok := recorded[k]; !ok {
			out = append(out, k+" (added)")
		}
	}
	sort.Strings(out)
	return out
}

func (j *Routine) bodyOf() body {
	return body{Run: j.Run, Explain: j.Explain, Steps: j.Steps, Skills: j.Skills, Harness: j.Harness,
		Prompts: j.Prompts, Models: j.Models, PromptSHA: j.PromptSHA, Limit: j.Limit}
}

func (j *Routine) applyVersion(v *Version) {
	j.Run, j.Explain, j.Steps, j.Skills, j.Harness = v.Run, v.Explain, v.Steps, v.Skills, v.Harness
	j.Prompts, j.Models, j.PromptSHA, j.Limit = v.Prompts, v.Models, v.PromptSHA, v.Limit
	j.Hash, j.Version, j.VersionHash = v.RoutineHash, v.Number, v.Hash
}

// WithVersion is a copy of the routine bound to version n: its run, steps, prompts and the rest are that
// version's, the routine's own fields are unchanged. A run keeps the copy it started with.
func (j *Routine) WithVersion(n int) (*Routine, error) {
	for _, v := range j.Versions {
		if v.Number != n {
			continue
		}
		if v.Err != nil {
			return nil, fmt.Errorf("%s v%d does not load: %w", j.ID(), n, v.Err)
		}
		c := *j
		c.applyVersion(v)
		return &c, nil
	}
	return nil, fmt.Errorf("%s has no version v%d", j.ID(), n)
}

// VersionNamed is the loaded version n, or nil.
func (j *Routine) VersionNamed(n int) *Version {
	for _, v := range j.Versions {
		if v.Number == n {
			return v
		}
	}
	return nil
}

// legacyOwned are the top-level keys of a flat spec that belong to its v1 (what a version's spec.yaml holds).
var legacyOwned = []string{"run", "explain", "steps", "skills", "harness", "prompts", "models"}

// legacyFields is, per version-owned key a flat spec writes, the sha256 of that key's value re-encoded from
// the spec's own YAML (decoded to plain values and marshalled with sorted keys): independent of the Go
// structs, so a new field, a tag or a default in them never moves a recorded hash. A comment or the layout
// of the YAML does not move it either.
func legacyFields(raw []byte) map[string]string {
	out := map[string]string{}
	var doc yaml.Node
	if yaml.Unmarshal(raw, &doc) != nil || len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return out
	}
	m := doc.Content[0]
	for i := 0; i+1 < len(m.Content); i += 2 {
		key := m.Content[i].Value
		for _, own := range legacyOwned {
			if key != own {
				continue
			}
			var v any
			if m.Content[i+1].Decode(&v) != nil {
				continue
			}
			b, err := yaml.Marshal(v)
			if err != nil {
				continue
			}
			out[key] = shaHex(b)
		}
	}
	return out
}

// legacyVersion makes a flat spec's v1. specsDir is the fleets folder, where the skill layers are; raw is the
// spec's bytes.
func (j *Routine) legacyVersion(specsDir string, raw []byte) {
	m := legacyFields(raw)
	for _, role := range sortedKeys(j.Prompts) {
		m["prompt-file:"+j.Prompts[role]] = j.PromptSHA[role]
	}
	for _, name := range j.SkillNames() {
		m["skill-file:"+name] = skillFileSHA(specsDir, j.Fleet, name)
	}
	v := &Version{Number: 1, Legacy: true, Path: j.Path, Manifest: m, Hash: manifestHash(m), RoutineHash: j.Hash, body: j.bodyOf()}
	j.Versions, j.Version, j.VersionHash = []*Version{v}, 1, v.Hash
}

// skillFileSHA is the sha256 of a skill's SKILL.md in the fleet's layer, else flotilla's own ("" when
// neither has it: internal/skills refuses that).
func skillFileSHA(specsDir, fleet, name string) string {
	for _, dir := range []string{
		filepath.Join(specsDir, fleet, "skills", name),
		filepath.Join(filepath.Dir(filepath.Clean(specsDir)), "skills", name),
	} {
		if b, err := os.ReadFile(filepath.Join(dir, "SKILL.md")); err == nil {
			return shaHex(b)
		}
	}
	return ""
}

// routineFile is routine.yaml: what the routine is, whichever version runs.
type routineFile struct {
	Fleet        string      `yaml:"fleet"`
	Routine      string      `yaml:"routine"`
	Description  string      `yaml:"description"`
	Enabled      bool        `yaml:"enabled"`
	Worker       string      `yaml:"worker"`
	Trigger      Trigger     `yaml:"trigger"`
	Detects      []string    `yaml:"detects"`
	Autonomy     string      `yaml:"autonomy"`
	MaxAutonomy  string      `yaml:"max_autonomy"`
	Budget       Budget      `yaml:"budget"`
	Freshness    string      `yaml:"freshness"`
	Resume       *Resume     `yaml:"resume"`
	BriefSection string      `yaml:"brief_section"`
	Chain        []ChainLink `yaml:"chain"`
	Acceptance   []string    `yaml:"acceptance"`
}

// versionFile is v<n>/spec.yaml: what one version does.
type versionFile struct {
	Run     Run               `yaml:"run"`
	Explain map[string]string `yaml:"explain"`
	Steps   []Step            `yaml:"steps"`
	Skills  Skills            `yaml:"skills"`
	Harness *Harness          `yaml:"harness"`
	Prompts map[string]string `yaml:"prompts"` // each a file inside v<n>/
	Models  []string          `yaml:"models"`
}

func strictDecode(raw []byte, into any) error {
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	return dec.Decode(into)
}

// loadFolder reads a routine folder from its routine.yaml path: the routine and every v<n>/ in it. A
// version that does not load is kept with its Err; a folder that cannot be read as a routine is an error.
func loadFolder(p string) (*Routine, error) {
	raw, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	var rf routineFile
	if err := strictDecode(raw, &rf); err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	dir := filepath.Dir(p)
	if want := filepath.Base(filepath.Dir(dir)); rf.Fleet != want {
		return nil, fmt.Errorf("%s: fleet %q, but the folder is under %s/", p, rf.Fleet, want)
	}
	if want := filepath.Base(dir); rf.Routine != want {
		return nil, fmt.Errorf("%s: routine %q, but the folder is %s/", p, rf.Routine, want)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), rf.Routine+".yaml")); err == nil {
		return nil, fmt.Errorf("%s: %s is also a flat spec (%s.yaml): one layout per routine", p, rf.Fleet+"/"+rf.Routine, rf.Routine)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var nums []int
	var errs []error
	for _, e := range entries {
		name := e.Name()
		if name == RoutineFile || strings.HasPrefix(name, ".") {
			continue
		}
		if m := versionDirRe.FindStringSubmatch(name); m != nil && e.IsDir() {
			n, err := strconv.Atoi(m[1])
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %s is not a version number (too large)", dir, name))
				continue
			}
			nums = append(nums, n)
			continue
		}
		errs = append(errs, fmt.Errorf("%s: %s is neither routine.yaml nor a version folder (v1, v2, ...)", dir, name))
	}
	if len(errs) == 0 && len(nums) == 0 {
		errs = append(errs, fmt.Errorf("%s: no version folder (v1/)", dir))
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	sort.Ints(nums)

	base := &Routine{Fleet: rf.Fleet, Routine: rf.Routine, Description: rf.Description, Enabled: rf.Enabled, Worker: rf.Worker,
		Trigger: rf.Trigger, Detects: rf.Detects, Autonomy: rf.Autonomy, MaxAutonomy: rf.MaxAutonomy, Budget: rf.Budget,
		Freshness: rf.Freshness, Resume: rf.Resume, BriefSection: rf.BriefSection, Chain: rf.Chain, Acceptance: rf.Acceptance, Path: p}
	base.normalizeLevels()
	// the routine's own fields are checked once, here: Every and Fresh are the same for every version, broken or retired ones included
	if errs := base.validateRoutineErrs(); len(errs) > 0 {
		return nil, fmt.Errorf("%s: %w", p, errors.Join(errs...))
	}
	for _, n := range nums {
		v, _ := loadVersionDir(base, raw, filepath.Join(dir, "v"+strconv.Itoa(n)), n)
		base.Versions = append(base.Versions, v)
	}
	base.Hash = shaHex(raw)[:12]
	for _, v := range base.Versions {
		if v.Err == nil {
			base.applyVersion(v)
			break
		}
	}
	return base, nil
}

// loadVersionDir loads one version folder into a Version and returns the routine as that version
// would run (validated: its Every, Fresh and Limit are set).
func loadVersionDir(base *Routine, routineRaw []byte, vdir string, n int) (*Version, *Routine) {
	v := &Version{Number: n, Dir: vdir, Path: filepath.Join(vdir, VersionFile)}
	a := *base
	fail := func(errs ...error) (*Version, *Routine) {
		v.Err = fmt.Errorf("%s: %w", v.Path, errors.Join(errs...))
		return v, &a
	}
	raw, err := os.ReadFile(v.Path)
	if err != nil {
		return fail(err)
	}
	var vf versionFile
	if err := strictDecode(raw, &vf); err != nil {
		return fail(err)
	}
	if v.Manifest, err = manifestOf(vdir); err != nil {
		return fail(err)
	}
	v.Hash = manifestHash(v.Manifest)
	a.Run, a.Explain, a.Steps, a.Skills, a.Harness, a.Models = vf.Run, vf.Explain, vf.Steps, vf.Skills, vf.Harness, vf.Models
	// A prompt path is written relative to v<n>/ and kept relative to the fleet's folder, where
	// internal/prompts and the run resolve it; one that is not a clean path stays as written and fails validate.
	rel := path0(base.Routine, n)
	if len(vf.Prompts) > 0 {
		a.Prompts, a.PromptSHA = map[string]string{}, map[string]string{}
		for role, p := range vf.Prompts {
			a.Prompts[role] = p
			if validPromptPath(p) {
				a.Prompts[role] = rel + p
				if b, err := os.ReadFile(filepath.Join(vdir, filepath.FromSlash(p))); err == nil {
					a.PromptSHA[role] = shaHex(b)
				}
			}
		}
	}
	if errs := a.validateErrs(); len(errs) > 0 {
		return fail(errs...)
	}
	v.RoutineHash = shaHex(append(append([]byte{}, routineRaw...), fmt.Sprintf("\nversion %d=%s", n, v.Hash)...))[:12]
	v.body = a.bodyOf()
	return v, &a
}

func path0(routine string, n int) string { return routine + "/v" + strconv.Itoa(n) + "/" }

// manifestOf maps every file under dir (slash paths relative to it) to its sha256. A symlink or any
// file that is not regular is refused: a version's bytes are what is in its folder, nothing outside it.
func manifestOf(dir string) (map[string]string, error) {
	m := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		if !d.Type().IsRegular() {
			return fmt.Errorf("%s is not a regular file (a version folder holds no symlinks)", filepath.ToSlash(rel))
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		m[filepath.ToSlash(rel)] = shaHex(b)
		return nil
	})
	return m, err
}
