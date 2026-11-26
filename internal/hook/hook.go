// Package hook is RFC-0014 §4.2's inbound webhook: an outside service (the installation's plumbing tool)
// signs a call that opens one case. What the case is (its kind, title, urgency, project, and which field is
// its target) is fixed in flotillad.yaml's hooks:, never chosen by the payload; the payload is data.
package hook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// The headers a call carries. The signature covers the timestamp, the delivery id and the body, so none of
// them can be swapped onto another call.
const (
	HeaderTimestamp = "X-Flotilla-Timestamp"
	HeaderDelivery  = "X-Flotilla-Delivery"
	HeaderSignature = "X-Flotilla-Signature"
)

// Window is how far a call's timestamp may be from flotillad's clock, either way; a delivery id is kept
// twice as long, so a replay inside the window is always recognised.
const Window = 5 * time.Minute

const (
	defaultMaxBody   = 64 << 10
	maxMaxBody       = 1 << 20
	defaultPerMinute = 30
	maxTitle         = 200
	maxDelivery      = 128
)

var (
	nameRe     = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,39}$`)
	kindRe     = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	fieldRe    = regexp.MustCompile(`^[a-z][a-z0-9_]{0,39}$`)
	deliveryRe = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)
)

// Field is one payload field a hook takes. A text is at most Max characters; an enum is one of Enum.
type Field struct {
	Type string   `yaml:"type"`
	Max  int      `yaml:"max"`
	Enum []string `yaml:"enum"`
}

// Asker is the field a hook's sender is in, and the directory tool its ids belong to.
type Asker struct {
	Field string `yaml:"field"`
	Tool  string `yaml:"tool"`
}

// The case-data keys flotillad writes for a hook with an asker: the person's email, or Unknown, and their name. No hook may
// declare a field with either name, so the payload never sets them.
const (
	AskerKey     = "asker"
	AskerNameKey = "asker_name"
	Unknown      = "unknown"
)

var toolRe = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

// Hook is one entry of flotillad.yaml's hooks:.
type Hook struct {
	Name       string `yaml:"name"`
	SecretFile string `yaml:"secret_file"`
	// The case it opens: everything here is the hook's, never the payload's.
	Kind    string `yaml:"kind"`
	Title   string `yaml:"title"`
	Urgency int    `yaml:"urgency"`
	Project string `yaml:"project"`
	// Target names the field that is the case's target, checked against TargetPattern.
	Target        string           `yaml:"target"`
	TargetPattern string           `yaml:"target_pattern"`
	Fields        map[string]Field `yaml:"fields"`
	MaxBody       int              `yaml:"max_body"`
	PerMinute     int              `yaml:"per_minute"`
	// CloseOnArrival closes the case as soon as it opens: the smoke hook, which proves the path end to end
	// without leaving work for a routine.
	CloseOnArrival bool `yaml:"close_on_arrival"`
	// Asker names the field holding who sent the call, as an id in one of the directory's tools (RFC-0014 case 1): flotillad
	// looks it up when the case opens and writes the answer into the case's data as AskerKey and AskerNameKey.
	Asker *Asker `yaml:"asker"`

	secret   []byte
	targetRe *regexp.Regexp
	mu       sync.Mutex
	window   time.Time
	calls    int
}

// Check validates every hook and reads its secret; the errors name the hook.
func Check(hooks []*Hook) []error {
	var errs []error
	seen := map[string]bool{}
	for _, h := range hooks {
		if err := h.check(); err != nil {
			errs = append(errs, fmt.Errorf("hooks: %s: %w", h.Name, err))
			continue
		}
		if seen[h.Name] {
			errs = append(errs, fmt.Errorf("hooks: %s: declared twice", h.Name))
		}
		seen[h.Name] = true
	}
	return errs
}

func (h *Hook) check() error {
	switch {
	case !nameRe.MatchString(h.Name):
		return errors.New("name: lower case letters, digits, - and _, starting with a letter, at most 40")
	case !kindRe.MatchString(h.Kind):
		return errors.New("kind: a case kind (lower case letters, digits and _)")
	case strings.TrimSpace(h.Title) == "" || len(h.Title) > maxTitle || strings.ContainsAny(h.Title, "\r\n"):
		return fmt.Errorf("title: one line of at most %d characters (the case's title is the hook's wording, never the payload's)", maxTitle)
	case h.Urgency < 0 || h.Urgency > 3:
		return errors.New("urgency: 0 to 3")
	case h.MaxBody < 0 || h.MaxBody > maxMaxBody:
		return fmt.Errorf("max_body: 0 (the default, %d) to %d bytes", defaultMaxBody, maxMaxBody)
	case h.PerMinute < 0 || h.PerMinute > 600:
		return fmt.Errorf("per_minute: 0 (the default, %d) to 600", defaultPerMinute)
	case len(h.Fields) == 0:
		return errors.New("fields: at least one, the target's")
	}
	for name, f := range h.Fields {
		if !fieldRe.MatchString(name) {
			return fmt.Errorf("fields: %q: lower case letters, digits and _", name)
		}
		switch f.Type {
		case "text":
			if f.Max < 1 || f.Max > maxMaxBody {
				return fmt.Errorf("fields: %s: a text needs max, 1 to %d", name, maxMaxBody)
			}
		case "enum":
			if len(f.Enum) == 0 {
				return fmt.Errorf("fields: %s: an enum needs its values", name)
			}
		default:
			return fmt.Errorf("fields: %s: type is text or enum", name)
		}
	}
	if _, ok := h.Fields[h.Target]; !ok {
		return fmt.Errorf("target: %q is not one of the fields", h.Target)
	}
	if h.Asker != nil {
		if _, ok := h.Fields[h.Asker.Field]; !ok {
			return fmt.Errorf("asker: field %q is not one of the fields", h.Asker.Field)
		}
		if !toolRe.MatchString(h.Asker.Tool) {
			return fmt.Errorf("asker: tool %q: a directory tool, a short lowercase word", h.Asker.Tool)
		}
		for _, k := range []string{AskerKey, AskerNameKey} {
			if _, ok := h.Fields[k]; ok {
				return fmt.Errorf("fields: %s is reserved for the asker flotillad resolves", k)
			}
		}
	}
	if h.TargetPattern == "" {
		return errors.New("target_pattern: required (a target reaches the case's key and its pages)")
	}
	re, err := regexp.Compile(`^(?:` + h.TargetPattern + `)$`)
	if err != nil {
		return fmt.Errorf("target_pattern: %v", err)
	}
	h.targetRe = re
	secret, err := readSecret(h.SecretFile)
	if err != nil {
		return fmt.Errorf("secret_file: %w", err)
	}
	h.secret = secret
	return nil
}

func readSecret(path string) ([]byte, error) {
	if path == "" {
		return nil, errors.New("required")
	}
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s is mode %o: it must be readable by its owner only", path, fi.Mode().Perm())
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	s := strings.TrimSpace(string(raw))
	if len(s) < 32 {
		return nil, fmt.Errorf("%s: a secret of at least 32 characters", path)
	}
	return []byte(s), nil
}

// MaxBodyBytes is the body cap the handler reads up to.
func (h *Hook) MaxBodyBytes() int64 {
	if h.MaxBody == 0 {
		return defaultMaxBody
	}
	return int64(h.MaxBody)
}

// Allow is the hook's rate: at most PerMinute calls in a fixed minute, counted before the signature is
// checked, so a flood of bad calls is cut off as cheaply as good ones.
func (h *Hook) Allow(now time.Time) bool {
	limit := h.PerMinute
	if limit == 0 {
		limit = defaultPerMinute
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if m := now.Truncate(time.Minute); !m.Equal(h.window) {
		h.window, h.calls = m, 0
	}
	if h.calls >= limit {
		return false
	}
	h.calls++
	return true
}

// Sign is the signature a call carries: hex HMAC-SHA256 over "timestamp.delivery.body", prefixed sha256=.
func Sign(secret []byte, timestamp, delivery string, body []byte) string {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(timestamp + "." + delivery + "."))
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

// Verify checks a call's signature and its timestamp's window. It returns the reason a call is refused;
// the reason is recorded, never sent back beyond a status.
func (h *Hook) Verify(timestamp, delivery, signature string, body []byte, now time.Time) error {
	if !deliveryRe.MatchString(delivery) {
		return errors.New("no delivery id, or not 1-128 of A-Z a-z 0-9 . _ : -")
	}
	sec, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return errors.New("no timestamp, or not unix seconds")
	}
	if d := now.Sub(time.Unix(sec, 0)); d > Window || d < -Window {
		return fmt.Errorf("timestamp outside the %s window", Window)
	}
	if !hmac.Equal([]byte(signature), []byte(Sign(h.secret, timestamp, delivery, body))) {
		return errors.New("bad signature")
	}
	return nil
}

// Payload is what a verified body becomes: the target, and the declared fields only, credentials redacted.
type Payload struct {
	Target string
	Data   map[string]string
	// Ignored are the body's keys the hook does not declare, by name only: what the caller tried to set.
	Ignored []string
}

// Parse reads a verified body: a JSON object whose declared fields are strings of their type. A declared
// field that is missing is empty, except the target, which must match its pattern.
func (h *Hook) Parse(body []byte) (*Payload, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil || raw == nil {
		return nil, errors.New("the body is not a JSON object")
	}
	p := &Payload{Data: map[string]string{}}
	for k := range raw {
		if _, ok := h.Fields[k]; !ok {
			p.Ignored = append(p.Ignored, k)
		}
	}
	sort.Strings(p.Ignored)
	for name, f := range h.Fields {
		v, ok := raw[name]
		if !ok {
			continue
		}
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			return nil, fmt.Errorf("%s: not a string", name)
		}
		switch f.Type {
		case "text":
			if !utf8.ValidString(s) || utf8.RuneCountInString(s) > f.Max {
				return nil, fmt.Errorf("%s: over %d characters, or not UTF-8", name, f.Max)
			}
		case "enum":
			found := false
			for _, e := range f.Enum {
				found = found || e == s
			}
			if !found {
				return nil, fmt.Errorf("%s: not one of its values", name)
			}
		}
		p.Data[name] = s
	}
	p.Target = p.Data[h.Target]
	if !h.targetRe.MatchString(p.Target) {
		return nil, fmt.Errorf("%s: the target does not match its pattern", h.Target)
	}
	for k, v := range p.Data {
		p.Data[k] = RedactCredentials(v)
	}
	return p, nil
}

// The credential rules of runtime/toolrec.py (_CREDENTIAL_RULES), in Go: keys and tokens, bearer and basic,
// URL credentials, NAME=value and "name": "value" for secret names. A payload is data a model may read and
// a person sees on the case's page; a credential pasted into a meeting note must not reach either.
var credentialRules = []struct {
	re   *regexp.Regexp
	repl string
}{
	{regexp.MustCompile(`sk-or-v1-[A-Za-z0-9]{16,}|sk-[A-Za-z0-9_-]{20,}|gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}` +
		`|glsa_[A-Za-z0-9_]{16,}|AKIA[0-9A-Z]{16}|xox[abprs]-[A-Za-z0-9-]{10,}` +
		`|eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+`), Mask},
	{regexp.MustCompile(`(?i)(authorization\s*[:=]\s*)(?:(?:bearer|basic|token)\s+)?[^\s'"]+`), "${1}" + Mask},
	{regexp.MustCompile(`(?i)\b(bearer|basic)\s+[A-Za-z0-9._~+/=-]{8,}`), "${1} " + Mask},
	{regexp.MustCompile(`(://)[^/\s:@'"]+:[^/\s@'"]+@`), "${1}" + Mask + "@"},
	{regexp.MustCompile(`(?i)(--?(?:token|password|passwd|secret|api-?key)(?:=|\s+))(?:"[^"]*"|'[^']*'|[^\s'"]+)`), "${1}" + Mask},
	{regexp.MustCompile(`(?i)("[^"]*(?:key|token|secret|passw(?:or)?d|credential)[^"]*"\s*:\s*)"[^"]*"`), `${1}"` + Mask + `"`},
	{regexp.MustCompile(`(?i)((?:key|token|secret|passw(?:or)?d|credential)[A-Za-z0-9_.-]{0,64}=)(?:"[^"]*"|'[^']*'|[^\s'"&;]+)`), "${1}" + Mask},
}

// Mask replaces a credential.
const Mask = "<redacted>"

// RedactCredentials masks the credentials in s.
func RedactCredentials(s string) string {
	for _, r := range credentialRules {
		s = r.re.ReplaceAllString(s, r.repl)
	}
	return s
}
