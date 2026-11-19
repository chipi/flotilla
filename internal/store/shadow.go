package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Shadows with no effect (v2 step 2, design section 4). A shadow run's rows are keyed "<fleet>/<routine>@v<n>" (runs, calls,
// spend, its ledger namespace) and runs.shadow is 1; nothing a shadow does lands under a live routine's key. What it would
// have written goes to shadow_records. These are the store's pieces: the frozen input of a live run, the queue of shadow runs,
// the records, and the ledger as a shadow sees it.

// liveRuns and liveCalls are the SQL conditions that keep a reader to live rows. Every read of runs, run_calls or spend in this
// package names one of them, IncludeShadow or a "shadow-ok:" comment saying why not (TestEveryReaderSaysWhatItDoesWithShadows).
const (
	liveRuns  = `shadow = 0`
	liveCalls = `instr(routine, '@v') = 0`
)

// ShadowKey is the key of a shadow run's rows: the routine's id and the version, "triage/cycle@v2".
func ShadowKey(routine string, version int) string { return fmt.Sprintf("%s@v%d", routine, version) }

// IsShadowKey reports whether a routine key names a shadow version's rows. A loaded routine's id is [a-z0-9-]+/[a-z0-9-]+, so
// "@v" never appears in a live one.
func IsShadowKey(key string) bool { return strings.Contains(key, "@v") }

// The kinds of run an input is for, and what a snapshot of the input's state is.
const (
	InputTick    = "tick"
	InputCase    = "case"
	InputCheck   = "check"
	InputExecute = "execute"

	SnapshotNone    = "none"
	SnapshotHeld    = "held"
	SnapshotDeleted = "deleted"
)

// InputSchemaVersion is the version of the envelope an input is exported as (runtime/input.schema.json).
const InputSchemaVersion = 1

// Input is what flotillad handed a live run and what a shadow of it is given, recorded when the live run starts, before it
// writes. Its key is the live run's id.
type Input struct {
	Key           string          `json:"input_key"`
	Routine       string          `json:"routine"`
	Kind          string          `json:"kind"`
	Version       int             `json:"version"`
	Trigger       string          `json:"trigger"`
	Actor         string          `json:"actor"`
	StartedAt     string          `json:"started_at"`
	Handed        json.RawMessage `json:"handed,omitempty"` // the case, the plan and the tick time as handed to the run
	MarkFilings   int64           `json:"ledger_mark_filings"`
	MarkEntries   int64           `json:"ledger_mark_entries"`
	LiveCaseKeys  []string        `json:"live_case_keys"`
	CommitRepo    string          `json:"commit_repo,omitempty"`
	CommitSHA     string          `json:"commit_sha,omitempty"`
	SnapshotPaths []string        `json:"snapshot_paths"`
	SnapshotState string          `json:"snapshot_state"`
	Pinned        bool            `json:"pinned"`
	SchemaVersion int             `json:"schema_version"`
}

var ErrNoSuchInput = errors.New("no such input")

const inputCols = `input_key, routine, kind, version, trigger, actor, started_at, handed, ledger_mark_filings, ledger_mark_entries,
	live_case_keys, commit_repo, commit_sha, snapshot_paths, snapshot_state, pinned, schema_version`

// NewInput records a live run's input. One per live run; a second with the same key is refused.
func (s *Store) NewInput(ctx context.Context, in Input) error {
	if in.Key == "" || in.Routine == "" {
		return errors.New("an input needs a key (the live run's id) and a routine")
	}
	switch in.Kind {
	case InputTick, InputCase, InputCheck, InputExecute:
	default:
		return fmt.Errorf("an input's kind is tick, case, check or execute, got %q", in.Kind)
	}
	if IsShadowKey(in.Routine) {
		return fmt.Errorf("an input is a live run's: %q is a shadow's key", in.Routine)
	}
	handed := string(in.Handed)
	if handed == "" {
		handed = "{}"
	}
	if !json.Valid([]byte(handed)) {
		return errors.New("an input's handed is not JSON")
	}
	keys, _ := json.Marshal(nonNil(in.LiveCaseKeys))
	paths, _ := json.Marshal(nonNil(in.SnapshotPaths))
	state := in.SnapshotState
	if state == "" {
		state = SnapshotNone
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO run_inputs (`+inputCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		in.Key, in.Routine, in.Kind, versionOrOne(in.Version), in.Trigger, in.Actor, in.StartedAt, handed, in.MarkFilings, in.MarkEntries,
		string(keys), in.CommitRepo, in.CommitSHA, string(paths), state, in.Pinned, InputSchemaVersion)
	return err
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// Input is one live run's recorded input, ErrNoSuchInput when there is none.
func (s *Store) Input(ctx context.Context, key string) (Input, error) {
	var in Input
	var handed, keys, paths string
	err := s.db.QueryRowContext(ctx, `SELECT `+inputCols+` FROM run_inputs WHERE input_key = ?`, key).Scan(&in.Key, &in.Routine, &in.Kind,
		&in.Version, &in.Trigger, &in.Actor, &in.StartedAt, &handed, &in.MarkFilings, &in.MarkEntries, &keys, &in.CommitRepo, &in.CommitSHA,
		&paths, &in.SnapshotState, &in.Pinned, &in.SchemaVersion)
	if errors.Is(err, sql.ErrNoRows) {
		return Input{}, fmt.Errorf("%w: %s", ErrNoSuchInput, key)
	}
	if err != nil {
		return Input{}, err
	}
	in.Handed = json.RawMessage(handed)
	if err := json.Unmarshal([]byte(keys), &in.LiveCaseKeys); err != nil {
		return Input{}, fmt.Errorf("input %s: live_case_keys: %w", key, err)
	}
	if err := json.Unmarshal([]byte(paths), &in.SnapshotPaths); err != nil {
		return Input{}, fmt.Errorf("input %s: snapshot_paths: %w", key, err)
	}
	return in, nil
}

// SetSnapshot records what became of an input's state snapshot: held (taken, with its paths), or deleted.
func (s *Store) SetSnapshot(ctx context.Context, key, state string, paths []string) error {
	switch state {
	case SnapshotNone, SnapshotHeld, SnapshotDeleted:
	default:
		return fmt.Errorf("a snapshot is none, held or deleted, got %q", state)
	}
	raw, _ := json.Marshal(nonNil(paths))
	res, err := s.db.ExecContext(ctx, `UPDATE run_inputs SET snapshot_state = ?, snapshot_paths = ? WHERE input_key = ?`, state, string(raw), key)
	return oneInput(res, err, key)
}

// PinInput keeps (or stops keeping) an input's snapshot past the end of its shadow runs, so an export can still read it.
func (s *Store) PinInput(ctx context.Context, key string, pinned bool) error {
	res, err := s.db.ExecContext(ctx, `UPDATE run_inputs SET pinned = ? WHERE input_key = ?`, pinned, key)
	return oneInput(res, err, key)
}

func oneInput(res sql.Result, err error, key string) error {
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("%w: %s", ErrNoSuchInput, key)
	}
	return nil
}

// LedgerMark is a ledger namespace's high-water mark: the newest seq of its filings and of its entries (0 for none). Seqs are
// global and only grow, so a row written after the mark has a larger seq whatever namespace it is in.
func (s *Store) LedgerMark(ctx context.Context, ns string) (filings, entries int64, err error) {
	if err = s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) FROM ledger_filings WHERE namespace = ?`, ns).Scan(&filings); err != nil {
		return
	}
	err = s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) FROM ledger_entries WHERE namespace = ?`, ns).Scan(&entries)
	return
}

// OpenCaseKeys is the canonical keys of the open (not closed) cases of the given kinds, sorted: what a monitor's detections are
// compared with, as of the moment an input is frozen.
func (s *Store) OpenCaseKeys(ctx context.Context, kinds []string) ([]string, error) {
	if len(kinds) == 0 {
		return []string{}, nil
	}
	ph := strings.TrimSuffix(strings.Repeat("?, ", len(kinds)), ", ")
	args := make([]any, len(kinds))
	for i, k := range kinds {
		args[i] = k
	}
	rows, err := s.db.QueryContext(ctx, `SELECT canonical_key FROM cases WHERE status <> 'closed' AND kind IN (`+ph+`) ORDER BY canonical_key`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// FilingsAsOf is the ledger's current filings as a shadow sees them: the live namespace's rows up to the mark, plus the
// shadow's own namespace; where both filed one fingerprint, the shadow's own row is the current one. keys narrows as Filings does.
func (s *Store) FilingsAsOf(ctx context.Context, liveNS, ownNS string, mark int64, keys map[string]string) ([]Filing, error) {
	for k := range keys {
		if !contains(FilingKeyKinds, k) {
			return nil, fmt.Errorf("unknown filing key %q", k)
		}
	}
	const visible = `((%[1]s.namespace = ? AND %[1]s.seq <= ?) OR %[1]s.namespace = ?)`
	q := `SELECT f.seq, f.row, f.imported FROM ledger_filings f WHERE ` + fmt.Sprintf(visible, "f") + `
		AND f.seq = (SELECT g.seq FROM ledger_filings g WHERE g.fingerprint = f.fingerprint AND ` + fmt.Sprintf(visible, "g") + `
			ORDER BY (g.namespace = ?) DESC, g.seq DESC LIMIT 1)`
	args := []any{liveNS, mark, ownNS, liveNS, mark, ownNS, ownNS}
	if len(keys) > 0 {
		var match []string
		for _, k := range FilingKeyKinds {
			if v := keys[k]; v != "" {
				match = append(match, `(kind = ? AND value = ?)`)
				args = append(args, k, v)
			}
		}
		if len(match) == 0 {
			return nil, nil
		}
		q += ` AND f.seq IN (SELECT filing_seq FROM ledger_keys WHERE ` + strings.Join(match, " OR ") + `)`
	}
	q += ` ORDER BY f.seq`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Filing
	for rows.Next() {
		var f Filing
		var raw string
		if err := rows.Scan(&f.Seq, &raw, &f.Imported); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(raw), &f.Row); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// EntriesAsOf is the decision ledger as a shadow sees it: the live namespace's entries up to the mark, then the shadow's own,
// in seq order, after afterSeq, at most limit (0: all).
func (s *Store) EntriesAsOf(ctx context.Context, liveNS, ownNS string, mark, afterSeq int64, limit int) ([]Entry, error) {
	q := `SELECT seq, kind, row, imported FROM ledger_entries WHERE ((namespace = ? AND seq <= ?) OR namespace = ?) AND seq > ? ORDER BY seq`
	args := []any{liveNS, mark, ownNS, afterSeq}
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Entry
	for rows.Next() {
		var e Entry
		var raw string
		if err := rows.Scan(&e.Seq, &e.Kind, &raw, &e.Imported); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(raw), &e.Row); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// A shadow run's place in the queue, per (input, version): queued until a worker takes it, running, done, or not run with
// the reason (superseded, queue full, budget, retired, the live run did not start, ...); cancelled when it was stopped in flight.
const (
	ShadowQueued    = "queued"
	ShadowRunning   = "running"
	ShadowDone      = "done"
	ShadowNotRun    = "not_run"
	ShadowCancelled = "cancelled"
)

// ShadowRun is one row of the shadow queue: version Version of Routine (the live id) on the input InputKey.
type ShadowRun struct {
	ID            int64  `json:"id"`
	InputKey      string `json:"input_key"`
	Routine       string `json:"routine"`
	Version       int    `json:"version"`
	VersionHash   string `json:"version_hash"`
	Status        string `json:"status"`
	Reason        string `json:"reason,omitempty"`
	RunID         string `json:"run_id,omitempty"` // the shadow's own run, once it started
	QueuedAt      string `json:"queued_at"`
	StartedAt     string `json:"started_at,omitempty"`
	LiveStartedAt string `json:"live_started_at,omitempty"` // when the live run of the input started: a shadow that starts later says both
	EndedAt       string `json:"ended_at,omitempty"`
}

var ErrShadowRunExists = errors.New("this version already has a shadow run on this input")

const shadowRunCols = `id, input_key, routine, version, version_hash, status, reason, run_id, queued_at, started_at, live_started_at, ended_at`

// QueueShadowRun adds a shadow run for (input, routine, version). Status is queued, or not_run with a reason when it was
// decided not to run at all. A second row for the same triple is refused.
func (s *Store) QueueShadowRun(ctx context.Context, r ShadowRun) (int64, error) {
	if r.InputKey == "" || r.Routine == "" || r.Version < 1 {
		return 0, errors.New("a shadow run needs an input, a routine and a version")
	}
	if r.Status == "" {
		r.Status = ShadowQueued
	}
	if r.Status != ShadowQueued && r.Status != ShadowNotRun {
		return 0, fmt.Errorf("a shadow run enters the queue queued or not_run, got %q", r.Status)
	}
	if r.Status == ShadowNotRun && r.Reason == "" {
		return 0, errors.New("a shadow run that is not run says why")
	}
	if r.QueuedAt == "" {
		r.QueuedAt = s.now().Format(ts)
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO shadow_runs (input_key, routine, version, version_hash, status, reason, queued_at, live_started_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, r.InputKey, r.Routine, r.Version, r.VersionHash, r.Status, r.Reason, r.QueuedAt, r.LiveStartedAt)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return 0, fmt.Errorf("%w: %s v%d on %s", ErrShadowRunExists, r.Routine, r.Version, r.InputKey)
		}
		return 0, err
	}
	return res.LastInsertId()
}

// ShadowRunStep moves a queue row: queued to running (with the shadow's run id), a running or queued one to done, cancelled or
// not_run (with the reason). Any other move is refused, so a terminal row stays as it ended.
func (s *Store) ShadowRunStep(ctx context.Context, id int64, to, reason, runID string) error {
	allowed := map[string][]string{
		ShadowRunning:   {ShadowQueued},
		ShadowDone:      {ShadowRunning},
		ShadowCancelled: {ShadowRunning, ShadowQueued},
		ShadowNotRun:    {ShadowQueued},
	}[to]
	if allowed == nil {
		return fmt.Errorf("a queued shadow run moves to running, done, cancelled or not_run, not %q", to)
	}
	if to == ShadowNotRun && reason == "" {
		return errors.New("a shadow run that is not run says why")
	}
	ph := strings.TrimSuffix(strings.Repeat("?, ", len(allowed)), ", ")
	args := []any{to, reason}
	set := `status = ?, reason = CASE WHEN ? = '' THEN reason ELSE ? END`
	args = append(args, reason)
	now := s.now().Format(ts)
	switch to {
	case ShadowRunning:
		set += `, started_at = ?, run_id = ?`
		args = append(args, now, runID)
	default:
		set += `, ended_at = ?`
		args = append(args, now)
	}
	args = append(args, id)
	for _, a := range allowed {
		args = append(args, a)
	}
	res, err := s.db.ExecContext(ctx, `UPDATE shadow_runs SET `+set+` WHERE id = ? AND status IN (`+ph+`)`, args...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("shadow run %d is not in a state that moves to %s", id, to)
	}
	return nil
}

// ShadowRunFilter picks rows of the queue; a zero field matches all.
type ShadowRunFilter struct {
	InputKey string
	Routine  string
	Version  int
	Status   string
}

// ShadowRuns lists queue rows, oldest first.
func (s *Store) ShadowRuns(ctx context.Context, f ShadowRunFilter) ([]ShadowRun, error) {
	q, args := `SELECT `+shadowRunCols+` FROM shadow_runs WHERE 1 = 1`, []any{}
	if f.InputKey != "" {
		q, args = q+` AND input_key = ?`, append(args, f.InputKey)
	}
	if f.Routine != "" {
		q, args = q+` AND routine = ?`, append(args, f.Routine)
	}
	if f.Version > 0 {
		q, args = q+` AND version = ?`, append(args, f.Version)
	}
	if f.Status != "" {
		q, args = q+` AND status = ?`, append(args, f.Status)
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ShadowRun
	for rows.Next() {
		var r ShadowRun
		if err := rows.Scan(&r.ID, &r.InputKey, &r.Routine, &r.Version, &r.VersionHash, &r.Status, &r.Reason, &r.RunID, &r.QueuedAt,
			&r.StartedAt, &r.LiveStartedAt, &r.EndedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ShadowRecord is one thing a shadow run would have written, kept instead of writing it. Subject is the pairing key (a case id,
// a signal's fingerprint) next to InputKey; Kind says what it is (plan, report, decision, note, detect.open, ledger_filing, ...).
type ShadowRecord struct {
	ID          int64           `json:"id"`
	RunID       string          `json:"run_id"`
	Routine     string          `json:"routine"` // the shadow's key, "<fleet>/<routine>@v<n>"
	Version     int             `json:"version"`
	VersionHash string          `json:"version_hash"`
	InputKey    string          `json:"input_key"`
	Subject     string          `json:"subject,omitempty"`
	Kind        string          `json:"kind"`
	Payload     json.RawMessage `json:"payload"`
	CreatedAt   string          `json:"created_at"`
}

// RecordShadow keeps one would-be write. The routine must be a shadow's key and the payload JSON (an object when empty).
func (s *Store) RecordShadow(ctx context.Context, r ShadowRecord) (int64, error) {
	if r.RunID == "" || r.InputKey == "" || r.Kind == "" {
		return 0, errors.New("a shadow record needs a run, an input and a kind")
	}
	if !IsShadowKey(r.Routine) {
		return 0, fmt.Errorf("a shadow record is under a shadow's key (<fleet>/<routine>@v<n>), got %q", r.Routine)
	}
	if r.Version < 1 {
		return 0, errors.New("a shadow record names its version")
	}
	payload := string(r.Payload)
	if payload == "" {
		payload = "{}"
	}
	if !json.Valid([]byte(payload)) {
		return 0, errors.New("a shadow record's payload is not JSON")
	}
	if r.CreatedAt == "" {
		r.CreatedAt = s.now().Format(ts)
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO shadow_records (run_id, routine, version, version_hash, input_key, subject, kind, payload, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, r.RunID, r.Routine, r.Version, r.VersionHash, r.InputKey, r.Subject, r.Kind, payload, r.CreatedAt)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ShadowRecordFilter picks records; a zero field matches all.
type ShadowRecordFilter struct {
	InputKey string
	RunID    string
	Routine  string // the shadow's key
	Subject  string
	Kind     string
	Since    string
	Until    string
}

// ShadowRecords lists records in the order they were written.
func (s *Store) ShadowRecords(ctx context.Context, f ShadowRecordFilter) ([]ShadowRecord, error) {
	q, args := `SELECT id, run_id, routine, version, version_hash, input_key, subject, kind, payload, created_at FROM shadow_records WHERE 1 = 1`, []any{}
	add := func(col, v string) {
		if v != "" {
			q, args = q+` AND `+col+` = ?`, append(args, v)
		}
	}
	add("input_key", f.InputKey)
	add("run_id", f.RunID)
	add("routine", f.Routine)
	add("subject", f.Subject)
	add("kind", f.Kind)
	if f.Since != "" {
		q, args = q+` AND created_at >= ?`, append(args, f.Since)
	}
	if f.Until != "" {
		q, args = q+` AND created_at < ?`, append(args, f.Until)
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ShadowRecord
	for rows.Next() {
		var r ShadowRecord
		var payload string
		if err := rows.Scan(&r.ID, &r.RunID, &r.Routine, &r.Version, &r.VersionHash, &r.InputKey, &r.Subject, &r.Kind, &payload, &r.CreatedAt); err != nil {
			return nil, err
		}
		r.Payload = json.RawMessage(payload)
		out = append(out, r)
	}
	return out, rows.Err()
}

// entrySubject is the SQL for the subject a decision entry is about: its own "subject" field, else its occurrence id, else its
// fingerprint (design section 4: a pair is matched per subject).
const entrySubject = `COALESCE(NULLIF(json_extract(e.row, '$.subject'), ''), NULLIF(e.occurrence_id, ''), e.fingerprint)`

// DecisionSubject is entrySubject for a row in Go (what a shadow's decision record carries as its subject).
func DecisionSubject(row map[string]string, occurrenceID, fingerprint string) string {
	if v := row["subject"]; v != "" {
		return v
	}
	if occurrenceID != "" {
		return occurrenceID
	}
	return fingerprint
}

// ErrShadowRun: a shadow run's plan is never decided and its action never sent. The shadow path does not reach either (it only
// records), and this is the refusal under it: a plan of a shadow run that somehow exists cannot become a host execution.
var ErrShadowRun = errors.New("a shadow run never decides a plan or sends an action: what it would run is a shadow record")

// RunIsShadow says whether a run is a shadow run; an unknown run is not.
func (s *Store) RunIsShadow(ctx context.Context, runID string) (bool, error) {
	var n int
	// shadow-ok: this is the check itself, by run id
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(shadow), 0) FROM runs WHERE id = ?`, runID).Scan(&n)
	return n != 0, err
}

// planIsShadows says whether the plan was made by a shadow run.
func (s *Store) planIsShadows(ctx context.Context, planID int64) (bool, error) {
	var n int
	// shadow-ok: this is the check itself, by plan id
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(r.shadow), 0) FROM plans p JOIN runs r ON r.id = p.run_id WHERE p.id = ?`, planID).Scan(&n)
	return n != 0, err
}

// ShadowRunsActive is how many of an input's shadow runs are still queued or running.
func (s *Store) ShadowRunsActive(ctx context.Context, inputKey string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM shadow_runs WHERE input_key = ? AND status IN ('queued', 'running')`, inputKey).Scan(&n)
	return n, err
}

// ExpireShadowRuns ends every shadow run left queued or running by a flotillad that stopped: a queued one did not run (the reason),
// a running one is cancelled (its worker's run was cancelled with the rest by RecoverInterrupted). It returns how many.
func (s *Store) ExpireShadowRuns(ctx context.Context, reason string) (int64, error) {
	now := s.now().Format(ts)
	a, err := s.db.ExecContext(ctx, `UPDATE shadow_runs SET status = 'not_run', reason = ?, ended_at = ? WHERE status = 'queued'`, reason, now)
	if err != nil {
		return 0, err
	}
	b, err := s.db.ExecContext(ctx, `UPDATE shadow_runs SET status = 'cancelled', reason = ?, ended_at = ? WHERE status = 'running'`, reason, now)
	if err != nil {
		return 0, err
	}
	na, _ := a.RowsAffected()
	nb, _ := b.RowsAffected()
	return na + nb, nil
}

// HeldSnapshots lists the inputs whose state snapshot is still on a live worker, oldest first.
func (s *Store) HeldSnapshots(ctx context.Context) ([]Input, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT input_key FROM run_inputs WHERE snapshot_state = ? ORDER BY started_at`, SnapshotHeld)
	if err != nil {
		return nil, err
	}
	var keys []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			rows.Close()
			return nil, err
		}
		keys = append(keys, k)
	}
	rows.Close()
	var out []Input
	for _, k := range keys {
		in, err := s.Input(ctx, k)
		if err != nil {
			return nil, err
		}
		out = append(out, in)
	}
	return out, nil
}
