// Package registry is ADR-0011's registry: the actions (and later the tools and gates) flotilla may
// plan, each named, typed and scoped (RFC-0004 §3.4). A component declares what it runs in a JSON file
// in the repo (RFC-0006 §10 step 4): the host agent's `actions` role in host/registry.json (ADR-0009),
// each worker in worker/<worker>/registry.json. flotillad's image carries its own tag's copies and
// refuses to start on a bad one, like a bad spec.
//
// Not yet (pipeline v1 plan, step 3): the workers' tools and gates, GET /v1/registry on the worker and
// the comparison with flotillad's copy, and the test that keeps a declaration equal to its code. The
// precondition and check names are checked for their shape only; the code that evaluates them (the
// acting job, pipeline v1 step 7) is tested to implement every name its declaration uses.
package registry

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"time"
)

// Duration is a Go duration written as a string ("60m") in the JSON.
type Duration time.Duration

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("a duration is a string such as \"60m\"")
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(time.Duration(d).String()) }

// Param is one parameter of an action. Kind "target_host" takes the case's host (the model cannot
// retarget: policy checks it equals the case's target); "enum" is one of Enum.
type Param struct {
	Kind string   `json:"kind"`
	Enum []string `json:"enum,omitempty"`
}

type Check struct {
	Verify string   `json:"verify"`
	Within Duration `json:"within"`
}

type Limits struct {
	PerTargetPerDay int `json:"per_target_per_day"`
}

// Action is a write an agent may only plan; policy decides it, the host agent's actions role runs it.
type Action struct {
	Name       string           `json:"name"`
	ForKinds   []string         `json:"for_kinds"`
	Params     map[string]Param `json:"params"`
	Risk       string           `json:"risk"`
	Reversible string           `json:"reversible"` // yes | regenerable | no (no is capped at propose)
	Pre        []string         `json:"pre"`
	PreShadow  []string         `json:"pre_shadow,omitempty"` // shadow's lower threshold (RFC-0004 §10)
	Check      Check            `json:"check"`
	Limits     Limits           `json:"limits"`
	Timeout    Duration         `json:"timeout"` // past it, executing becomes unknown (RFC-0004 §3.5)
}

// Declaration is one component's file.
type Declaration struct {
	Component string   `json:"component"`
	Actions   []Action `json:"actions"`
}

// Registry is every loaded declaration.
type Registry struct {
	actions map[string]Action
	from    map[string]string // action -> the file that declared it
}

var (
	actionRe = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)+$`)
	kindRe   = regexp.MustCompile(`^[a-z][a-z0-9_]{0,39}$`)
	condRe   = regexp.MustCompile(`^[a-z][a-z0-9_]*(\([^()]*\))?$`)
)

// Load reads and validates declarations; any error in any file refuses them all, naming each.
func Load(paths ...string) (*Registry, error) {
	r := &Registry{actions: map[string]Action{}, from: map[string]string{}}
	var errs []error
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		var d Declaration
		if err := dec.Decode(&d); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", p, err))
			continue
		}
		if d.Component == "" {
			errs = append(errs, fmt.Errorf("%s: component is required", p))
		}
		for _, a := range d.Actions {
			if err := validAction(a); err != nil {
				errs = append(errs, fmt.Errorf("%s: action %q: %w", p, a.Name, err))
				continue
			}
			if prev, dup := r.from[a.Name]; dup {
				errs = append(errs, fmt.Errorf("%s: action %q is already declared in %s", p, a.Name, prev))
				continue
			}
			r.actions[a.Name], r.from[a.Name] = a, p
		}
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return r, nil
}

func validAction(a Action) error {
	var errs []error
	if !actionRe.MatchString(a.Name) {
		errs = append(errs, errors.New("name must be dotted lowercase words (backup.rerun_observability)"))
	}
	if len(a.ForKinds) == 0 {
		errs = append(errs, errors.New("for_kinds is required: the case kinds it may act on"))
	}
	for _, k := range a.ForKinds {
		if !kindRe.MatchString(k) {
			errs = append(errs, fmt.Errorf("for_kinds: %q is not a case kind", k))
		}
	}
	if len(a.Params) == 0 {
		errs = append(errs, errors.New("params is required"))
	}
	for name, p := range a.Params {
		switch {
		case p.Kind == "target_host" && len(p.Enum) == 0:
		case p.Kind == "enum" && len(p.Enum) > 0:
		default:
			errs = append(errs, fmt.Errorf("param %s: kind is target_host, or enum with values", name))
		}
	}
	switch a.Risk {
	case "low", "medium", "high":
	default:
		errs = append(errs, errors.New("risk is low, medium or high"))
	}
	switch a.Reversible {
	case "yes", "regenerable", "no":
	default:
		errs = append(errs, errors.New("reversible is yes, regenerable or no"))
	}
	if len(a.Pre) == 0 {
		errs = append(errs, errors.New("pre is required: the host re-checks it before it runs"))
	}
	for _, c := range append(append([]string{}, a.Pre...), a.PreShadow...) {
		if !condRe.MatchString(c) {
			errs = append(errs, fmt.Errorf("precondition %q is not name or name(arg)", c))
		}
	}
	if !condRe.MatchString(a.Check.Verify) || a.Check.Within <= 0 {
		errs = append(errs, errors.New("check needs verify (a name) and within (> 0)"))
	}
	if a.Limits.PerTargetPerDay < 1 {
		errs = append(errs, errors.New("limits.per_target_per_day must be at least 1"))
	}
	if a.Timeout <= 0 {
		errs = append(errs, errors.New("timeout must be > 0"))
	}
	return errors.Join(errs...)
}

// Action returns the declared action.
func (r *Registry) Action(name string) (Action, bool) {
	a, ok := r.actions[name]
	return a, ok
}

// Actions lists every action's name, sorted.
func (r *Registry) Actions() []string {
	var out []string
	for n := range r.actions {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// For reports whether the action may act on a case of this kind.
func (a Action) For(kind string) bool {
	for _, k := range a.ForKinds {
		if k == kind {
			return true
		}
	}
	return false
}
