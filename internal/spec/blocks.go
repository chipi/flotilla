package spec

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Blocks (RFC-0016): a block is a numbered folder, blocks/<name>/v<n>/, with a block.yaml that states its interface
// and the texts its routines carry. A flow's step pins one (use: name@n); the generator (flow.go) turns the step into
// an ordinary routine folder. The block's code stays where the workers carry it (ops/*.py): block.yaml names it.

// BlockFile is a block version's interface.
const BlockFile = "block.yaml"

// Roles of a block, and what each is triggered by.
const (
	RoleMonitor = "monitor" // a schedule; reports detections
	RoleAct     = "act"     // a case kind, stage ""
	RoleStage   = "stage"   // a case kind and a named stage
	RoleVerify  = "verify"  // a registry check name
	RoleLearn   = "learn"   // a schedule; reports proposals
)

var blockRoles = map[string]bool{RoleMonitor: true, RoleAct: true, RoleStage: true, RoleVerify: true, RoleLearn: true}

// emitKinds is what a block's code may report.
var emitKinds = map[string]bool{"detection": true, "plan": true, "hand_over": true, "verified": true}

// Param kinds a block's params declare. Every value is checked against its kind before a flow deploys, and a
// value that reaches a command line is limited to characters a shell passes through unchanged.
const (
	KindRegistryAction = "registry_action"
	KindMetricName     = "metric_name"
	KindLabelList      = "label_list"
	KindLabelMatch     = "label_match"
	KindCount          = "count"
	KindString         = "string"
	KindURL            = "url"
	KindPath           = "path"
	KindRepo           = "repo"
	KindRepoFile       = "repo_file"
	KindFlowList       = "flow_list"
)

var paramKinds = map[string]bool{KindRegistryAction: true, KindMetricName: true, KindLabelList: true, KindLabelMatch: true,
	KindCount: true, KindString: true, KindURL: true, KindPath: true, KindRepo: true, KindRepoFile: true, KindFlowList: true}

var (
	blockNameRe  = regexp.MustCompile(`^[a-z][a-z0-9-]{0,39}$`)
	metricNameRe = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)
	labelNameRe  = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)
	shellSafeRe  = regexp.MustCompile(`^[A-Za-z0-9._/:@%+=,-]{1,200}$`)
	urlParamRe   = regexp.MustCompile(`^https?://[A-Za-z0-9._:/-]{1,200}$`)
	pathParamRe  = regexp.MustCompile(`^/[A-Za-z0-9._/-]{0,199}$`)
	envNameRe    = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)
	argNameRe    = regexp.MustCompile(`^--[a-z][a-z0-9-]{0,39}$`)
	useRe        = regexp.MustCompile(`^([a-z][a-z0-9-]{0,39})@([1-9][0-9]{0,3})$`)
)

// Consumes says what triggers a block's routine, per role.
type Consumes struct {
	Every bool   `yaml:"every,omitempty"` // monitor, learn
	Case  bool   `yaml:"case,omitempty"`  // act, stage
	Stage string `yaml:"stage,omitempty"` // stage: the stage of the kind's cases it works
	Check bool   `yaml:"check,omitempty"` // verify: the registry's check of the action
}

// Param is one typed parameter of a block. Exactly one of Env and Arg says where its value goes on the command.
type Param struct {
	Name     string `yaml:"-"`
	Kind     string `yaml:"kind"`
	Optional bool   `yaml:"optional,omitempty"`
	Env      string `yaml:"env,omitempty"` // the environment variable the command sets
	Arg      string `yaml:"arg,omitempty"` // the command-line flag the value follows
}

// Params keeps the declaration order: it is the order of the generated command line.
type Params []Param

// UnmarshalYAML reads a mapping, in its order.
func (ps *Params) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.MappingNode {
		return errors.New("params: a mapping of name to {kind: ...}")
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		var p Param
		if err := n.Content[i+1].Decode(&p); err != nil {
			return fmt.Errorf("params %s: %w", n.Content[i].Value, err)
		}
		p.Name = n.Content[i].Value
		*ps = append(*ps, p)
	}
	return nil
}

// Block is one block version, as its block.yaml declares it.
type Block struct {
	Block       string `yaml:"block"`
	Version     int    `yaml:"version"`
	Role        string `yaml:"role"`
	Description string `yaml:"description"`
	Consumes    Consumes
	Emits       []string `yaml:"emits"`
	HandsTo     []string `yaml:"hands_to"`
	// Actions are what the block needs in the registry: a need (precondition_of, check_of) to the param that names the action.
	Actions map[string]string `yaml:"actions"`
	Params  Params            `yaml:"params"`
	// Env is what the worker must provide (documentation: the worker's config is the installation's, not checked here).
	Env []string `yaml:"env"`
	// Model is none, or station: the block ships its stations (SpecTail) and a flow names the model alias only.
	Model string `yaml:"model"`
	// MaxAutonomy is the cap: a flow may lower it, never raise it. Autonomy is the level a routine starts at.
	MaxAutonomy string  `yaml:"max_autonomy"`
	Autonomy    string  `yaml:"autonomy"`
	BudgetUSD   float64 `yaml:"budget_usd"`
	Timeout     string  `yaml:"timeout"`
	Every       string  `yaml:"every"`     // the default schedule of a monitor or learn block
	Freshness   string  `yaml:"freshness"` // and its default freshness
	// Command is the run's command, before the params' env and args. {{fleet}} is the fleet's name.
	Command string `yaml:"command"`
	// CaseKind is the kind the block works on when it is its own (process_change, sop_proposal); empty: the flow's.
	CaseKind string `yaml:"case_kind"`
	// Code lists the repo files the block runs (checked by the repo's tests: the workers carry them at the same paths).
	Code []string `yaml:"code"`
	// Texts are files of the block's folder that each routine version carries (prompts), copied byte for byte.
	Texts []string `yaml:"texts"`
	// SpecTail is a file of the block's folder appended to the version's spec.yaml after its run section (stations,
	// limits, explain). $MODEL is replaced by the flow's model alias and $WORKER by the step's worker.
	SpecTail string `yaml:"spec_tail"`

	Dir string `yaml:"-"`
}

// BlockRef is a step's pin: use: name@n.
type BlockRef struct {
	Name    string
	Version int
}

func (r BlockRef) String() string { return r.Name + "@" + strconv.Itoa(r.Version) }

// ParseUse reads name@n.
func ParseUse(s string) (BlockRef, error) {
	m := useRe.FindStringSubmatch(s)
	if m == nil {
		return BlockRef{}, fmt.Errorf("use %q: a block pinned to a version, name@n", s)
	}
	n, _ := strconv.Atoi(m[2])
	return BlockRef{m[1], n}, nil
}

// BlockDir is where a block version lives under a root.
func BlockDir(root string, r BlockRef) string {
	return filepath.Join(root, r.Name, "v"+strconv.Itoa(r.Version))
}

// LoadBlock reads and validates the block version in dir. Unknown fields are errors.
func LoadBlock(dir string) (*Block, error) {
	p := filepath.Join(dir, BlockFile)
	raw, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	var b Block
	if err := dec.Decode(&b); err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	b.Dir = dir
	b.Description = strings.Join(strings.Fields(b.Description), " ")
	if err := b.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	return &b, nil
}

func (b *Block) validate() error {
	var errs []error
	bad := func(f string, a ...any) { errs = append(errs, fmt.Errorf(f, a...)) }
	if !blockNameRe.MatchString(b.Block) {
		bad("block %q: lower-case letters, digits and dashes", b.Block)
	}
	if b.Version < 1 {
		bad("version %d: a number from 1", b.Version)
	}
	if want := filepath.Base(filepath.Dir(b.Dir)); b.Block != want {
		bad("block %q, but the folder is under %s/", b.Block, want)
	}
	if want := "v" + strconv.Itoa(b.Version); filepath.Base(b.Dir) != want {
		bad("version %d, but the folder is %s", b.Version, filepath.Base(b.Dir))
	}
	if !blockRoles[b.Role] {
		bad("role %q: monitor, act, stage, verify or learn", b.Role)
	}
	if b.Description == "" {
		bad("description is required")
	}
	b.validateConsumes(bad)
	for _, e := range b.Emits {
		if !emitKinds[e] {
			bad("emits %q: detection, plan, hand_over or verified", e)
		}
	}
	if b.Role == RoleMonitor || b.Role == RoleLearn {
		if len(b.Emits) != 1 || b.Emits[0] != "detection" {
			bad("a %s block emits [detection]", b.Role)
		}
	}
	if len(b.HandsTo) > 0 && !contains(b.Emits, "hand_over") {
		bad("hands_to names stages, but the block does not emit hand_over")
	}
	for _, s := range b.HandsTo {
		if !nameRe.MatchString(s) {
			bad("hands_to %q: a stage name", s)
		}
	}
	params := map[string]Param{}
	for _, p := range b.Params {
		if !labelNameRe.MatchString(p.Name) {
			bad("param %q: a plain name", p.Name)
		}
		if !paramKinds[p.Kind] {
			bad("param %s kind %q: not a kind the library knows", p.Name, p.Kind)
		}
		if (p.Env == "") == (p.Arg == "") {
			bad("param %s: exactly one of env and arg says where its value goes", p.Name)
		}
		if p.Env != "" && !envNameRe.MatchString(p.Env) {
			bad("param %s env %q: an upper-case variable name", p.Name, p.Env)
		}
		if p.Arg != "" && !argNameRe.MatchString(p.Arg) {
			bad("param %s arg %q: a --flag", p.Name, p.Arg)
		}
		if _, dup := params[p.Name]; dup {
			bad("param %s is declared twice", p.Name)
		}
		params[p.Name] = p
	}
	for need, pn := range b.Actions {
		if need != "precondition_of" && need != "check_of" {
			bad("actions %q: precondition_of or check_of", need)
		}
		if p, ok := params[pn]; !ok || p.Kind != KindRegistryAction {
			bad("actions %s: %q is not a param of kind registry_action", need, pn)
		}
	}
	for _, e := range b.Env {
		if !envNameRe.MatchString(e) {
			bad("env %q: an upper-case variable name", e)
		}
	}
	switch b.Model {
	case "none":
		if b.SpecTail != "" && b.hasStation() {
			bad("model none, but the spec tail declares stations")
		}
	case "station":
		if b.SpecTail == "" {
			bad("model station: the block ships its stations in a spec_tail")
		}
	default:
		bad("model %q: none or station", b.Model)
	}
	if !levels[b.MaxAutonomy] {
		bad("max_autonomy %q: record, propose or auto", b.MaxAutonomy)
	}
	if b.Autonomy == "" {
		b.Autonomy = AutonomyRecord
	}
	if !levels[b.Autonomy] || levelRank[b.Autonomy] > levelRank[b.MaxAutonomy] {
		bad("autonomy %q: a level at or below max_autonomy %s", b.Autonomy, b.MaxAutonomy)
	}
	if b.BudgetUSD < 0 {
		bad("budget_usd %v: not negative", b.BudgetUSD)
	}
	if b.Timeout == "" {
		bad("timeout is required (the run's)")
	} else {
		durField(bad, "timeout", b.Timeout, time0)
	}
	for _, d := range []struct{ f, v string }{{"every", b.Every}, {"freshness", b.Freshness}} {
		if d.v != "" {
			durField(bad, d.f, d.v, time0)
		}
	}
	if b.Command == "" || !strings.HasPrefix(b.Command, "python3 /opt/flotilla/") {
		bad("command %q: python3 /opt/flotilla/<path>, the path the workers carry the code at", b.Command)
	}
	if b.CaseKind != "" && !caseFieldRe.MatchString(b.CaseKind) {
		bad("case_kind %q: a case kind", b.CaseKind)
	}
	if len(b.Code) == 0 {
		bad("code: the repo files the block runs")
	}
	for _, t := range b.Texts {
		if !validPromptPath(t) {
			bad("texts %q: a clean .md or .txt file name in the block's folder", t)
		}
		if _, err := os.Stat(filepath.Join(b.Dir, t)); err != nil {
			bad("texts %q: %v", t, err)
		}
	}
	if b.SpecTail != "" {
		tailErrs(b, bad)
	}
	return errors.Join(errs...)
}

// time0 is no minimum: a block's durations only need to parse.
const time0 = 0

func (b *Block) validateConsumes(bad func(string, ...any)) {
	c := b.Consumes
	switch b.Role {
	case RoleMonitor, RoleLearn:
		if !c.Every || c.Case || c.Stage != "" || c.Check {
			bad("a %s block consumes {every: true}", b.Role)
		}
	case RoleAct:
		if !c.Case || c.Every || c.Stage != "" || c.Check {
			bad("an act block consumes {case: true}")
		}
	case RoleStage:
		if !c.Case || c.Every || c.Check || !nameRe.MatchString(c.Stage) {
			bad("a stage block consumes {case: true, stage: <name>}")
		}
	case RoleVerify:
		if !c.Check || c.Every || c.Case || c.Stage != "" {
			bad("a verify block consumes {check: true}")
		}
	}
}

// tailErrs reads the spec tail as the generator will use it: it must parse with a model alias in place, carry no
// run section, and exist.
func tailErrs(b *Block, bad func(string, ...any)) {
	raw, err := os.ReadFile(filepath.Join(b.Dir, b.SpecTail))
	if err != nil {
		bad("spec_tail: %v", err)
		return
	}
	var m map[string]any
	if err := yaml.Unmarshal(fillTail(raw, "alias", "worker"), &m); err != nil {
		bad("spec_tail %s: %v", b.SpecTail, err)
		return
	}
	if _, has := m["run"]; has {
		bad("spec_tail %s: run is the generator's", b.SpecTail)
	}
	if b.Model == "station" && !bytes.Contains(raw, []byte("$MODEL")) {
		bad("spec_tail %s: a block with a station names $MODEL for the flow's alias", b.SpecTail)
	}
	if b.Model == "none" && bytes.Contains(raw, []byte("$MODEL")) {
		bad("spec_tail %s: names $MODEL, but the block has no model", b.SpecTail)
	}
}

func fillTail(raw []byte, model, worker string) []byte {
	return bytes.ReplaceAll(bytes.ReplaceAll(raw, []byte("$MODEL"), []byte(model)), []byte("$WORKER"), []byte(worker))
}

// hasStation is whether the tail declares stations (read leniently: validate runs before tailErrs).
func (b *Block) hasStation() bool {
	raw, err := os.ReadFile(filepath.Join(b.Dir, b.SpecTail))
	if err != nil {
		return false
	}
	var m map[string]any
	if yaml.Unmarshal(fillTail(raw, "alias", "worker"), &m) != nil {
		return false
	}
	_, has := m["stations"]
	return has
}

// SpecTailText is the tail with the step's model alias and worker in place.
func (b *Block) SpecTailText(model, worker string) ([]byte, error) {
	if b.SpecTail == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(filepath.Join(b.Dir, b.SpecTail))
	if err != nil {
		return nil, err
	}
	return fillTail(raw, model, worker), nil
}

// ParamByName is the declared param.
func (b *Block) ParamByName(name string) (Param, bool) {
	for _, p := range b.Params {
		if p.Name == name {
			return p, true
		}
	}
	return Param{}, false
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// LoadLibrary reads every block version under root (blocks/<name>/v<n>/block.yaml): the library as the repo ships
// it. Every error is named; an empty library is an error.
func LoadLibrary(root string) (map[BlockRef]*Block, error) {
	paths, err := filepath.Glob(filepath.Join(root, "*", "v*", BlockFile))
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	out := map[BlockRef]*Block{}
	var errs []error
	for _, p := range paths {
		b, err := LoadBlock(filepath.Dir(p))
		if err != nil {
			errs = append(errs, err)
			continue
		}
		out[BlockRef{b.Block, b.Version}] = b
	}
	if len(errs) == 0 && len(out) == 0 {
		errs = append(errs, fmt.Errorf("no blocks under %s", root))
	}
	return out, errors.Join(errs...)
}
