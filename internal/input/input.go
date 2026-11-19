// Package input is the frozen input of a live run (v2 step 2, docs/wip/v2-design.md sections 4 and 6): what flotillad handed the
// run when it started, as the envelope runtime/input.schema.json describes. A shadow version of the routine is given exactly this,
// an eval starts from it, and an operator exports it (with the ledger rows as of its mark and the state snapshot) to a folder
// outside any repository, where it is production data until the installation scrubs it.
package input

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/chipi/flotilla/internal/store"
)

// SchemaVersion is the envelope's version (the schema's "schema_version" const).
const SchemaVersion = store.InputSchemaVersion

// The env keys a case, check or execute run is handed, and the envelope field each one is kept in.
var handedKeys = map[string]string{"FLOTILLA_CASE": "case", "FLOTILLA_PLAN": "plan", "FLOTILLA_CHECK": "check", "FLOTILLA_RESUME": "resume"}

// Commit is the commit flotillad resolved at dispatch for a routine that works on a repository.
type Commit struct {
	Repo string `json:"repo"`
	SHA  string `json:"sha"`
}

// LedgerEntry is one row of the decision ledger.
type LedgerEntry struct {
	Kind string            `json:"kind"`
	Row  map[string]string `json:"row"`
}

// Ledger is the routine's ledger as of the run's start: the high-water marks and, in an export that asked for them, the rows.
type Ledger struct {
	MarkFilings *int64              `json:"mark_filings,omitempty"`
	MarkEntries *int64              `json:"mark_entries,omitempty"`
	Filings     []map[string]string `json:"filings,omitempty"`
	Entries     []LedgerEntry       `json:"entries,omitempty"`
}

// StateSnapshot names the routine's snapshotted state paths and, in an export that asked for it, the archive beside the envelope.
type StateSnapshot struct {
	Paths []string `json:"paths"`
	Tar   string   `json:"tar,omitempty"`
}

// Envelope is the frozen input. The fields only a live capture has are optional, so an input built from a dataset item is one too.
type Envelope struct {
	SchemaVersion int             `json:"schema_version"`
	InputKey      string          `json:"input_key"`
	Routine       string          `json:"routine"`
	Kind          string          `json:"kind"`
	Version       int             `json:"version,omitempty"`
	Trigger       string          `json:"trigger,omitempty"`
	StartedAt     string          `json:"started_at,omitempty"`
	TickTime      string          `json:"tick_time,omitempty"`
	Case          json.RawMessage `json:"case,omitempty"`
	Plan          json.RawMessage `json:"plan,omitempty"`
	Check         json.RawMessage `json:"check,omitempty"`
	Resume        json.RawMessage `json:"resume,omitempty"`
	Commit        *Commit         `json:"commit,omitempty"`
	Ledger        *Ledger         `json:"ledger,omitempty"`
	LiveCaseKeys  *[]string       `json:"live_case_keys,omitempty"`
	StateSnapshot *StateSnapshot  `json:"state_snapshot,omitempty"`
	// RecordedToolResults is reserved for build step 3 and not produced before it.
	RecordedToolResults json.RawMessage `json:"recorded_tool_results,omitempty"`
}

var (
	routineRe = regexp.MustCompile(`^[a-z0-9-]+/[a-z0-9-]+$`)
	timeRe    = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]+)?(Z|[+-][0-9]{2}:[0-9]{2})$`)
	shaRe     = regexp.MustCompile(`^[0-9a-f]{7,64}$`)
	tarRe     = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
)

// Kind reads what kind of run a trigger made: a case, check or execute run is its trigger, and every other run (a schedule, a
// manual start) is a tick.
func Kind(trigger string, handed bool) string {
	switch trigger {
	case store.InputCase, store.InputCheck, store.InputExecute:
		return trigger
	}
	if handed {
		return store.InputCase
	}
	return store.InputTick
}

// Handed is what a run was handed besides its schedule, as the JSON object the store keeps: each of its FLOTILLA_CASE,
// FLOTILLA_PLAN, FLOTILLA_CHECK and FLOTILLA_RESUME that is JSON, under case, plan, check and resume. A value that is not JSON is
// not kept (it cannot be in the envelope), and an env with none gives "{}".
func Handed(env map[string]string) json.RawMessage {
	out := map[string]json.RawMessage{}
	for k, field := range handedKeys {
		raw := strings.TrimSpace(env[k])
		if raw == "" {
			continue
		}
		var obj map[string]any
		if json.Unmarshal([]byte(raw), &obj) == nil && obj != nil {
			out[field] = json.RawMessage(raw)
		}
	}
	b, _ := json.Marshal(out)
	return b
}

// FromStore is the envelope of a recorded input.
func FromStore(in store.Input) (Envelope, error) {
	e := Envelope{SchemaVersion: SchemaVersion, InputKey: in.Key, Routine: in.Routine, Kind: in.Kind, Version: in.Version,
		Trigger: in.Trigger, StartedAt: in.StartedAt}
	var handed map[string]json.RawMessage
	if len(in.Handed) > 0 {
		if err := json.Unmarshal(in.Handed, &handed); err != nil {
			return e, fmt.Errorf("input %s: handed: %w", in.Key, err)
		}
	}
	e.Case, e.Plan, e.Check, e.Resume = handed["case"], handed["plan"], handed["check"], handed["resume"]
	if in.Kind == store.InputTick {
		e.TickTime = in.StartedAt
	}
	mf, me := in.MarkFilings, in.MarkEntries
	e.Ledger = &Ledger{MarkFilings: &mf, MarkEntries: &me}
	keys := in.LiveCaseKeys
	if keys == nil {
		keys = []string{}
	}
	e.LiveCaseKeys = &keys
	if in.CommitSHA != "" {
		e.Commit = &Commit{Repo: in.CommitRepo, SHA: in.CommitSHA}
	}
	if len(in.SnapshotPaths) > 0 {
		e.StateSnapshot = &StateSnapshot{Paths: in.SnapshotPaths}
	}
	return e, nil
}

// WithLedger adds the ledger's rows as of the input's marks: the routine's current filings and its decisions and recurrences, in
// ledger order. They are production data.
func WithLedger(ctx context.Context, st *store.Store, e *Envelope) error {
	if e.Ledger == nil || e.Ledger.MarkFilings == nil || e.Ledger.MarkEntries == nil {
		return errors.New("the input has no ledger marks to read the ledger as of")
	}
	fs, err := st.FilingsAsOf(ctx, e.Routine, "", *e.Ledger.MarkFilings, nil)
	if err != nil {
		return err
	}
	for _, f := range fs {
		e.Ledger.Filings = append(e.Ledger.Filings, f.Row)
	}
	es, err := st.EntriesAsOf(ctx, e.Routine, "", *e.Ledger.MarkEntries, 0, 0)
	if err != nil {
		return err
	}
	for _, x := range es {
		e.Ledger.Entries = append(e.Ledger.Entries, LedgerEntry{Kind: x.Kind, Row: x.Row})
	}
	return nil
}

// Validate says the envelope is valid as runtime/input.schema.json says. The Python checker (runtime/schemacheck.py) is the full
// validator and the Go and Python tests share the golden files; this is the same rules for the fields Go can see.
func (e Envelope) Validate() error {
	var errs []string
	bad := func(format string, a ...any) { errs = append(errs, fmt.Sprintf(format, a...)) }
	if e.SchemaVersion != SchemaVersion {
		bad("schema_version must be %d", SchemaVersion)
	}
	if n := len(e.InputKey); n < 1 || n > 200 {
		bad("input_key is 1 to 200 characters")
	}
	if !routineRe.MatchString(e.Routine) {
		bad("routine %q is not <fleet>/<routine>", e.Routine)
	}
	switch e.Kind {
	case store.InputTick, store.InputCase, store.InputCheck, store.InputExecute:
	default:
		bad("kind %q is not tick, case, check or execute", e.Kind)
	}
	if e.Version < 0 {
		bad("version is 1 or more")
	}
	if len(e.Trigger) > 64 {
		bad("trigger is at most 64 characters")
	}
	for name, v := range map[string]string{"started_at": e.StartedAt, "tick_time": e.TickTime} {
		if v != "" && !timeRe.MatchString(v) {
			bad("%s %q is not an RFC 3339 time", name, v)
		}
	}
	for name, raw := range map[string]json.RawMessage{"case": e.Case, "plan": e.Plan, "check": e.Check, "resume": e.Resume, "recorded_tool_results": e.RecordedToolResults} {
		if len(raw) == 0 {
			continue
		}
		var obj map[string]json.RawMessage
		if json.Unmarshal(raw, &obj) != nil || obj == nil {
			bad("%s must be an object", name)
		}
	}
	if c := e.Commit; c != nil {
		if c.Repo == "" || len(c.Repo) > 200 {
			bad("commit.repo is 1 to 200 characters")
		}
		if !shaRe.MatchString(c.SHA) {
			bad("commit.sha %q is not a commit id", c.SHA)
		}
	}
	if l := e.Ledger; l != nil {
		for name, m := range map[string]*int64{"mark_filings": l.MarkFilings, "mark_entries": l.MarkEntries} {
			if m != nil && *m < 0 {
				bad("ledger.%s is 0 or more", name)
			}
		}
		for i, en := range l.Entries {
			if en.Kind != "decision" && en.Kind != "recurrence" {
				bad("ledger.entries[%d].kind %q is not decision or recurrence", i, en.Kind)
			}
			if en.Row == nil {
				bad("ledger.entries[%d].row is required", i)
			}
		}
	}
	if e.LiveCaseKeys != nil {
		for i, k := range *e.LiveCaseKeys {
			if k == "" {
				bad("live_case_keys[%d] is empty", i)
			}
		}
	}
	if s := e.StateSnapshot; s != nil {
		if len(s.Paths) < 1 {
			bad("state_snapshot.paths needs at least one path")
		}
		for i, p := range s.Paths {
			if p == "" {
				bad("state_snapshot.paths[%d] is empty", i)
			}
		}
		if s.Tar != "" && !tarRe.MatchString(s.Tar) {
			bad("state_snapshot.tar %q is not a file name", s.Tar)
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

// Decode reads an envelope strictly: an unknown field is an error, as the schema's additionalProperties says, and so is anything
// Validate refuses.
func Decode(raw []byte) (Envelope, error) {
	var e Envelope
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&e); err != nil {
		return e, err
	}
	if dec.More() {
		return e, errors.New("more than one JSON value")
	}
	return e, e.Validate()
}
