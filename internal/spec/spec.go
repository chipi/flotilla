// Package spec loads and validates routine specs: fleets/<fleet>/<routine>.yaml.
//
// A spec says what a routine is: when it runs, which worker runs it, what it runs, how long it may take,
// what it may spend, and its default autonomy. It holds nothing about one installation (hosts, paths,
// secrets); those are the worker's and flotillad's config.
package spec

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Autonomy levels, a lifecycle (ADR-0008): record, then propose, then auto. The lowest level records
// the plan and acts on nothing; it was called shadow until routine versions (v2 step 1) took that word.
// What each allows is the routine's business: the autonomy is passed to the routine as FLEETD_STAGE,
// and the triage fleet enforces it.
const (
	AutonomyRecord  = "record"
	AutonomyPropose = "propose"
	AutonomyAuto    = "auto"
)

// deprecatedShadow is the old name of AutonomyRecord. A spec may still say it (a deploy that predates the
// rename keeps loading); the loader reads it as record and says so in Routine.Notes. The spec's bytes,
// and so its hash, are untouched.
const deprecatedShadow = "shadow"

var levels = map[string]bool{AutonomyRecord: true, AutonomyPropose: true, AutonomyAuto: true}

var levelRank = map[string]int{AutonomyRecord: 0, AutonomyPropose: 1, AutonomyAuto: 2}

// FleetdStage is the FLEETD_STAGE value routines read. It is a contract older than the rename: the
// triage fleet's dry-run guarantee and the ops routines test the string "shadow", so record maps to
// it, and auto to "live" (ADR-0008 keeps both until the code is ported). Only a new version of a routine
// may read the new name; spec_test.go pins the table.
func FleetdStage(autonomy string) string {
	switch autonomy {
	case AutonomyAuto:
		return "live"
	case AutonomyRecord:
		return deprecatedShadow
	}
	return autonomy
}

// ShadowNote is what the API and CLI say when they are given the deprecated word.
const ShadowNote = "shadow is now called record"

// CanonicalLevel reads the deprecated level word as record. The note is ShadowNote when it did, else empty.
func CanonicalLevel(level string) (canonical, note string) {
	if level == deprecatedShadow {
		return AutonomyRecord, ShadowNote
	}
	return level, ""
}

// normalizeLevels reads the deprecated autonomy word as record, and notes each use.
func (j *Routine) normalizeLevels() {
	if j.Autonomy == deprecatedShadow {
		j.Autonomy = AutonomyRecord
		j.Notes = append(j.Notes, "autonomy: shadow is the old name of record; write record")
	}
	if j.MaxAutonomy == deprecatedShadow {
		j.MaxAutonomy = AutonomyRecord
		j.Notes = append(j.Notes, "max_autonomy: shadow is the old name of record; write record")
	}
}

// Rank orders the autonomy levels (record 0, propose 1, auto 2), so a change can be told apart as
// raising or lowering.
func Rank(autonomy string) int { return levelRank[autonomy] }

// Allows reports whether autonomy s is within the routine's cap (max_autonomy; no cap when it is empty).
func (j *Routine) Allows(s string) bool {
	return j.MaxAutonomy == "" || levelRank[s] <= levelRank[j.MaxAutonomy]
}

// Acceptance kinds a routine may require of a version before it is promoted.
const (
	AcceptPairs        = "pairs"
	AcceptReplay       = "replay"
	AcceptScenarioEval = "scenario_eval"
)

// AcceptanceKinds is the set of kinds a routine may declare.
var AcceptanceKinds = map[string]bool{AcceptPairs: true, AcceptReplay: true, AcceptScenarioEval: true}

// Accepts is the evidence a promote of this routine's versions needs: what acceptance: declares, [pairs]
// when it declares nothing.
func (j *Routine) Accepts() []string {
	if len(j.Acceptance) == 0 {
		return []string{AcceptPairs}
	}
	return append([]string(nil), j.Acceptance...)
}

var nameRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,39}$`)

type Routine struct {
	Fleet       string   `yaml:"fleet"`
	Routine     string   `yaml:"routine"`
	Description string   `yaml:"description"`
	Enabled     bool     `yaml:"enabled"`
	Worker      string   `yaml:"worker"`
	Trigger     Trigger  `yaml:"trigger"`
	Run         Run      `yaml:"run"`
	Autonomy    string   `yaml:"autonomy"`
	MaxAutonomy string   `yaml:"max_autonomy"` // optional: the highest autonomy the routine may ever run at
	Budget      Budget   `yaml:"budget"`
	Freshness   string   `yaml:"freshness"`
	Skills      Skills   `yaml:"skills"`  // ADR-0012: the skills the routine may load; default none
	Harness     *Harness `yaml:"harness"` // RFC-0002 §4: the routine's model-driven step, when it has one
	// The system prompts the routine runs with (ADR-0018): a role (system, rules) to a file under the
	// fleet's folder. `system` is optional: a harness routine without it loads its default as before.
	Prompts map[string]string `yaml:"prompts"`
	Resume  *Resume           `yaml:"resume"` // ADR-0019: when flotillad lets a case's next run continue the last one
	// The case kinds this routine may open (pipeline v1 step 6): a monitor reports detections in its
	// plan file and flotillad opens the cases; a kind not listed here is refused.
	Detects []string `yaml:"detects"`
	// The model aliases (flotillad.yaml models) this routine may call (security T4): its runs get only
	// these in FLOTILLA_MODELS, so an alias it does not name does not exist for it. Default none.
	Models []string `yaml:"models"`
	// The brief's section a monitor's summary lines go to (RFC-0012 §4): a monitor may report up to 20
	// lines with its detections, and the brief written after its run shows them under this title. Only
	// a monitor (one that detects) has one.
	BriefSection string `yaml:"brief_section"`
	// What the routine page draws (explain.go): a text per drawn block, keyed by the spec part it explains,
	// the steps its code takes that no other field shows, and the routines that come after it.
	Explain map[string]string `yaml:"explain"`
	Steps   []Step            `yaml:"steps"`
	Chain   []ChainLink       `yaml:"chain"`
	// What a promote of one of this routine's versions needs as evidence (docs/wip/v2-design.md section 6): a
	// list from pairs, replay and scenario_eval; Accepts says [pairs] when it is empty.
	Acceptance []string `yaml:"acceptance"`
	// Shadows (v2 step 2, design section 4). ShadowWorker is the worker a shadow version of this routine runs on (default
	// "<worker>-shadow"); State is the routine's own state, paths under its worker's HOME that the live worker snapshots when a
	// live run starts and a shadow starts from; Repo is the repository it works on, whose branch head flotillad resolves at dispatch.
	ShadowWorker string   `yaml:"shadow_worker"`
	State        []string `yaml:"state"`
	Repo         *RepoRef `yaml:"repo"`

	// ConfigDisabled is set by flotillad for a routine its configuration lists in disabled_routines (not by a spec's enabled: false,
	// which still allows a run by hand): no run starts, scheduled or by hand, whatever a caller asks.
	ConfigDisabled bool `yaml:"-"`

	// filled by Load
	Path string `yaml:"-"`
	Hash string `yaml:"-"`
	// The routine's versions (version.go): every loaded version, the number of the one these fields are
	// bound to (0 when none loaded) and its frozen hash. A flat spec is one routine with one version, v1.
	Notes       []string      `yaml:"-"` // what -check says about the spec itself (a deprecated word)
	Versions    []*Version    `yaml:"-"`
	Version     int           `yaml:"-"`
	VersionHash string        `yaml:"-"`
	Every       time.Duration `yaml:"-"`
	Limit       time.Duration `yaml:"-"`
	Fresh       time.Duration `yaml:"-"`
	// the sha256 of each declared prompt file when the spec was loaded ("" for a file that cannot be
	// read: prompts.Check refuses it)
	PromptSHA map[string]string `yaml:"-"`
	// A version with stations (stations.go): its stations, their texts as loaded, and the version's own flow limits. Never read from a flat spec.
	Stations      map[string]*Station               `yaml:"-"`
	StationText   map[string]map[string]StationText `yaml:"-"`
	VersionLimits map[string]any                    `yaml:"-"`
}

// RepoRef is the repository a routine works on and the branch whose head a run starts from.
type RepoRef struct {
	Name   string `yaml:"name"`
	Branch string `yaml:"branch"`
}

var (
	repoNameRe  = regexp.MustCompile(`^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$`)
	branchRe    = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,100}$`)
	statePathRe = regexp.MustCompile(`^[A-Za-z0-9._][A-Za-z0-9._/-]{0,199}$`)
)

// ShadowWorkerName is the worker a shadow version of the routine runs on.
func (j *Routine) ShadowWorkerName() string {
	if j.ShadowWorker != "" {
		return j.ShadowWorker
	}
	return j.Worker + "-shadow"
}

// Trigger is what starts the routine (ADR-0007): time (every) or an event on a case (RFC-0004 §3.7):
// its hand-over (case), the execution of an approved repo action (execute) or its check after an
// execution (check). Exactly one of them.
type Trigger struct {
	Every   string          `yaml:"every"`
	Case    *CaseTrigger    `yaml:"case"`
	Execute *ExecuteTrigger `yaml:"execute"`
	Check   *CheckTrigger   `yaml:"check"`
}

// CaseTrigger starts the routine when a case of its kind is handed over: it is that kind's acting
// routine (pipeline v1 step 5). One routine per kind and stage (RFC-0013 §2): the empty stage is the
// kind's first routine, and an acting run may hand the case on to another stage.
type CaseTrigger struct {
	Kind  string     `yaml:"kind"`
	Stage string     `yaml:"stage"`
	Limit *CaseLimit `yaml:"limit"`
	// OnRecord is what a plan recorded at the record level does to its case: close it (the default) or keep it
	// open as watching until its monitor reports the key clear (ADR-0023).
	OnRecord string `yaml:"on_record"`
}

// What a recorded plan does to its case (CaseTrigger.OnRecord).
const (
	OnRecordClose = "close"
	OnRecordWatch = "watch"
)

// CaseLimit is what all of a case's acting runs may spend together, in dollars and in minutes (RFC-0013
// §2). It is declared on the kind's first-stage routine; a case past either is escalated instead of run
// again. Spend counts only what the runs report (a routine whose model calls flotillad does not see adds
// time, not money).
type CaseLimit struct {
	USD     float64 `yaml:"usd"`
	Minutes int     `yaml:"minutes"`
}

// ExecuteTrigger starts the routine when a person has approved a plan for this repo action (RFC-0010
// §3): it carries the approved plan out (opens the PR) and reports it. One routine per action. A host
// action never has one: its host agent executes it, signed (ADR-0006).
type ExecuteTrigger struct {
	Action string `yaml:"action"`
}

// CheckTrigger starts the routine while a case is in checking, for an action whose registry check is
// this verify condition (RFC-0004 §7: verify within minutes). One routine per condition.
type CheckTrigger struct {
	Verify string `yaml:"verify"`
}

// OnCase is the case kind the routine acts on, "" for a routine on time.
func (j *Routine) OnCase() string {
	if j.Trigger.Case == nil {
		return ""
	}
	return j.Trigger.Case.Kind
}

// OnStage is the stage of its kind's cases the routine acts on ("" is the first).
func (j *Routine) OnStage() string {
	if j.Trigger.Case == nil {
		return ""
	}
	return j.Trigger.Case.Stage
}

// OnExecute is the repo action the routine executes, "" for any other routine.
func (j *Routine) OnExecute() string {
	if j.Trigger.Execute == nil {
		return ""
	}
	return j.Trigger.Execute.Action
}

// OnCheck is the verify condition the routine checks, "" for any other routine.
func (j *Routine) OnCheck() string {
	if j.Trigger.Check == nil {
		return ""
	}
	return j.Trigger.Check.Verify
}

// OnEvent: the routine runs when a case calls for it, never on time.
func (j *Routine) OnEvent() bool {
	return j.Trigger.Case != nil || j.Trigger.Execute != nil || j.Trigger.Check != nil
}

// Skills (ADR-0012, RFC-0005 §4): rules the routine's code matches against its case's structured fields;
// every rule that matches loads its skills, in spec order. On-demand loading comes with RFC-0002 §9 step 4.
type Skills struct {
	Rule     []SkillRule `yaml:"rule"`
	OnDemand []string    `yaml:"on_demand"`
}

type SkillRule struct {
	When map[string]string `yaml:"when" json:"when"`
	Load []string          `yaml:"load" json:"load"`
}

var (
	skillNameRe    = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	caseFieldRe    = regexp.MustCompile(`^[a-z][a-z0-9_]{0,39}$`)
	verifyRe       = regexp.MustCompile(`^[a-z][a-z0-9_]*(\([^()]*\))?$`)        // as the registry's conditions
	actionRe       = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)+$`) // as the registry's names
	briefSectionRe = regexp.MustCompile(`^[A-Z][A-Za-z ]{0,31}$`)
	promptPathRe   = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,200}$`)
)

// Resume (ADR-0019) says when a case's next run may continue the one that was cut short: the previous run
// ended with one of the statuses in On (interrupted, error), at most Max times in a row. The run gets
// FLOTILLA_RESUME and decides by its own checkpoint whether it can; flotillad only says it may.
type Resume struct {
	On  []string `yaml:"on"`
	Max int      `yaml:"max"`
}

var resumeOn = map[string]bool{"interrupted": true, "error": true}

// ResumesOn is whether a run that ended with status may be resumed.
func (r *Resume) ResumesOn(status string) bool {
	if r == nil {
		return false
	}
	for _, s := range r.On {
		if s == status {
			return true
		}
	}
	return false
}

func (j *Routine) validateResume(bad func(string, ...any)) {
	r := j.Resume
	if r == nil {
		return
	}
	if j.OnCase() == "" {
		bad("resume: only a routine that acts on a case (trigger.case) has runs to resume")
	}
	if len(r.On) == 0 {
		bad("resume.on: at least one of interrupted, error")
	}
	seen := map[string]bool{}
	for _, s := range r.On {
		if !resumeOn[s] || seen[s] {
			bad("resume.on: %q is not interrupted or error, or is listed twice", s)
		}
		seen[s] = true
	}
	if r.Max < 1 || r.Max > 3 {
		bad("resume.max %d: 1 to 3", r.Max)
	}
}

// Harness (RFC-0002 §4) configures the routine's model-driven step: its protocol, the tools offered, the
// gates its decision passes, its limits, and the quirks that keep the first fleet's loop like for like
// until RFC-0002 §9 step 6 removes them one by one.
type Harness struct {
	Protocol string        `yaml:"protocol" json:"protocol"`
	Tools    []string      `yaml:"tools" json:"tools"`
	Gates    []string      `yaml:"gates" json:"gates"`
	Limits   HarnessLimits `yaml:"limits" json:"limits"`
	Quirks   HarnessQuirks `yaml:"quirks" json:"quirks"`
	// Hooks (ADR-0018): code run around each tool call, named here and implemented in the routine's code
	// (runtime/hooks.py). A pointer, so a harness without hooks hands the run the JSON it always did.
	Hooks *HarnessHooks `yaml:"hooks" json:"hooks,omitempty"`
}

type HarnessHooks struct {
	BeforeTool []HookRef `yaml:"before_tool" json:"before_tool,omitempty"`
	AfterTool  []HookRef `yaml:"after_tool" json:"after_tool,omitempty"`
}

// HookRef names a hook; Mode is enforce (the default: its refusal or replacement stands) or observe (it runs and
// what it would have done is recorded, nothing changes).
type HookRef struct {
	Name string `yaml:"name" json:"name"`
	Mode string `yaml:"mode" json:"mode"`
}

type HarnessLimits struct {
	MaxToolCalls     int `yaml:"max_tool_calls" json:"max_tool_calls"`
	MaxSteps         int `yaml:"max_steps" json:"max_steps"`
	ToolCallsPerTurn int `yaml:"tool_calls_per_turn" json:"tool_calls_per_turn"`
}

type HarnessQuirks struct {
	BadJSONUsesTurn   bool `yaml:"bad_json_uses_turn" json:"bad_json_uses_turn"`
	EmptyRetry        bool `yaml:"empty_retry" json:"empty_retry"`
	FinalDecisionCall bool `yaml:"final_decision_call" json:"final_decision_call"`
	ReplyEchoChars    int  `yaml:"reply_echo_chars" json:"reply_echo_chars"`
	DecisionEchoChars int  `yaml:"decision_echo_chars" json:"decision_echo_chars"`
	ResultChars       int  `yaml:"result_chars" json:"result_chars"`
}

// HarnessJSON is what a run gets in FLOTILLA_HARNESS, or "" when the routine has no harness step.
func (j *Routine) HarnessJSON() string {
	if j.Harness == nil {
		return ""
	}
	b, _ := json.Marshal(j.Harness)
	return string(b)
}

// PromptsJSON is what a run gets in FLOTILLA_PROMPTS ({"system": {"path": ..., "sha256": ...}}), or "" when the
// routine declares none. The run refuses a prompt whose file does not hash to this (runtime/prompts.py).
func (j *Routine) PromptsJSON() string {
	if len(j.Prompts) == 0 {
		return ""
	}
	type entry struct {
		Path   string `json:"path"`
		SHA256 string `json:"sha256"`
	}
	m := map[string]entry{}
	for role, p := range j.Prompts {
		m[role] = entry{p, j.PromptSHA[role]}
	}
	b, _ := json.Marshal(m)
	return string(b)
}

// SkillNames lists every skill the spec names, each once, in spec order.
func (j *Routine) SkillNames() []string {
	var out []string
	seen := map[string]bool{}
	for _, r := range j.Skills.Rule {
		for _, n := range r.Load {
			if !seen[n] {
				seen[n], out = true, append(out, n)
			}
		}
	}
	return out
}

// SkillsJSON is what a run gets in FLOTILLA_SKILLS ({"rule": [...]}), or "" when the routine has no rules.
func (j *Routine) SkillsJSON() string {
	if len(j.Skills.Rule) == 0 {
		return ""
	}
	b, _ := json.Marshal(map[string]any{"rule": j.Skills.Rule})
	return string(b)
}

type Run struct {
	Command   string `yaml:"command"`
	Workdir   string `yaml:"workdir"`
	Timeout   string `yaml:"timeout"`
	SpendFile string `yaml:"spend_file"`
}

type Budget struct {
	DayUSD float64 `yaml:"day_usd"`
}

// ID is fleet/routine, the key everything else uses.
func (j *Routine) ID() string { return j.Fleet + "/" + j.Routine }

// Load reads every routine under dir: the flat fleets/<fleet>/<routine>.yaml and the folder
// fleets/<fleet>/<routine>/routine.yaml with its v<n>/ versions (version.go). Any invalid spec or
// version fails the whole load, so a bad spec stops a deploy instead of silently dropping a routine.
func Load(dir string) ([]*Routine, error) {
	routines, err := LoadVersions(dir)
	if err != nil {
		return nil, err
	}
	var errs []error
	for _, j := range routines {
		for _, v := range j.Versions {
			if v.Err != nil {
				errs = append(errs, v.Err)
			}
		}
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return routines, nil
}

type loadedRoutine struct {
	j *Routine
	p string
}

// LoadVersions is Load that keeps a folder routine's invalid versions (Version.Err) instead of
// failing on them, so the caller can tell a retired one from a live one. Everything else that is
// wrong still fails. Each routine is bound to its lowest valid version.
func LoadVersions(dir string) ([]*Routine, error) {
	var found []loadedRoutine
	var errs []error
	flat, err := filepath.Glob(filepath.Join(dir, "*", "*.yaml"))
	if err != nil {
		return nil, err
	}
	sort.Strings(flat)
	for _, p := range flat {
		if filepath.Base(p) == FleetFile {
			continue // the fleet's own description (LoadFleets), not a routine
		}
		j, err := LoadFile(p)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if want := filepath.Base(filepath.Dir(p)); j.Fleet != want {
			errs = append(errs, fmt.Errorf("%s: fleet %q, but the file is under %s/", p, j.Fleet, want))
			continue
		}
		if want := strings.TrimSuffix(filepath.Base(p), ".yaml"); j.Routine != want {
			errs = append(errs, fmt.Errorf("%s: routine %q, but the file is %s.yaml", p, j.Routine, want))
			continue
		}
		found = append(found, loadedRoutine{j, p})
	}
	folders, err := filepath.Glob(filepath.Join(dir, "*", "*", RoutineFile))
	if err != nil {
		return nil, err
	}
	sort.Strings(folders)
	for _, p := range folders {
		j, err := loadFolder(p)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		found = append(found, loadedRoutine{j, p})
	}
	var routines []*Routine
	seen := map[string]string{}
	for _, f := range found {
		j, p := f.j, f.p
		if prev, dup := seen[j.ID()]; dup {
			errs = append(errs, fmt.Errorf("%s: %s already defined in %s", p, j.ID(), prev))
			continue
		}
		seen[j.ID()] = p
		if k := j.OnCase(); k != "" {
			key := "case:" + k + "/" + j.OnStage()
			if prev, dup := seen[key]; dup {
				errs = append(errs, fmt.Errorf("%s: %s cases at stage %q already have an acting routine in %s", p, k, j.OnStage(), prev))
				continue
			}
			seen[key] = p
		}
		if v := j.OnCheck(); v != "" {
			if prev, dup := seen["check:"+v]; dup {
				errs = append(errs, fmt.Errorf("%s: %s already has a check routine in %s", p, v, prev))
				continue
			}
			seen["check:"+v] = p
		}
		if a := j.OnExecute(); a != "" {
			if prev, dup := seen["execute:"+a]; dup {
				errs = append(errs, fmt.Errorf("%s: %s already has an executing routine in %s", p, a, prev))
				continue
			}
			seen["execute:"+a] = p
		}
		routines = append(routines, j)
	}
	sort.SliceStable(routines, func(a, b int) bool { return routines[a].Path < routines[b].Path })
	errs = append(errs, checkChains(routines)...)
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	if len(routines) == 0 {
		return nil, fmt.Errorf("no routine specs under %s", dir)
	}
	return routines, nil
}

// FleetFile is a fleet's own description beside its routines: fleets/<fleet>/fleet.yaml.
const FleetFile = "fleet.yaml"

// Fleet is what a fleet says about itself (G6): its name and what it is for, for people. Pack is the
// hash of its routines' specs, so a change to any routine's text changes it.
type Fleet struct {
	Fleet       string `yaml:"fleet"`
	Description string `yaml:"description"`
	Pack        string `yaml:"-"`
}

// LoadFleets reads every fleets/<fleet>/fleet.yaml under dir (a fleet without one has none) and the
// pack hash of each fleet's routines. Unknown fields and a name other than the folder's are errors.
func LoadFleets(dir string, routines []*Routine) (map[string]Fleet, error) {
	out := map[string]Fleet{}
	paths, err := filepath.Glob(filepath.Join(dir, "*", FleetFile))
	if err != nil {
		return nil, err
	}
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		dec := yaml.NewDecoder(bytes.NewReader(raw))
		dec.KnownFields(true)
		var f Fleet
		if err := dec.Decode(&f); err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		if want := filepath.Base(filepath.Dir(p)); f.Fleet != want {
			return nil, fmt.Errorf("%s: fleet %q, but the file is under %s/", p, f.Fleet, want)
		}
		f.Description = strings.Join(strings.Fields(f.Description), " ")
		out[f.Fleet] = f
	}
	byFleet := map[string][]string{}
	for _, j := range routines {
		byFleet[j.Fleet] = append(byFleet[j.Fleet], j.Routine+"="+j.Hash)
	}
	for name, hs := range byFleet {
		sort.Strings(hs)
		sum := sha256.Sum256([]byte(strings.Join(hs, "\n")))
		f := out[name]
		f.Fleet, f.Pack = name, hex.EncodeToString(sum[:])
		out[name] = f
	}
	return out, nil
}

// LoadFile parses and validates one spec. Unknown fields are errors: a typo must not become a
// silently ignored setting.
func LoadFile(path string) (*Routine, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	var j Routine
	if err := dec.Decode(&j); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	j.normalizeLevels()
	if err := j.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	sum := sha256.Sum256(raw)
	if len(j.Prompts) > 0 {
		// a changed prompt is a changed routine: its hash covers the prompt files. A routine without prompts keeps
		// the hash of its spec bytes alone, so the fleet's pack hash does not move for it.
		j.PromptSHA = map[string]string{}
		h := sha256.New()
		h.Write(raw)
		for _, role := range sortedKeys(j.Prompts) {
			if b, err := os.ReadFile(filepath.Join(filepath.Dir(path), filepath.FromSlash(j.Prompts[role]))); err == nil {
				ps := sha256.Sum256(b)
				j.PromptSHA[role] = hex.EncodeToString(ps[:])
			}
			fmt.Fprintf(h, "\nprompt %s=%s", role, j.PromptSHA[role])
		}
		sum = [32]byte(h.Sum(nil))
	}
	j.Path, j.Hash = path, hex.EncodeToString(sum[:])[:12]
	j.legacyVersion(filepath.Dir(filepath.Dir(path)), raw)
	return &j, nil
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// validPromptPath: a clean path relative to the fleet's folder, a .md or .txt file, no way out of the folder.
func validPromptPath(p string) bool {
	if !promptPathRe.MatchString(p) || path.Clean(p) != p || strings.HasPrefix(p, "/") || p == ".." || strings.HasPrefix(p, "../") {
		return false
	}
	ext := path.Ext(p)
	return ext == ".md" || ext == ".txt"
}

func (j *Routine) validate() error { return errors.Join(j.validateErrs()...) }

// durField parses a duration field, reporting a bad or too-short value.
func durField(bad func(string, ...any), field, s string, min time.Duration) time.Duration {
	d, err := time.ParseDuration(s)
	if err != nil {
		bad("%s %q: a duration such as 10m", field, s)
		return 0
	}
	if d < min {
		bad("%s %s: at least %s", field, d, min)
	}
	return d
}

// validateRoutineErrs checks what the routine is whichever version runs (identity, autonomy, budget, trigger,
// freshness, ...) and sets Every and Fresh. A folder routine runs it once on routine.yaml, so a broken or
// retired version cannot leave them unset.
func (j *Routine) validateRoutineErrs() []error {
	var errs []error
	bad := func(f string, a ...any) { errs = append(errs, fmt.Errorf(f, a...)) }
	j.validateRoutine(bad)
	return errs
}

func (j *Routine) validateRoutine(bad func(string, ...any)) {
	if !nameRe.MatchString(j.Fleet) {
		bad("fleet %q: lower-case letters, digits and dashes", j.Fleet)
	}
	if !nameRe.MatchString(j.Routine) {
		bad("routine %q: lower-case letters, digits and dashes", j.Routine)
	}
	if strings.TrimSpace(j.Description) == "" {
		bad("description is required")
	}
	if !nameRe.MatchString(j.Worker) {
		bad("worker %q: lower-case letters, digits and dashes", j.Worker)
	}
	if !levels[j.Autonomy] {
		bad("autonomy %q: one of record, propose, auto", j.Autonomy)
	}
	if j.MaxAutonomy != "" && !levels[j.MaxAutonomy] {
		bad("max_autonomy %q: one of record, propose, auto", j.MaxAutonomy)
	} else if levels[j.Autonomy] && !j.Allows(j.Autonomy) {
		bad("autonomy %s is above max_autonomy %s", j.Autonomy, j.MaxAutonomy)
	}
	seenKind := map[string]bool{}
	for _, k := range j.Acceptance {
		if !AcceptanceKinds[k] {
			bad("acceptance %q: one of pairs, replay, scenario_eval", k)
		} else if seenKind[k] {
			bad("acceptance %q is listed twice", k)
		}
		seenKind[k] = true
	}
	if j.Budget.DayUSD < 0 {
		bad("budget.day_usd %.2f: must not be negative", j.Budget.DayUSD)
	}
	if j.ShadowWorker != "" && !nameRe.MatchString(j.ShadowWorker) {
		bad("shadow_worker %q: lower-case letters, digits and dashes", j.ShadowWorker)
	}
	seenState := map[string]bool{}
	for _, p := range j.State {
		switch {
		case !statePathRe.MatchString(p) || strings.Contains(p, "..") || strings.Contains(p, "//") || strings.HasSuffix(p, "/"):
			bad("state %q: a path under the worker's HOME, no .., no leading or trailing slash", p)
		case seenState[p]:
			bad("state %q is listed twice", p)
		}
		seenState[p] = true
	}
	if j.Repo != nil {
		if !repoNameRe.MatchString(j.Repo.Name) {
			bad("repo.name %q: owner/name", j.Repo.Name)
		}
		if !branchRe.MatchString(j.Repo.Branch) || strings.Contains(j.Repo.Branch, "..") {
			bad("repo.branch %q: a branch name", j.Repo.Branch)
		}
	}
	j.validateResume(bad)
	n := 0
	for _, set := range []bool{j.Trigger.Every != "", j.Trigger.Case != nil, j.Trigger.Execute != nil, j.Trigger.Check != nil} {
		if set {
			n++
		}
	}
	switch {
	case n > 1:
		bad("trigger: one of every, case, execute and check")
	case j.Trigger.Case != nil:
		if !caseFieldRe.MatchString(j.Trigger.Case.Kind) {
			bad("trigger.case.kind %q: a case kind such as failed_backup", j.Trigger.Case.Kind)
		}
		if st := j.Trigger.Case.Stage; st != "" && !caseFieldRe.MatchString(st) {
			bad("trigger.case.stage %q: a stage name such as clarify", st)
		}
		if r := j.Trigger.Case.OnRecord; r != "" && r != OnRecordClose && r != OnRecordWatch {
			bad("trigger.case.on_record %q: close or watch", r)
		}
		if l := j.Trigger.Case.Limit; l != nil {
			if j.Trigger.Case.Stage != "" {
				bad("trigger.case.limit: declared on the kind's first-stage routine, not on stage %q", j.Trigger.Case.Stage)
			}
			if l.USD <= 0 || l.Minutes <= 0 {
				bad("trigger.case.limit: usd and minutes both above 0")
			}
		}
	case j.Trigger.Execute != nil:
		if !actionRe.MatchString(j.Trigger.Execute.Action) {
			bad("trigger.execute.action %q: a registry action such as github.open_pr", j.Trigger.Execute.Action)
		}
	case j.Trigger.Check != nil:
		if !verifyRe.MatchString(j.Trigger.Check.Verify) {
			bad("trigger.check.verify %q: a registry check condition such as backup_newer_than_execution", j.Trigger.Check.Verify)
		}
	default:
		j.Every = durField(bad, "trigger.every", j.Trigger.Every, time.Minute)
	}
	for _, k := range j.Detects {
		if !caseFieldRe.MatchString(k) {
			bad("detects: %q is not a case kind", k)
		}
	}
	if len(j.Detects) > 0 && j.OnEvent() {
		bad("detects: a routine on cases acts on them; a monitor runs on time")
	}
	if j.BriefSection != "" {
		switch {
		case len(j.Detects) == 0:
			bad("brief_section: only a monitor (a routine with detects) reports summary lines")
		case !briefSectionRe.MatchString(j.BriefSection):
			bad("brief_section: %q is not a short title (a capital, then letters and spaces, at most 32)", j.BriefSection)
		case j.BriefSection == "Needs you" || j.BriefSection == "Handled overnight" || j.BriefSection == "Watch" || j.BriefSection == "Decisions":
			bad("brief_section: %q is one of the brief's own sections", j.BriefSection)
		}
	}
	if j.Freshness != "" || !j.OnEvent() { // a routine on cases runs when there is work: no freshness
		j.Fresh = durField(bad, "freshness", j.Freshness, time.Minute)
	}
}

func (j *Routine) validateErrs() []error {
	errs := j.validateRoutineErrs()
	bad := func(f string, a ...any) { errs = append(errs, fmt.Errorf(f, a...)) }
	if strings.TrimSpace(j.Run.Command) == "" {
		bad("run.command is required")
	}
	for i, r := range j.Skills.Rule {
		if len(r.When) == 0 || len(r.Load) == 0 {
			bad("skills.rule[%d]: needs a non-empty when and load", i)
		}
		for k, v := range r.When {
			if !caseFieldRe.MatchString(k) || v == "" {
				bad("skills.rule[%d].when: field %q (lower-case, digits, _) with a non-empty value", i, k)
			}
		}
		for _, n := range r.Load {
			if !skillNameRe.MatchString(n) {
				bad("skills.rule[%d].load: skill name %q: lower-case letters, digits and dashes", i, n)
			}
		}
	}
	if len(j.Skills.OnDemand) > 0 {
		bad("skills.on_demand: on-demand loading comes with RFC-0002 §9 step 4, not yet supported")
	}
	for _, role := range sortedKeys(j.Prompts) {
		if !caseFieldRe.MatchString(role) {
			bad("prompts: role %q (lower-case, digits, _)", role)
		}
		if p := j.Prompts[role]; !validPromptPath(p) {
			bad("prompts.%s: %q is not a .md or .txt path inside the fleet's folder (relative, no ..)", role, p)
		}
	}
	if h := j.Harness; h != nil {
		if h.Protocol != "json" {
			bad("harness.protocol %q: json (native tool calls come later)", h.Protocol)
		}
		if len(h.Tools) == 0 || len(h.Gates) == 0 {
			bad("harness: tools and gates are required")
		}
		for _, n := range append(append([]string{}, h.Tools...), h.Gates...) {
			if !caseFieldRe.MatchString(n) {
				bad("harness: tool or gate name %q (lower-case, digits, _)", n)
			}
		}
		l := h.Limits
		if l.MaxToolCalls < 1 || l.MaxSteps != l.MaxToolCalls+1 || l.ToolCallsPerTurn != 1 {
			bad("harness.limits: max_tool_calls >= 1, max_steps = max_tool_calls + 1, tool_calls_per_turn 1 (got %d, %d, %d)",
				l.MaxToolCalls, l.MaxSteps, l.ToolCallsPerTurn)
		}
		if hk := h.Hooks; hk != nil {
			for _, ph := range []struct {
				phase string
				refs  []HookRef
			}{{"before_tool", hk.BeforeTool}, {"after_tool", hk.AfterTool}} {
				phase, refs := ph.phase, ph.refs
				seen := map[string]bool{}
				for i := range refs {
					r := &refs[i]
					switch {
					case !caseFieldRe.MatchString(r.Name):
						bad("harness.hooks.%s: hook name %q (lower-case, digits, _)", phase, r.Name)
					case seen[r.Name]:
						bad("harness.hooks.%s: hook %q is declared twice", phase, r.Name)
					}
					seen[r.Name] = true
					if r.Mode == "" {
						r.Mode = "enforce"
					}
					if r.Mode != "enforce" && r.Mode != "observe" {
						bad("harness.hooks.%s.%s: mode %q is enforce or observe", phase, r.Name, r.Mode)
					}
				}
			}
		}
		q := h.Quirks
		if !q.BadJSONUsesTurn || !q.EmptyRetry || !q.FinalDecisionCall {
			bad("harness.quirks: bad_json_uses_turn, empty_retry and final_decision_call are true until RFC-0002 §9 step 6")
		}
		if q.ReplyEchoChars < 1 || q.DecisionEchoChars < 1 || q.ResultChars < 1 {
			bad("harness.quirks: reply_echo_chars, decision_echo_chars and result_chars must be positive")
		}
	}
	seenModel := map[string]bool{}
	for _, m := range j.Models {
		if m == "" || seenModel[m] {
			bad("models: %q is empty or named twice", m)
		}
		seenModel[m] = true
	}
	j.validateExplain(bad)
	j.Limit = durField(bad, "run.timeout", j.Run.Timeout, 10*time.Second)
	if j.Every > 0 && j.Limit > j.Every*3 {
		bad("run.timeout %s is more than three intervals (%s)", j.Limit, j.Every)
	}
	if j.Every > 0 && j.Fresh > 0 && j.Fresh < j.Every {
		bad("freshness %s is shorter than the interval %s: the routine would always look stale", j.Fresh, j.Every)
	}
	return errs
}

// PromptRoles lists the roles of the declared prompts, sorted.
func (j *Routine) PromptRoles() []string { return sortedKeys(j.Prompts) }
