// Package hostcfg is a host's own config for its host agent (ADR-0006 §5): what this host allows, how
// often, and up to which autonomy, whatever flotillad signs (security T14). It is root-owned on the host,
// reviewed in the installation's private git, and changed only by a reviewed deploy. The sudoers lines
// the agent's user needs are generated from it (Sudoers), never written by hand.
//
// An action is an exact argv. A parameter replaces one whole argv element ("{name}"), never part of one,
// and nothing runs through a shell. A privileged command (argv starting with /usr/bin/sudo) takes only
// enum parameters: the generator writes one sudoers line per allowed value, with absolute paths, and
// refuses any element sudoers would read as a wildcard or a special character. A pattern parameter is
// allowed only for an unprivileged command, which the agent checks itself.
package hostcfg

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Sudo is the only privileged entry point: argv[0] of a privileged command.
const Sudo = "/usr/bin/sudo"

type Duration time.Duration

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("a duration is a string such as \"30m\"")
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

// Param is a parameter of an action on this host. "this_host": the execution names this host (it is
// never substituted into argv). "enum": one of Values. "pattern": matches Pattern, unprivileged only.
type Param struct {
	Kind    string   `json:"kind"`
	Values  []string `json:"values,omitempty"`
	Pattern string   `json:"pattern,omitempty"`
}

// PreKinds are the precondition kinds the agent can check (internal/hostexec), each with its exact
// arguments; a config naming another is refused at load, not found out at execution.
var PreKinds = map[string][]string{
	"launchd_not_running": {"label"},
}

// Pre is a local precondition the agent checks before it runs (ADR-0006 §7).
type Pre struct {
	Kind string            `json:"kind"`
	Args map[string]string `json:"args,omitempty"`
}

type Action struct {
	Argv    []string         `json:"argv"`
	Params  map[string]Param `json:"params"`
	PerHour int              `json:"per_hour"`
	Timeout Duration         `json:"timeout"`
	Pre     []Pre            `json:"pre"`
}

// Config is the whole file.
type Config struct {
	Host        string            `json:"host"`
	User        string            `json:"user"`         // the agent's own user (_flotilla on macOS)
	Ceiling     string            `json:"ceiling"`      // the highest autonomy this host accepts
	SigningKeys map[string]string `json:"signing_keys"` // key id -> base64 Ed25519 public key (two during a rotation)
	Actions     map[string]Action `json:"actions"`

	keys map[string]ed25519.PublicKey
}

var (
	nameRe   = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	userRe   = regexp.MustCompile(`^_?[a-z][a-z0-9_-]{0,31}$`)
	actionRe = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)+$`)
	keyIDRe  = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)
	placeRe  = regexp.MustCompile(`^\{([a-z][a-z0-9_]*)\}$`)
	// what a privileged argv element (and an enum value reaching one) may hold: no sudoers wildcard
	// (* ? [ ] !), no separator or escape (, : = \), no whitespace, no quotes
	safeRe = regexp.MustCompile(`^[A-Za-z0-9._/@%+-]+$`)
)

// Load reads and validates a host's config; every problem is named.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(raw)
}

func Parse(raw []byte) (*Config, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var c Config
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("host config: %w", err)
	}
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("host config: %w", err)
	}
	return &c, nil
}

func (c *Config) validate() error {
	var errs []error
	bad := func(f string, a ...any) { errs = append(errs, fmt.Errorf(f, a...)) }
	if !nameRe.MatchString(c.Host) {
		bad("host %q: a lowercase name such as mini", c.Host)
	}
	if !userRe.MatchString(c.User) || c.User == "root" {
		bad("user %q: the agent's own user, not root", c.User)
	}
	switch c.Ceiling {
	case "shadow", "propose", "auto":
	default:
		bad("ceiling must be shadow, propose or auto")
	}
	if len(c.SigningKeys) == 0 || len(c.SigningKeys) > 2 {
		bad("signing_keys: one, or two during a rotation")
	}
	c.keys = map[string]ed25519.PublicKey{}
	for id, b64 := range c.SigningKeys {
		k, err := base64.StdEncoding.DecodeString(b64)
		if !keyIDRe.MatchString(id) || err != nil || len(k) != ed25519.PublicKeySize {
			bad("signing key %q: an id and a base64 Ed25519 public key", id)
			continue
		}
		c.keys[id] = ed25519.PublicKey(k)
	}
	if len(c.Actions) == 0 {
		bad("actions: none allowed")
	}
	for name, a := range c.Actions {
		if err := validAction(name, a); err != nil {
			errs = append(errs, fmt.Errorf("action %s: %w", name, err))
		}
	}
	return errors.Join(errs...)
}

// Privileged reports whether the action runs through sudo.
func (a Action) Privileged() bool { return len(a.Argv) > 0 && a.Argv[0] == Sudo }

func validAction(name string, a Action) error {
	var errs []error
	bad := func(f string, x ...any) { errs = append(errs, fmt.Errorf(f, x...)) }
	if !actionRe.MatchString(name) {
		bad("the name must be dotted lowercase words")
	}
	if len(a.Argv) == 0 || !strings.HasPrefix(a.Argv[0], "/") {
		bad("argv[0] must be an absolute path")
	}
	priv := a.Privileged()
	if priv {
		// sudo -n [-u USER] /absolute/command ...: nothing else before the command
		rest := a.Argv[1:]
		if len(rest) == 0 || rest[0] != "-n" {
			bad("a privileged command is %s -n [-u user] /absolute/command ...", Sudo)
		} else {
			rest = rest[1:]
			if len(rest) >= 2 && rest[0] == "-u" {
				if !userRe.MatchString(rest[1]) {
					bad("sudo -u %q: a user name", rest[1])
				}
				rest = rest[2:]
			}
			if len(rest) == 0 || !strings.HasPrefix(rest[0], "/") {
				bad("the command after sudo must be an absolute path")
			}
		}
	}
	used := map[string]bool{}
	for i, el := range a.Argv {
		if m := placeRe.FindStringSubmatch(el); m != nil {
			used[m[1]] = true
			p, ok := a.Params[m[1]]
			switch {
			case !ok:
				bad("argv[%d] names parameter %s, which is not declared", i, m[1])
			case p.Kind == "this_host":
				bad("argv[%d]: a this_host parameter is checked, never substituted", i)
			}
			continue
		}
		if strings.ContainsAny(el, "{}") {
			bad("argv[%d] %q: a parameter is a whole element, {name}", i, el)
		}
		if priv && !safeRe.MatchString(el) {
			bad("argv[%d] %q: a privileged element holds no wildcard, separator, space or quote", i, el)
		}
	}
	for pname, p := range a.Params {
		switch p.Kind {
		case "this_host":
		case "enum":
			if len(p.Values) == 0 {
				bad("parameter %s: an enum needs values", pname)
			}
			for _, v := range p.Values {
				if !safeRe.MatchString(v) {
					bad("parameter %s value %q: no wildcard, separator, space or quote", pname, v)
				}
			}
		case "pattern":
			if priv {
				bad("parameter %s: a pattern never reaches a privileged command", pname)
			}
			if _, err := regexp.Compile("^(?:" + p.Pattern + ")$"); err != nil || p.Pattern == "" {
				bad("parameter %s: a pattern that compiles", pname)
			}
		default:
			bad("parameter %s: kind is this_host, enum or pattern", pname)
		}
		if p.Kind != "this_host" && !used[pname] {
			bad("parameter %s is declared but not used in argv", pname)
		}
	}
	if a.PerHour < 1 {
		bad("per_hour must be at least 1")
	}
	if a.Timeout <= 0 {
		bad("timeout must be > 0")
	}
	for _, p := range a.Pre {
		args, ok := PreKinds[p.Kind]
		if !ok {
			bad("precondition kind %q is not one the agent can check", p.Kind)
			continue
		}
		if len(p.Args) != len(args) {
			bad("precondition %s takes %v", p.Kind, args)
		}
		for _, k := range args {
			if p.Args[k] == "" {
				bad("precondition %s needs %s", p.Kind, k)
			}
		}
	}
	return errors.Join(errs...)
}

// Keys are the accepted execution-signing keys, by id.
func (c *Config) Keys() map[string]ed25519.PublicKey { return c.keys }

// Argv resolves an action's argv for an execution's parameters, checking each against its declaration;
// it is the only way the agent builds a command. The execution must name exactly the declared parameters.
func (c *Config) Argv(action string, params map[string]string) ([]string, error) {
	a, ok := c.Actions[action]
	if !ok {
		return nil, fmt.Errorf("%s is not allowed on this host", action)
	}
	for name := range params {
		if _, ok := a.Params[name]; !ok {
			return nil, fmt.Errorf("parameter %s is not declared for %s", name, action)
		}
	}
	for name, p := range a.Params {
		v, ok := params[name]
		if !ok {
			return nil, fmt.Errorf("parameter %s is missing", name)
		}
		switch p.Kind {
		case "this_host":
			if v != c.Host {
				return nil, fmt.Errorf("parameter %s=%q: this is %s", name, v, c.Host)
			}
		case "enum":
			if !contains(p.Values, v) {
				return nil, fmt.Errorf("parameter %s=%q is not one of %v", name, v, p.Values)
			}
		case "pattern":
			if !regexp.MustCompile("^(?:" + p.Pattern + ")$").MatchString(v) {
				return nil, fmt.Errorf("parameter %s=%q does not match", name, v)
			}
		}
	}
	out := make([]string, len(a.Argv))
	for i, el := range a.Argv {
		if m := placeRe.FindStringSubmatch(el); m != nil {
			out[i] = params[m[1]]
		} else {
			out[i] = el
		}
	}
	return out, nil
}

// Sudoers writes the lines the agent's user needs: one per privileged action and combination of enum
// values (capped), absolute paths, no wildcard (validation refused any). Check the result with
// `visudo -c -f` before installing it.
func (c *Config) Sudoers() (string, error) {
	var lines []string
	names := make([]string, 0, len(c.Actions))
	for n := range c.Actions {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		a := c.Actions[name]
		if !a.Privileged() {
			continue
		}
		runAs, cmd := "root", a.Argv[2:]
		if len(cmd) >= 2 && cmd[0] == "-u" {
			runAs, cmd = cmd[1], cmd[2:]
		}
		combos := [][]string{{}}
		for _, el := range cmd {
			vals := []string{el}
			if m := placeRe.FindStringSubmatch(el); m != nil {
				vals = a.Params[m[1]].Values
			}
			var next [][]string
			for _, cmb := range combos {
				for _, v := range vals {
					next = append(next, append(append([]string{}, cmb...), v))
				}
			}
			combos = next
			if len(combos) > 64 {
				return "", fmt.Errorf("action %s: more than 64 sudoers lines", name)
			}
		}
		for _, cmb := range combos {
			if len(cmb) == 1 {
				// a command alone in sudoers allows it with ANY arguments; "" allows none
				cmb = append(cmb, `""`)
			}
			lines = append(lines, fmt.Sprintf("%s ALL=(%s) NOPASSWD: %s  # %s", c.User, runAs, strings.Join(cmb, " "), name))
		}
	}
	var b strings.Builder
	b.WriteString("# Generated by flotilla-host from this host's config (ADR-0006 §5). Do not edit: change the config\n")
	b.WriteString("# by a reviewed deploy and generate again. Check with visudo -c -f before installing.\n")
	for _, l := range lines {
		b.WriteString(l + "\n")
	}
	return b.String(), nil
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}
