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
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Autonomy levels, a lifecycle (ADR-0008): shadow, then propose, then auto. What each allows is the
// routine's business in phase 2: the autonomy is passed to the routine as FLEETD_STAGE, as fleetd did,
// and the triage fleet enforces it.
const (
	AutonomyShadow  = "shadow"
	AutonomyPropose = "propose"
	AutonomyAuto    = "auto"
)

var levels = map[string]bool{AutonomyShadow: true, AutonomyPropose: true, AutonomyAuto: true}

var levelRank = map[string]int{AutonomyShadow: 0, AutonomyPropose: 1, AutonomyAuto: 2}

// FleetdStage is the FLEETD_STAGE value the triage fleet's frozen code reads: its top step is still
// "live" (ADR-0008 keeps that contract until the code is ported).
func FleetdStage(autonomy string) string {
	if autonomy == AutonomyAuto {
		return "live"
	}
	return autonomy
}

// Rank orders the autonomy levels (shadow 0, propose 1, auto 2), so a change can be told apart as
// raising or lowering.
func Rank(autonomy string) int { return levelRank[autonomy] }

// Allows reports whether autonomy s is within the routine's cap (max_autonomy; no cap when it is empty).
func (j *Routine) Allows(s string) bool {
	return j.MaxAutonomy == "" || levelRank[s] <= levelRank[j.MaxAutonomy]
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

	// filled by Load
	Path  string        `yaml:"-"`
	Hash  string        `yaml:"-"`
	Every time.Duration `yaml:"-"`
	Limit time.Duration `yaml:"-"`
	Fresh time.Duration `yaml:"-"`
}

// Trigger is what starts the routine (ADR-0007): time (every) or an event on a case (RFC-0004 §3.7),
// exactly one of them.
type Trigger struct {
	Every string       `yaml:"every"`
	Case  *CaseTrigger `yaml:"case"`
}

// CaseTrigger starts the routine when a case of its kind is handed over: it is that kind's acting
// routine (pipeline v1 step 5). One routine per kind.
type CaseTrigger struct {
	Kind string `yaml:"kind"`
}

// OnCase is the case kind the routine acts on, "" for a routine on time.
func (j *Routine) OnCase() string {
	if j.Trigger.Case == nil {
		return ""
	}
	return j.Trigger.Case.Kind
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
	skillNameRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	caseFieldRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,39}$`)
)

// Harness (RFC-0002 §4) configures the routine's model-driven step: its protocol, the tools offered, the
// gates its decision passes, its limits, and the quirks that keep the first fleet's loop like for like
// until RFC-0002 §9 step 6 removes them one by one.
type Harness struct {
	Protocol string        `yaml:"protocol" json:"protocol"`
	Tools    []string      `yaml:"tools" json:"tools"`
	Gates    []string      `yaml:"gates" json:"gates"`
	Limits   HarnessLimits `yaml:"limits" json:"limits"`
	Quirks   HarnessQuirks `yaml:"quirks" json:"quirks"`
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

// Load reads every fleets/<fleet>/<routine>.yaml under dir. Any invalid spec fails the whole load, so a
// bad spec stops a deploy instead of silently dropping a routine.
func Load(dir string) ([]*Routine, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*", "*.yaml"))
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	var routines []*Routine
	seen := map[string]string{}
	var errs []error
	for _, p := range paths {
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
		if prev, dup := seen[j.ID()]; dup {
			errs = append(errs, fmt.Errorf("%s: %s already defined in %s", p, j.ID(), prev))
			continue
		}
		seen[j.ID()] = p
		if k := j.OnCase(); k != "" {
			if prev, dup := seen["case:"+k]; dup {
				errs = append(errs, fmt.Errorf("%s: %s cases already have an acting routine in %s", p, k, prev))
				continue
			}
			seen["case:"+k] = p
		}
		routines = append(routines, j)
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	if len(routines) == 0 {
		return nil, fmt.Errorf("no routine specs under %s", dir)
	}
	return routines, nil
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
	if err := j.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	sum := sha256.Sum256(raw)
	j.Path, j.Hash = path, hex.EncodeToString(sum[:])[:12]
	return &j, nil
}

func (j *Routine) validate() error {
	var errs []error
	bad := func(f string, a ...any) { errs = append(errs, fmt.Errorf(f, a...)) }
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
	if strings.TrimSpace(j.Run.Command) == "" {
		bad("run.command is required")
	}
	if !levels[j.Autonomy] {
		bad("autonomy %q: one of shadow, propose, auto", j.Autonomy)
	}
	if j.MaxAutonomy != "" && !levels[j.MaxAutonomy] {
		bad("max_autonomy %q: one of shadow, propose, auto", j.MaxAutonomy)
	} else if levels[j.Autonomy] && !j.Allows(j.Autonomy) {
		bad("autonomy %s is above max_autonomy %s", j.Autonomy, j.MaxAutonomy)
	}
	if j.Budget.DayUSD < 0 {
		bad("budget.day_usd %.2f: must not be negative", j.Budget.DayUSD)
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
		q := h.Quirks
		if !q.BadJSONUsesTurn || !q.EmptyRetry || !q.FinalDecisionCall {
			bad("harness.quirks: bad_json_uses_turn, empty_retry and final_decision_call are true until RFC-0002 §9 step 6")
		}
		if q.ReplyEchoChars < 1 || q.DecisionEchoChars < 1 || q.ResultChars < 1 {
			bad("harness.quirks: reply_echo_chars, decision_echo_chars and result_chars must be positive")
		}
	}
	dur := func(field, s string, min time.Duration) time.Duration {
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
	switch {
	case j.Trigger.Case != nil && j.Trigger.Every != "":
		bad("trigger: every or case, not both")
	case j.Trigger.Case != nil:
		if !caseFieldRe.MatchString(j.Trigger.Case.Kind) {
			bad("trigger.case.kind %q: a case kind such as failed_backup", j.Trigger.Case.Kind)
		}
	default:
		j.Every = dur("trigger.every", j.Trigger.Every, time.Minute)
	}
	j.Limit = dur("run.timeout", j.Run.Timeout, 10*time.Second)
	if j.Freshness != "" || j.Trigger.Case == nil { // a routine on cases runs when there is work: no freshness
		j.Fresh = dur("freshness", j.Freshness, time.Minute)
	}
	if j.Every > 0 && j.Limit > j.Every*3 {
		bad("run.timeout %s is more than three intervals (%s)", j.Limit, j.Every)
	}
	if j.Every > 0 && j.Fresh > 0 && j.Fresh < j.Every {
		bad("freshness %s is shorter than the interval %s: the routine would always look stale", j.Fresh, j.Every)
	}
	return errors.Join(errs...)
}
