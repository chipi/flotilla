package spec

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Flows (RFC-0016): a flow is a folder in a fleet, fleets/<fleet>/flows/<flow>/flow.yaml, that wires blocks into the
// routines of one chain. The generator expands it into the ordinary routine folders (routine.yaml and v<n>/spec.yaml)
// and the expansion is committed: -check refuses when the committed folders differ from what the generator writes now.
// Nothing about versions, the freeze, shadows or the ledger changes: a flow only writes files.

// FlowsDir is a fleet's folder of flows, beside its routines.
const FlowsDir = "flows"

// FlowFile and LearnFile are the files of a flow and of the fleet's learning blocks.
const (
	FlowFile  = "flow.yaml"
	LearnFile = "learn.yaml"
)

// StepCase is what a step's case trigger adds to the kind and stage its block fixes.
type StepCase struct {
	OnRecord string `yaml:"on_record"`
}

// FlowStep is one step of a flow, or one of the fleet's learning blocks: a block pinned, becoming one routine.
type FlowStep struct {
	Name string `yaml:"name"` // the routine it becomes
	Use  string `yaml:"use"`  // name@n
	// Version is the routine version folder it writes (default 1). A bumped pin is a new routine version: the old folder is frozen
	// and left as it is.
	Version     int       `yaml:"version"`
	Description string    `yaml:"description"` // one line of the routine's description per line of this text
	Worker      string    `yaml:"worker"`
	Every       string    `yaml:"every"`
	Freshness   string    `yaml:"freshness"`
	Autonomy    string    `yaml:"autonomy"`
	MaxAutonomy string    `yaml:"max_autonomy"` // written into routine.yaml when given; never above the block's
	BudgetUSD   *float64  `yaml:"budget_usd"`
	Timeout     string    `yaml:"timeout"`
	Model       string    `yaml:"model"` // the alias a block with a station runs on
	Case        *StepCase `yaml:"case"`
	Verify      string    `yaml:"verify"` // a verify step: the registry check it works
	With        map[string]any
}

// Flow is one flows/<flow>/flow.yaml.
type Flow struct {
	Flow     string     `yaml:"flow"`
	CaseKind string     `yaml:"case_kind"`
	Steps    []FlowStep `yaml:"steps"`
	Dir      string     `yaml:"-"`
}

type learnFile struct {
	Learn []FlowStep `yaml:"learn"`
}

// FleetFlows is every flow of one fleet and its learning blocks.
type FleetFlows struct {
	Fleet string
	Dir   string // fleets/<fleet>
	Flows []*Flow
	Learn []FlowStep
}

// ActionInfo is what a flow's checks need to know of a registry action.
type ActionInfo struct {
	Pre      []string
	Verify   string
	ForKinds []string
}

// Actions looks an action up in the registry. nil: the actions are not checked (the generator ran without a registry).
type Actions func(name string) (ActionInfo, bool)

func decodeStrict(p string, v any) error {
	raw, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("%s: %w", p, err)
	}
	return nil
}

// LoadFlows reads fleets/<fleet>/flows/: each <flow>/flow.yaml and the fleet's learn.yaml. It returns nil when the fleet has no flows.
func LoadFlows(fleetDir string) (*FleetFlows, error) {
	root := filepath.Join(fleetDir, FlowsDir)
	if st, err := os.Stat(root); err != nil || !st.IsDir() {
		return nil, nil
	}
	ff := &FleetFlows{Fleet: filepath.Base(fleetDir), Dir: fleetDir}
	var errs []error
	paths, err := filepath.Glob(filepath.Join(root, "*", FlowFile))
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	for _, p := range paths {
		var f Flow
		if err := decodeStrict(p, &f); err != nil {
			errs = append(errs, err)
			continue
		}
		f.Dir = filepath.Dir(p)
		if want := filepath.Base(f.Dir); f.Flow != want {
			errs = append(errs, fmt.Errorf("%s: flow %q, but the file is under %s/", p, f.Flow, want))
			continue
		}
		ff.Flows = append(ff.Flows, &f)
	}
	if p := filepath.Join(root, LearnFile); fileExists(p) {
		var l learnFile
		if err := decodeStrict(p, &l); err != nil {
			errs = append(errs, err)
		}
		ff.Learn = l.Learn
	}
	if len(errs) == 0 && len(ff.Flows) == 0 && len(ff.Learn) == 0 {
		errs = append(errs, fmt.Errorf("%s: a flows folder with no flow.yaml and no learn.yaml", root))
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return ff, nil
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// Expansion is what the generator writes for a fleet: the files of its routine folders, by path under the fleet's folder.
type Expansion struct {
	Files map[string][]byte
	Steps []ExpandedStep
}

// ExpandedStep is one step as resolved: what the wiring checks read.
type ExpandedStep struct {
	Flow, Name string // Flow is "" for a learning block
	Block      *Block
	CaseKind   string
	Stage      string
	Verify     string
	Version    int
}

// RoutineDirs are the folders the expansion owns: each step's v<n>.
func (e *Expansion) versionDirs() []string {
	var out []string
	for _, s := range e.Steps {
		out = append(out, s.Name+"/v"+strconv.Itoa(s.Version))
	}
	return out
}

var (
	aliasRe      = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	workerNameRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,39}$`)
)

type expander struct {
	ff      *FleetFlows
	libRoot string
	actions Actions
	cache   map[string]*Block
	errs    []error
}

func (x *expander) bad(f string, a ...any) { x.errs = append(x.errs, fmt.Errorf(f, a...)) }

func (x *expander) block(root string, r BlockRef) (*Block, error) {
	dir := BlockDir(root, r)
	if b, ok := x.cache[dir]; ok {
		return b, nil
	}
	if _, err := os.Stat(filepath.Join(dir, BlockFile)); err != nil {
		return nil, err
	}
	b, err := LoadBlock(dir)
	if err != nil {
		return nil, err
	}
	x.cache[dir] = b
	return b, nil
}

// Expand resolves every step's pin and writes, in memory, the routine folders of the fleet's flows. Every wiring error is
// returned, named by flow and step; nothing is returned on any.
func (ff *FleetFlows) Expand(libRoot string, actions Actions) (*Expansion, error) {
	x := &expander{ff: ff, libRoot: libRoot, actions: actions, cache: map[string]*Block{}}
	exp := &Expansion{Files: map[string][]byte{}}
	names := map[string]string{}
	acting := map[string]string{}
	checks := map[string]string{}
	flowKinds := map[string]string{}
	for _, f := range ff.Flows {
		if !nameRe.MatchString(f.Flow) {
			x.bad("flow %q: lower-case letters, digits and dashes", f.Flow)
		}
		if !caseFieldRe.MatchString(f.CaseKind) {
			x.bad("flow %s: case_kind %q: a case kind", f.Flow, f.CaseKind)
		}
		flowKinds[f.Flow] = f.CaseKind
	}
	var all []resolvedStep
	each := func(f *Flow, steps []FlowStep) {
		where := "learn"
		if f != nil {
			where = "flow " + f.Flow
		}
		for _, st := range steps {
			if st.Name == "" || !nameRe.MatchString(st.Name) || st.Name == FlowsDir {
				x.bad("%s: step name %q: lower-case letters, digits and dashes (the routine it becomes)", where, st.Name)
				continue
			}
			if prev, dup := names[st.Name]; dup {
				x.bad("%s: step %s is already a step of %s", where, st.Name, prev)
				continue
			}
			names[st.Name] = where
			es, ok := x.step(f, st, where, flowKinds)
			if ok {
				all = append(all, resolvedStep{f, st, es})
			}
		}
	}
	for _, f := range ff.Flows {
		each(f, f.Steps)
	}
	each(nil, ff.Learn)
	if len(x.errs) > 0 { // a step that did not resolve would make every wiring check about it noise
		return nil, errors.Join(x.errs...)
	}
	// The wiring across steps.
	for _, r := range all {
		b, where := r.es.Block, "step "+r.step.Name
		for _, s := range b.HandsTo {
			if !hasStage(all, r.flow, s) {
				x.bad("%s: %s hands cases to stage %q, and the flow has no step for it", where, b.Block, s)
			}
		}
		switch b.Role {
		case RoleAct, RoleStage:
			key := r.es.CaseKind + "/" + r.es.Stage
			if prev, dup := acting[key]; dup {
				x.bad("%s: %s cases at stage %q already have an acting step, %s", where, r.es.CaseKind, r.es.Stage, prev)
			}
			acting[key] = r.step.Name
		case RoleVerify:
			if prev, dup := checks[r.es.Verify]; dup {
				x.bad("%s: the check %s already has a step, %s", where, r.es.Verify, prev)
			}
			checks[r.es.Verify] = r.step.Name
		}
	}
	if len(x.errs) > 0 {
		return nil, errors.Join(x.errs...)
	}
	for _, r := range all {
		files, err := x.render(r.step, r.es, flowKinds)
		if err != nil {
			x.errs = append(x.errs, fmt.Errorf("step %s: %w", r.step.Name, err))
			continue
		}
		for p, c := range files {
			exp.Files[p] = c
		}
		exp.Steps = append(exp.Steps, r.es)
	}
	if len(x.errs) > 0 {
		return nil, errors.Join(x.errs...)
	}
	return exp, nil
}

type resolvedStep struct {
	flow *Flow // nil for a learning block
	step FlowStep
	es   ExpandedStep
}

// hasStage is whether a step of the flow is a stage block that works the stage. A learning block hands over to nothing.
func hasStage(all []resolvedStep, f *Flow, stage string) bool {
	for _, r := range all {
		if r.flow == f && r.es.Block.Role == RoleStage && r.es.Stage == stage {
			return true
		}
	}
	return false
}

// step resolves one step's pin and checks it against its block: the fields its role takes, the params' kinds, the caps,
// the actions it needs in the registry. The routine's files are rendered later, once the whole wiring is known.
func (x *expander) step(f *Flow, st FlowStep, where string, flowKinds map[string]string) (ExpandedStep, bool) {
	n0 := len(x.errs)
	bad := func(format string, a ...any) { x.bad("%s: step %s: "+format, append([]any{where, st.Name}, a...)...) }
	es := ExpandedStep{Name: st.Name, Version: st.Version}
	if f != nil {
		es.Flow = f.Flow
	}
	if es.Version == 0 {
		es.Version = 1
	} else if es.Version < 1 {
		bad("version %d: a number from 1", st.Version)
	}
	ref, err := ParseUse(st.Use)
	if err != nil {
		bad("%v", err)
		return es, false
	}
	var b *Block
	if f != nil {
		if b, err = x.block(filepath.Join(f.Dir, "blocks"), ref); err != nil && !errors.Is(err, fs.ErrNotExist) {
			bad("%v", err)
			return es, false
		}
	}
	if b == nil {
		if b, err = x.block(x.libRoot, ref); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				bad("use %s: no such block (the library is %s)", ref, x.libRoot)
			} else {
				bad("%v", err)
			}
			return es, false
		}
	}
	es.Block = b
	es.CaseKind = b.CaseKind
	if es.CaseKind == "" && f != nil {
		es.CaseKind = f.CaseKind
	}
	if b.Role != RoleVerify && es.CaseKind == "" {
		bad("%s works on a case kind of the flow's, and a learning block has none of its own", b.Block)
	}
	es.Stage = b.Consumes.Stage
	if !workerNameRe.MatchString(st.Worker) {
		bad("worker %q: the worker that runs it", st.Worker)
	}
	// What the role takes.
	scheduled := b.Role == RoleMonitor || b.Role == RoleLearn
	if scheduled {
		if every := firstNonEmpty(st.Every, b.Every); every == "" {
			bad("every: a %s step is scheduled", b.Role)
		} else {
			durField(bad, "every", every, 0)
		}
	} else if st.Every != "" || st.Freshness != "" {
		bad("every and freshness: only a scheduled step has them")
	}
	if st.Freshness != "" {
		durField(bad, "freshness", st.Freshness, 0)
	}
	if b.SpecTail != "" && scheduled && specTailExplains(b) &&
		((st.Every != "" && st.Every != b.Every) || (st.Freshness != "" && st.Freshness != b.Freshness)) {
		bad("every and freshness: the block's explain text states its defaults (%s, %s); a flow cannot change them under it", b.Every, b.Freshness)
	}
	if st.Case != nil && b.Role != RoleAct && b.Role != RoleStage {
		bad("case: only an act or stage step has a case trigger")
	}
	if st.Case != nil && st.Case.OnRecord != OnRecordClose && st.Case.OnRecord != OnRecordWatch {
		bad("case on_record %q: close or watch", st.Case.OnRecord)
	}
	if b.Role == RoleVerify {
		es.Verify = st.Verify
		if !verifyRe.MatchString(st.Verify) {
			bad("verify %q: the registry check the step works, such as backup_newer_than_execution", st.Verify)
		}
	} else if st.Verify != "" {
		bad("verify: only a verify step has one")
	}
	switch {
	case b.Model == "station" && !aliasRe.MatchString(st.Model):
		bad("model %q: %s has a station, and the flow names the model alias it runs on", st.Model, b.Block)
	case b.Model == "none" && st.Model != "":
		bad("model %q: %s has no model", st.Model, b.Block)
	}
	if st.Timeout != "" {
		durField(bad, "timeout", st.Timeout, 0)
	}
	if st.BudgetUSD != nil && *st.BudgetUSD < 0 {
		bad("budget_usd %v: not negative", *st.BudgetUSD)
	}
	if st.BudgetUSD != nil && *st.BudgetUSD > 0 && b.Model == "none" {
		bad("budget_usd %v: %s has no model to spend on", *st.BudgetUSD, b.Block)
	}
	if st.MaxAutonomy != "" {
		if !levels[st.MaxAutonomy] {
			bad("max_autonomy %q: record, propose or auto", st.MaxAutonomy)
		} else if levelRank[st.MaxAutonomy] > levelRank[b.MaxAutonomy] {
			bad("max_autonomy %s raises %s's cap, %s: a flow may lower a block's cap, never raise it", st.MaxAutonomy, b.Block, b.MaxAutonomy)
		}
	}
	if st.Autonomy != "" {
		cap := firstNonEmpty(st.MaxAutonomy, b.MaxAutonomy)
		if !levels[st.Autonomy] {
			bad("autonomy %q: record, propose or auto", st.Autonomy)
		} else if levels[cap] && levelRank[st.Autonomy] > levelRank[cap] {
			bad("autonomy %s is above the step's cap, %s", st.Autonomy, cap)
		}
	}
	if st.Description != "" {
		if err := checkDescription(st.Description); err != nil {
			bad("description: %v", err)
		}
	}
	// Params: every one declared, and every value of its kind.
	for name := range st.With {
		if _, ok := b.ParamByName(name); !ok {
			bad("with %s: %s has no such param", name, b.Block)
		}
	}
	for _, p := range b.Params {
		v, given := st.With[p.Name]
		if !given {
			if !p.Optional {
				bad("with %s: required (%s)", p.Name, p.Kind)
			}
			continue
		}
		if _, err := renderParam(p, v, flowKinds); err != nil {
			bad("with %s: %v", p.Name, err)
		}
	}
	x.checkActions(b, st, es, bad)
	return es, len(x.errs) == n0
}

func firstNonEmpty(s ...string) string {
	for _, x := range s {
		if x != "" {
			return x
		}
	}
	return ""
}

// specTailExplains is whether the block's spec tail carries an explain section: texts that state the block's schedule.
func specTailExplains(b *Block) bool {
	raw, err := os.ReadFile(filepath.Join(b.Dir, b.SpecTail))
	return err == nil && (bytes.Contains(raw, []byte("\nexplain:")) || bytes.HasPrefix(raw, []byte("explain:")))
}

// checkActions is the registry's side of the wiring: each action a param names exists and serves the flow's case kind, a
// monitor's action carries the precondition it reads, and a verify step names the check its action declares.
func (x *expander) checkActions(b *Block, st FlowStep, es ExpandedStep, bad func(string, ...any)) {
	if x.actions == nil {
		return
	}
	for _, p := range b.Params {
		v, ok := st.With[p.Name].(string)
		if p.Kind != KindRegistryAction || !ok {
			continue
		}
		info, found := x.actions(v)
		if !found {
			bad("with %s: the registry has no action %s", p.Name, v)
			continue
		}
		if es.CaseKind != "" && len(info.ForKinds) > 0 && !contains(info.ForKinds, es.CaseKind) {
			bad("with %s: %s is not an action for %s cases (it is for %s)", p.Name, v, es.CaseKind, strings.Join(info.ForKinds, ", "))
		}
		for need, pn := range b.Actions {
			if pn != p.Name {
				continue
			}
			switch need {
			case "precondition_of":
				if len(info.Pre) == 0 {
					bad("with %s: %s declares no precondition for %s to read", p.Name, v, b.Block)
				}
			case "check_of":
				if info.Verify == "" || info.Verify != es.Verify {
					bad("verify %q: %s's registry check is %q", es.Verify, v, info.Verify)
				}
			}
		}
	}
}

func checkDescription(d string) error {
	for i, l := range strings.Split(d, "\n") {
		if l == "" || l != strings.TrimSpace(l) || strings.ContainsAny(l, "\t\r") {
			return fmt.Errorf("line %d: a line of text with no leading or trailing space (each is one line of the routine's folded description)", i+1)
		}
	}
	return nil
}
