package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/chipi/flotilla/internal/cases"
	"github.com/chipi/flotilla/internal/spec"
)

// Routine versions (docs/wip/v2-design.md section 1): which version of a routine is live, shadow,
// previous or retired. Every state change is one transaction that also writes an events row, kind
// version.deploy|update|promote|rollback|retire, subject <routine>@v<n>, the way SetControlBecause does
// for a control. What a state change may do is decided here; whether the version's folder still has
// the hash recorded for it, and whether the routine is enabled and its worker known, are the caller's
// to check before it asks (internal/versions, internal/api).

// Version states.
const (
	VersionLive     = "live"
	VersionShadow   = "shadow"
	VersionPrevious = "previous"
	VersionRetired  = "retired"
)

// How a retired version was retired.
const (
	RetiredPlain      = "retired"
	RetiredRolledBack = "rolled_back"
)

var (
	ErrNoSuchVersion   = errors.New("no such version")
	ErrVersionConflict = errors.New("version state conflict")  // the version is not in a state the change needs
	ErrVersionInvalid  = errors.New("invalid version request") // a missing or unacceptable argument
)

// runVersion is the version of the run an INSERT is for, as a sub-select over the run id it is bound to:
// 1 for a row with no run (imported, or from before versions).
const runVersion = `COALESCE((SELECT version FROM runs WHERE id = ?), 1)` // shadow-ok: by run id; a shadow's row takes its own run's version

func versionOrOne(v int) int {
	if v < 1 {
		return 1
	}
	return v
}

var autonomyRank = map[string]int{"record": 0, "propose": 1, "auto": 2}

// RoutineVersion is one row of routine_versions.
type RoutineVersion struct {
	Routine           string            `json:"routine"`
	Version           int               `json:"version"`
	Hash              string            `json:"hash"`
	Manifest          map[string]string `json:"manifest,omitempty"`
	State             string            `json:"state"`
	DeployedAt        string            `json:"deployed_at"`
	LiveAt            string            `json:"live_at,omitempty"`
	StateBy           string            `json:"state_by,omitempty"`
	StateAt           string            `json:"state_at,omitempty"`
	StateWhy          string            `json:"state_why,omitempty"`
	RetiredKind       string            `json:"retired_kind,omitempty"`
	AutonomyAtPromote string            `json:"autonomy_at_promote,omitempty"`
	// Promotion is what the promote that made it live recorded: the acceptance in force and the evidence given.
	Promotion *PromoteRecord `json:"promotion,omitempty"`
}

// LoadedVersion is a version as flotillad loaded it from the fleets folder.
type LoadedVersion struct {
	Version  int
	Hash     string
	Manifest map[string]string
	// Legacy: it is a flat spec's v1, which is not frozen (a changed one is re-recorded, see EnsureVersions).
	Legacy bool
	// Moved: a folder v1 that replaces a flat one and that spec.ProveMoved found unchanged: the keys and files compared and equal. The
	// caller sets it after the proof; EnsureVersions then re-records the row from the folder layout and says what was compared.
	Moved []string
}

const versionCols = `routine, version, hash, manifest, state, deployed_at, live_at, state_by, state_at, state_why, retired_kind, autonomy_at_promote, promote_evidence`

// versionColsBefore29 is versionCols for a store older than the step that added promote_evidence.
const versionColsBefore29 = `routine, version, hash, manifest, state, deployed_at, live_at, state_by, state_at, state_why, retired_kind, autonomy_at_promote, ''`

func scanVersion(sc interface{ Scan(...any) error }) (RoutineVersion, error) {
	var v RoutineVersion
	var manifest, evidence string
	if err := sc.Scan(&v.Routine, &v.Version, &v.Hash, &manifest, &v.State, &v.DeployedAt, &v.LiveAt, &v.StateBy, &v.StateAt,
		&v.StateWhy, &v.RetiredKind, &v.AutonomyAtPromote, &evidence); err != nil {
		return v, err
	}
	_ = json.Unmarshal([]byte(manifest), &v.Manifest)
	if evidence != "" {
		var pr PromoteRecord
		if json.Unmarshal([]byte(evidence), &pr) == nil {
			v.Promotion = &pr
		}
	}
	return v, nil
}

type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func versionsOf(ctx context.Context, q queryer, where string, args ...any) ([]RoutineVersion, error) {
	return versionsOfCols(ctx, q, versionCols, where, args...)
}

func versionsOfCols(ctx context.Context, q queryer, cols, where string, args ...any) ([]RoutineVersion, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+cols+` FROM routine_versions `+where+` ORDER BY routine, version`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RoutineVersion
	for rows.Next() {
		v, err := scanVersion(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// Versions is one routine's versions, oldest number first.
func (s *Store) Versions(ctx context.Context, routine string) ([]RoutineVersion, error) {
	return versionsOf(ctx, s.db, `WHERE routine = ?`, routine)
}

// AllVersions is every routine's versions.
func (s *Store) AllVersions(ctx context.Context) ([]RoutineVersion, error) {
	return versionsOf(ctx, s.db, ``)
}

// LiveVersion is the routine's live version, false when it has none recorded.
func (s *Store) LiveVersion(ctx context.Context, routine string) (RoutineVersion, bool, error) {
	vs, err := versionsOf(ctx, s.db, `WHERE routine = ? AND state = 'live'`, routine)
	if err != nil || len(vs) == 0 {
		return RoutineVersion{}, false, err
	}
	return vs[0], true, nil
}

func addVersionEvent(ctx context.Context, tx *sql.Tx, at, actor, via, kind, routine string, version int, detail string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO events (at, actor, via, kind, subject, detail) VALUES (?, ?, ?, ?, ?, ?)`,
		at, actor, via, kind, fmt.Sprintf("%s@v%d", routine, version), detail)
	return err
}

// EnsureVersions records what flotillad loaded, at its start: the first versions of a routine with none
// recorded (the lowest goes live, "deploy"; any others arrive as shadows), and a new number of a routine
// that has some always as a shadow. A shadow that changed on disk gets its new hash; so does a flat spec's live or
// previous v1 (not frozen, see LoadedVersion.Legacy). Any other live, previous or retired version is never touched
// here (its freeze is checked by the caller). It reports what it did.
func (s *Store) EnsureVersions(ctx context.Context, routine string, loaded []LoadedVersion, actor string) (EnsureResult, error) {
	var res EnsureResult
	if len(loaded) == 0 {
		return res, nil
	}
	sort.Slice(loaded, func(i, j int) bool { return loaded[i].Version < loaded[j].Version })
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return res, err
	}
	defer tx.Rollback()
	existing, err := versionsOf(ctx, tx, `WHERE routine = ?`, routine)
	if err != nil {
		return res, err
	}
	by := map[int]RoutineVersion{}
	for _, v := range existing {
		by[v.Version] = v
	}
	first := len(existing) == 0
	at := s.now().Format(ts)
	for _, lv := range loaded {
		manifest, _ := json.Marshal(lv.Manifest)
		row, ok := by[lv.Version]
		switch {
		case !ok && first:
			first = false
			if _, err := tx.ExecContext(ctx, `INSERT INTO routine_versions (routine, version, hash, manifest, state, deployed_at, live_at, state_by, state_at, state_why)
				VALUES (?, ?, ?, ?, 'live', ?, ?, ?, ?, 'the first version, live at deploy')`, routine, lv.Version, lv.Hash, string(manifest), at, at, actor, at); err != nil {
				return res, err
			}
			if err := addVersionEvent(ctx, tx, at, actor, "deploy", "version.deploy", routine, lv.Version, "live: the first version of the routine, hash "+lv.Hash); err != nil {
				return res, err
			}
			res.Live = lv.Version
		case !ok:
			if _, err := tx.ExecContext(ctx, `INSERT INTO routine_versions (routine, version, hash, manifest, state, deployed_at, state_by, state_at, state_why)
				VALUES (?, ?, ?, ?, 'shadow', ?, ?, ?, 'arrived with a deploy')`, routine, lv.Version, lv.Hash, string(manifest), at, actor, at); err != nil {
				return res, err
			}
			if err := addVersionEvent(ctx, tx, at, actor, "deploy", "version.deploy", routine, lv.Version, "shadow: a new version, hash "+lv.Hash); err != nil {
				return res, err
			}
			res.Shadows = append(res.Shadows, lv.Version)
		case row.State == VersionShadow && row.Hash != lv.Hash:
			if _, err := tx.ExecContext(ctx, `UPDATE routine_versions SET hash = ?, manifest = ? WHERE routine = ? AND version = ?`,
				lv.Hash, string(manifest), routine, lv.Version); err != nil {
				return res, err
			}
			if err := addVersionEvent(ctx, tx, at, actor, "deploy", "version.update", routine, lv.Version, "shadow changed: hash "+row.Hash+" -> "+lv.Hash); err != nil {
				return res, err
			}
			res.Updated = append(res.Updated, lv.Version)
		case (row.State == VersionLive || row.State == VersionPrevious) && len(lv.Moved) > 0 && !lv.Legacy && row.Hash != lv.Hash && RecordedLegacy(row):
			// A flat v1 moved to the folder layout, proven unchanged against its recorded manifest by the caller: the row follows the folder.
			if _, err := tx.ExecContext(ctx, `UPDATE routine_versions SET hash = ?, manifest = ? WHERE routine = ? AND version = ?`,
				lv.Hash, string(manifest), routine, lv.Version); err != nil {
				return res, err
			}
			detail := "legacy v1 (flat) moved to the folder layout, unchanged (" + strings.Join(lv.Moved, ", ") + "): re-recorded: hash " + row.Hash + " -> " + lv.Hash
			if err := addVersionEvent(ctx, tx, at, actor, "deploy", "version.update", routine, lv.Version, detail); err != nil {
				return res, err
			}
			res.Rerecorded = append(res.Rerecorded, lv.Version)
		case (row.State == VersionLive || row.State == VersionPrevious) && lv.Legacy && row.Hash != lv.Hash && RecordedLegacy(row):
			// A flat spec's v1 is not frozen: its playbooks and settings change in the ordinary way, so the new hash and
			// manifest are recorded and the event says what moved. A folder version stays frozen (Rerecord moves it).
			if _, err := tx.ExecContext(ctx, `UPDATE routine_versions SET hash = ?, manifest = ? WHERE routine = ? AND version = ?`,
				lv.Hash, string(manifest), routine, lv.Version); err != nil {
				return res, err
			}
			detail := "legacy v1 (flat) re-recorded: hash " + row.Hash + " -> " + lv.Hash + "; changed: " + strings.Join(spec.ChangedFiles(row.Manifest, lv.Manifest), ", ")
			if err := addVersionEvent(ctx, tx, at, actor, "deploy", "version.update", routine, lv.Version, detail); err != nil {
				return res, err
			}
			res.Rerecorded = append(res.Rerecorded, lv.Version)
		}
	}
	return res, tx.Commit()
}

// EnsureResult is what EnsureVersions changed: the version that went live as the routine's first, new
// shadows, and shadows whose hash moved.
type EnsureResult struct {
	Live       int
	Shadows    []int
	Updated    []int
	Rerecorded []int // a flat spec's live or previous v1 whose hash moved
}

// RecordedLegacy says the row was recorded from a flat spec (its manifest names version-owned keys such as "run", not
// the files of a folder): only such a row follows a flat spec's changes.
func RecordedLegacy(r RoutineVersion) bool {
	_, run := r.Manifest["run"]
	_, folder := r.Manifest["spec.yaml"]
	return run && !folder
}

// PromoteArgs is a request to make a shadow live.
type PromoteArgs struct {
	Routine string
	Version int
	// Hash is the version's hash as flotillad has it loaded: it must be the one recorded, so a shadow edited
	// since the operator looked is refused, not promoted.
	Hash string
	// Autonomy is the routine's autonomy the operator chooses for the version (never inherited in silence);
	// Current is what the routine runs at now: a raise needs a reason.
	Autonomy, Current  string
	Actor, Via, Reason string
	// WithoutPairs: the operator says they know the version has no compared pairs. It is only recorded (in the
	// event): the reason a promote with missing or failed evidence needs is the acknowledgement.
	WithoutPairs bool
	// Acceptance is what the routine declares a promote needs (spec.Routine.Accepts); empty means [pairs].
	// Evidence is what the operator gives for it: flotilla stores the verdicts, it never computes them.
	Acceptance []string
	Evidence   Evidence
}

// Gate is a gate's result, as the operator gives it: Result is pass or fail, Ref where it can be read.
type Gate struct {
	Name   string `json:"name"`
	Result string `json:"result"`
	Ref    string `json:"ref,omitempty"`
}

// EvalRun is an eval-harness run and its verdict (pass or fail).
type EvalRun struct {
	RunID   string `json:"run_id"`
	Verdict string `json:"verdict"`
}

// Evidence is what a promote is given beside the compared pairs, which the store counts itself.
type Evidence struct {
	Gates []Gate    `json:"gates,omitempty"`
	Evals []EvalRun `json:"evals,omitempty"`
}

// PromoteRecord is the acceptance in force at a promote and the evidence it had, stored with the version it
// made live (routine_versions.promote_evidence) and in its version.promote event. Missing and Failed are what
// the promote went ahead without or in spite of, which only a reason allows.
type PromoteRecord struct {
	Acceptance   []string  `json:"acceptance"`
	Pairs        int       `json:"pairs"`
	Gates        []Gate    `json:"gates,omitempty"`
	Evals        []EvalRun `json:"evals,omitempty"`
	Missing      []string  `json:"missing,omitempty"`
	Failed       []string  `json:"failed,omitempty"`
	WithoutPairs bool      `json:"without_pairs,omitempty"`
}

func passing(v string) bool { return v == "pass" }

// assess checks the evidence given against the acceptance and says what is missing (a declared kind with
// nothing behind it) and what failed (a gate or verdict given as fail, whether or not its kind is declared).
// pairs: a compared pair exists; replay: a gate result was given; scenario_eval: an eval run was given.
func assess(accept []string, pairs int, ev Evidence) (rec PromoteRecord, err error) {
	if len(accept) == 0 {
		accept = []string{"pairs"}
	}
	rec = PromoteRecord{Acceptance: accept, Pairs: pairs, Gates: ev.Gates, Evals: ev.Evals}
	for _, g := range ev.Gates {
		if strings.TrimSpace(g.Name) == "" || (g.Result != "pass" && g.Result != "fail") {
			return rec, fmt.Errorf("%w: a gate is name=pass|fail, not %q=%q", ErrVersionInvalid, g.Name, g.Result)
		}
		if !passing(g.Result) {
			rec.Failed = append(rec.Failed, "gate "+g.Name)
		}
	}
	for _, e := range ev.Evals {
		if strings.TrimSpace(e.RunID) == "" || (e.Verdict != "pass" && e.Verdict != "fail") {
			return rec, fmt.Errorf("%w: an eval is run_id=pass|fail, not %q=%q", ErrVersionInvalid, e.RunID, e.Verdict)
		}
		if !passing(e.Verdict) {
			rec.Failed = append(rec.Failed, "eval "+e.RunID)
		}
	}
	for _, k := range accept {
		switch k {
		case "pairs":
			if pairs == 0 {
				rec.Missing = append(rec.Missing, "pairs")
			}
		case "replay":
			if len(ev.Gates) == 0 {
				rec.Missing = append(rec.Missing, "replay")
			}
		case "scenario_eval":
			if len(ev.Evals) == 0 {
				rec.Missing = append(rec.Missing, "scenario_eval")
			}
		default:
			return rec, fmt.Errorf("%w: acceptance %q: one of pairs, replay, scenario_eval", ErrVersionInvalid, k)
		}
	}
	return rec, nil
}

func (r PromoteRecord) summary() string {
	parts := []string{"acceptance " + strings.Join(r.Acceptance, ",")}
	parts = append(parts, fmt.Sprintf("%d compared pair(s)", r.Pairs))
	for _, g := range r.Gates {
		parts = append(parts, "gate "+g.Name+"="+g.Result)
	}
	for _, e := range r.Evals {
		parts = append(parts, "eval "+e.RunID+"="+e.Verdict)
	}
	if len(r.Missing) > 0 {
		parts = append(parts, "missing "+strings.Join(r.Missing, ","))
	}
	if len(r.Failed) > 0 {
		parts = append(parts, "failed "+strings.Join(r.Failed, ","))
	}
	if r.WithoutPairs {
		parts = append(parts, "without pairs, acknowledged")
	}
	return strings.Join(parts, "; ")
}

func upsertAutonomy(ctx context.Context, tx *sql.Tx, routine, level, actor, via, reason, at string) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO controls (subject, key, value, actor, at) VALUES (?, 'autonomy', ?, ?, ?)
		ON CONFLICT(subject, key) DO UPDATE SET value = excluded.value, actor = excluded.actor, at = excluded.at`, routine, level, actor, at); err != nil {
		return err
	}
	detail := level
	if reason != "" {
		detail += ": " + reason
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO events (at, actor, via, kind, subject, detail) VALUES (?, ?, ?, 'control.autonomy', ?, ?)`, at, actor, via, routine, detail)
	return err
}

func checkAutonomy(level, current, reason string) error {
	if _, ok := autonomyRank[level]; !ok {
		return fmt.Errorf("%w: autonomy %q: choose record, propose or auto", ErrVersionInvalid, level)
	}
	if autonomyRank[level] > autonomyRank[current] && strings.TrimSpace(reason) == "" {
		return fmt.Errorf("%w: raising the autonomy to %s needs a reason", ErrVersionInvalid, level)
	}
	return nil
}

func (s *Store) rowTx(ctx context.Context, tx *sql.Tx, routine string, version int) (RoutineVersion, error) {
	vs, err := versionsOf(ctx, tx, `WHERE routine = ? AND version = ?`, routine, version)
	if err != nil {
		return RoutineVersion{}, err
	}
	if len(vs) == 0 {
		return RoutineVersion{}, fmt.Errorf("%w: %s v%d", ErrNoSuchVersion, routine, version)
	}
	return vs[0], nil
}

func (s *Store) stateTx(ctx context.Context, tx *sql.Tx, routine, state string) (RoutineVersion, bool, error) {
	vs, err := versionsOf(ctx, tx, `WHERE routine = ? AND state = ?`, routine, state)
	if err != nil || len(vs) == 0 {
		return RoutineVersion{}, false, err
	}
	return vs[0], true, nil
}

// Promote makes a shadow the routine's live version: the live one becomes previous, the old previous is
// retired, the chosen autonomy is set as the routine's control, and one events row says who, when and why.
func (s *Store) Promote(ctx context.Context, a PromoteArgs) error {
	if strings.TrimSpace(a.Actor) == "" {
		return fmt.Errorf("%w: who promotes", ErrVersionInvalid)
	}
	if err := checkAutonomy(a.Autonomy, a.Current, a.Reason); err != nil {
		return err
	}
	pairs, err := s.PairsFor(ctx, a.Routine, a.Version)
	if err != nil {
		return err
	}
	rec, err := assess(a.Acceptance, pairs, a.Evidence)
	if err != nil {
		return err
	}
	rec.WithoutPairs = a.WithoutPairs
	if (len(rec.Missing) > 0 || len(rec.Failed) > 0) && strings.TrimSpace(a.Reason) == "" {
		var why []string
		if len(rec.Missing) > 0 {
			why = append(why, "lacks the evidence its acceptance names ("+strings.Join(rec.Missing, ", ")+")")
		}
		if len(rec.Failed) > 0 {
			why = append(why, "has a failed result ("+strings.Join(rec.Failed, ", ")+")")
		}
		return fmt.Errorf("%w: v%d %s: give a reason to promote it anyway", ErrVersionInvalid, a.Version, strings.Join(why, " and "))
	}
	evidence, _ := json.Marshal(rec)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	v, err := s.rowTx(ctx, tx, a.Routine, a.Version)
	if err != nil {
		return err
	}
	if v.State != VersionShadow {
		return fmt.Errorf("%w: %s v%d is %s, not a shadow", ErrVersionConflict, a.Routine, a.Version, v.State)
	}
	if v.Hash != a.Hash {
		return fmt.Errorf("%w: %s v%d changed since it was recorded (recorded %s, loaded %s)", ErrVersionConflict, a.Routine, a.Version, v.Hash, a.Hash)
	}
	at := s.now().Format(ts)
	live, hasLive, err := s.stateTx(ctx, tx, a.Routine, VersionLive)
	if err != nil {
		return err
	}
	if prev, ok, err := s.stateTx(ctx, tx, a.Routine, VersionPrevious); err != nil {
		return err
	} else if ok {
		if _, err := tx.ExecContext(ctx, `UPDATE routine_versions SET state = 'retired', retired_kind = ?, state_by = ?, state_at = ?, state_why = ?
			WHERE routine = ? AND version = ?`, RetiredPlain, a.Actor, at, fmt.Sprintf("superseded by v%d", a.Version), a.Routine, prev.Version); err != nil {
			return err
		}
		if err := addVersionEvent(ctx, tx, at, a.Actor, a.Via, "version.retire", a.Routine, prev.Version, fmt.Sprintf("superseded by v%d", a.Version)); err != nil {
			return err
		}
	}
	if hasLive {
		if _, err := tx.ExecContext(ctx, `UPDATE routine_versions SET state = 'previous', state_by = ?, state_at = ?, state_why = ?
			WHERE routine = ? AND version = ?`, a.Actor, at, fmt.Sprintf("v%d was promoted", a.Version), a.Routine, live.Version); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE routine_versions SET state = 'live', live_at = ?, state_by = ?, state_at = ?, state_why = ?, autonomy_at_promote = ?, promote_evidence = ?
		WHERE routine = ? AND version = ?`, at, a.Actor, at, a.Reason, a.Autonomy, string(evidence), a.Routine, a.Version); err != nil {
		return err
	}
	detail := "autonomy " + a.Autonomy
	if hasLive {
		detail += fmt.Sprintf("; v%d is previous", live.Version)
	}
	detail += "; " + rec.summary() + "; evidence " + string(evidence)
	if a.Reason != "" {
		detail += ": " + a.Reason
	}
	if err := addVersionEvent(ctx, tx, at, a.Actor, a.Via, "version.promote", a.Routine, a.Version, detail); err != nil {
		return err
	}
	if err := upsertAutonomy(ctx, tx, a.Routine, a.Autonomy, a.Actor, a.Via, a.Reason, at); err != nil {
		return err
	}
	return tx.Commit()
}

// RollbackArgs is a request to make the previous version live again.
type RollbackArgs struct {
	Routine string
	// Hash is the previous version's hash as flotillad has it loaded: it is re-verified before it goes live.
	Hash string
	// Autonomy is chosen again, as at promote; Current is what the routine runs at now.
	Autonomy, Current  string
	Actor, Via, Reason string
}

// RollbackResult is what a rollback left for its caller to finish: the version that was rolled back and the
// one now live, the cases whose plan was marked rolled back (each is back to the live version's to plan), and
// the rolled-back version's runs in flight, which the caller cancels.
type RollbackResult struct {
	RolledBack, Restored int
	Cases                []string
	RunsInFlight         []string
}

// Rollback makes the previous version live at once. The live version is retired as rolled back, with who
// and why; its plans awaiting approval are marked, so they cannot be approved. There is no previous after.
func (s *Store) Rollback(ctx context.Context, a RollbackArgs) (RollbackResult, error) {
	var res RollbackResult
	if strings.TrimSpace(a.Actor) == "" {
		return res, fmt.Errorf("%w: who rolls back", ErrVersionInvalid)
	}
	if strings.TrimSpace(a.Reason) == "" {
		return res, fmt.Errorf("%w: a rollback needs a reason", ErrVersionInvalid)
	}
	if err := checkAutonomy(a.Autonomy, a.Current, a.Reason); err != nil {
		return res, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return res, err
	}
	defer tx.Rollback()
	prev, ok, err := s.stateTx(ctx, tx, a.Routine, VersionPrevious)
	if err != nil {
		return res, err
	}
	if !ok {
		return res, fmt.Errorf("%w: %s has no previous version to roll back to", ErrVersionConflict, a.Routine)
	}
	live, ok, err := s.stateTx(ctx, tx, a.Routine, VersionLive)
	if err != nil {
		return res, err
	}
	if !ok {
		return res, fmt.Errorf("%w: %s has no live version", ErrVersionConflict, a.Routine)
	}
	if prev.Hash != a.Hash {
		return res, fmt.Errorf("%w: %s v%d changed since it was live (recorded %s, loaded %s)", ErrVersionConflict, a.Routine, prev.Version, prev.Hash, a.Hash)
	}
	at := s.now().Format(ts)
	if _, err := tx.ExecContext(ctx, `UPDATE routine_versions SET state = 'retired', retired_kind = ?, state_by = ?, state_at = ?, state_why = ?
		WHERE routine = ? AND version = ?`, RetiredRolledBack, a.Actor, at, a.Reason, a.Routine, live.Version); err != nil {
		return res, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE routine_versions SET state = 'live', live_at = ?, state_by = ?, state_at = ?, state_why = ?, autonomy_at_promote = ?
		WHERE routine = ? AND version = ?`, at, a.Actor, at, fmt.Sprintf("v%d was rolled back: %s", live.Version, a.Reason), a.Autonomy, a.Routine, prev.Version); err != nil {
		return res, err
	}
	// shadow-ok: a plan's run, joined by id; a shadow's plans are never written (they are shadow_records)
	rows, err := tx.QueryContext(ctx, `SELECT p.id, p.case_id FROM plans p JOIN runs r ON r.id = p.run_id JOIN cases c ON c.id = p.case_id
		WHERE r.routine = ? AND p.version = ? AND c.status = 'awaiting_approval' AND p.rolled_back_at IS NULL
		AND p.id = (SELECT MAX(id) FROM plans WHERE case_id = p.case_id) ORDER BY p.id`, a.Routine, live.Version)
	if err != nil {
		return res, err
	}
	var planIDs []int64
	for rows.Next() {
		var id int64
		var caseID string
		if err := rows.Scan(&id, &caseID); err != nil {
			rows.Close()
			return res, err
		}
		planIDs, res.Cases = append(planIDs, id), append(res.Cases, caseID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return res, err
	}
	for i, id := range planIDs {
		if _, err := tx.ExecContext(ctx, `UPDATE plans SET rolled_back_at = ?, rolled_back_by = ? WHERE id = ?`, at, a.Actor, id); err != nil {
			return res, err
		}
		// the case goes back to the acting routine, which plans again with the live version; the target lock is released
		if err := s.moveCaseAs(ctx, tx, "case.rolled_back", res.Cases[i], cases.HandedOver, cases.ByOperator, "", a.Actor,
			fmt.Sprintf("v%d was rolled back, its plan is withdrawn: %s", live.Version, a.Reason)); err != nil {
			return res, err
		}
	}
	// shadow-ok: the exact live routine key; a shadow's runs are keyed with @v and are cancelled by retire
	runs, err := tx.QueryContext(ctx, `SELECT id FROM runs WHERE routine = ? AND version = ? AND status = ? ORDER BY started_at`, a.Routine, live.Version, StatusRunning)
	if err != nil {
		return res, err
	}
	for runs.Next() {
		var id string
		if err := runs.Scan(&id); err != nil {
			runs.Close()
			return res, err
		}
		res.RunsInFlight = append(res.RunsInFlight, id)
	}
	runs.Close()
	if err := runs.Err(); err != nil {
		return res, err
	}
	detail := fmt.Sprintf("v%d is live again, autonomy %s; %d plan(s) awaiting approval marked rolled back, %d run(s) in flight: %s",
		prev.Version, a.Autonomy, len(planIDs), len(res.RunsInFlight), a.Reason)
	if err := addVersionEvent(ctx, tx, at, a.Actor, a.Via, "version.rollback", a.Routine, live.Version, detail); err != nil {
		return res, err
	}
	if err := upsertAutonomy(ctx, tx, a.Routine, a.Autonomy, a.Actor, a.Via, a.Reason, at); err != nil {
		return res, err
	}
	res.RolledBack, res.Restored = live.Version, prev.Version
	return res, tx.Commit()
}

// Retire stops a shadow. A live or previous version is moved by promote or roll back, never retired.
func (s *Store) Retire(ctx context.Context, routine string, version int, actor, via, reason string) error {
	if strings.TrimSpace(actor) == "" || strings.TrimSpace(reason) == "" {
		return fmt.Errorf("%w: a retire needs who and a reason", ErrVersionInvalid)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	v, err := s.rowTx(ctx, tx, routine, version)
	if err != nil {
		return err
	}
	switch v.State {
	case VersionShadow:
	case VersionLive, VersionPrevious:
		return fmt.Errorf("%w: %s v%d is %s: roll back or promote instead", ErrVersionConflict, routine, version, v.State)
	default:
		return fmt.Errorf("%w: %s v%d is already retired", ErrVersionConflict, routine, version)
	}
	at := s.now().Format(ts)
	if _, err := tx.ExecContext(ctx, `UPDATE routine_versions SET state = 'retired', retired_kind = ?, state_by = ?, state_at = ?, state_why = ?
		WHERE routine = ? AND version = ?`, RetiredPlain, actor, at, reason, routine, version); err != nil {
		return err
	}
	if err := addVersionEvent(ctx, tx, at, actor, via, "version.retire", routine, version, reason); err != nil {
		return err
	}
	return tx.Commit()
}

// VersionsRead is what a read-only look at a store found.
type VersionsRead struct {
	Exists bool // the file exists
	Schema int  // its schema step (0: none yet)
	Rows   []RoutineVersion
}

// ReadVersions reads the recorded versions of the store at path without creating, migrating or writing
// anything (flotillad -check, before a deploy swaps fleets). A missing file or a store older than the
// version table reads as no rows: Exists and Schema say which, so the caller can say it skipped the freeze.
func ReadVersions(path string) (VersionsRead, error) {
	var out VersionsRead
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return out, nil
	}
	out.Exists = true
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return out, err
	}
	defer db.Close()
	if err := db.QueryRow(`SELECT COALESCE(MAX(n), 0) FROM schema_version`).Scan(&out.Schema); err != nil {
		if strings.Contains(err.Error(), "no such table") {
			return out, nil
		}
		return out, fmt.Errorf("store: %w", err)
	}
	if out.Schema < VersionsSchema {
		return out, nil
	}
	cols := versionCols
	if out.Schema < EvidenceSchema {
		cols = versionColsBefore29
	}
	out.Rows, err = versionsOfCols(context.Background(), db, cols, ``)
	return out, err
}

// VersionsSchema is the schema step that added routine_versions.
const VersionsSchema = 26

// EvidenceSchema is the schema step that added routine_versions.promote_evidence.
const EvidenceSchema = 29

// RunVersionRolledBack says whether the version a run was made by has since been rolled back, and when and by whom:
// a plan the run files after the rollback marked the plans that existed is still that version's (the run can end
// ok before its cancel lands).
func (s *Store) RunVersionRolledBack(ctx context.Context, runID string) (at, by string, rolledBack bool, err error) {
	// shadow-ok: joined on the exact routine key and version; a run by id
	err = s.db.QueryRowContext(ctx, `SELECT rv.state_at, rv.state_by FROM routine_versions rv JOIN runs r ON r.routine = rv.routine AND r.version = rv.version
		WHERE r.id = ? AND rv.state = 'retired' AND rv.retired_kind = ?`, runID, RetiredRolledBack).Scan(&at, &by)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", false, nil
	}
	return at, by, err == nil, err
}

// RerecordArgs is a request to re-record the hash of a live or previous version from the folder as it is now.
type RerecordArgs struct {
	Routine string
	Version int
	// Hash and Manifest are the version's as flotillad loaded it from the folder.
	Hash     string
	Manifest map[string]string
	// Legacy: the loaded version is a flat spec's v1.
	Legacy             bool
	Actor, Via, Reason string
}

// RerecordResult is what a re-record changed.
type RerecordResult struct {
	OldHash, NewHash string
	Changed          []string // the manifest keys that differ, as spec.ChangedFiles names them
}

// Rerecord replaces the recorded hash and manifest of a live or previous version with the loaded folder's, in one
// transaction with a version.rerecord event that carries the old and the new hash and what changed. It is the
// operator's controlled way past the freeze when a change to a live version is deliberate: a flat routine moved to
// the folder layout, a routine removed and added again. A shadow follows its folder by itself and a retired version
// is history, so neither is re-recorded.
func (s *Store) Rerecord(ctx context.Context, a RerecordArgs) (RerecordResult, error) {
	var res RerecordResult
	if strings.TrimSpace(a.Actor) == "" || strings.TrimSpace(a.Reason) == "" {
		return res, fmt.Errorf("%w: a re-record needs who and a reason", ErrVersionInvalid)
	}
	if a.Hash == "" {
		return res, fmt.Errorf("%w: a re-record needs the loaded version's hash", ErrVersionInvalid)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return res, err
	}
	defer tx.Rollback()
	v, err := s.rowTx(ctx, tx, a.Routine, a.Version)
	if err != nil {
		return res, err
	}
	switch v.State {
	case VersionLive, VersionPrevious:
	case VersionShadow:
		return res, fmt.Errorf("%w: %s v%d is a shadow: its hash follows its folder, nothing to re-record", ErrVersionConflict, a.Routine, a.Version)
	default:
		return res, fmt.Errorf("%w: %s v%d is retired: it is history and is not re-recorded", ErrVersionConflict, a.Routine, a.Version)
	}
	if v.Hash == a.Hash {
		return res, fmt.Errorf("%w: %s v%d already has this hash (%s): nothing to re-record", ErrVersionConflict, a.Routine, a.Version, a.Hash)
	}
	manifest, _ := json.Marshal(a.Manifest)
	at := s.now().Format(ts)
	if _, err := tx.ExecContext(ctx, `UPDATE routine_versions SET hash = ?, manifest = ? WHERE routine = ? AND version = ?`,
		a.Hash, string(manifest), a.Routine, a.Version); err != nil {
		return res, err
	}
	res = RerecordResult{OldHash: v.Hash, NewHash: a.Hash, Changed: spec.ChangedFiles(v.Manifest, a.Manifest)}
	layout := "folder"
	if a.Legacy {
		layout = "flat"
	}
	detail := fmt.Sprintf("%s v%d re-recorded (loaded as %s): hash %s -> %s; changed: %s: %s", v.State, a.Version, layout, v.Hash, a.Hash,
		strings.Join(res.Changed, ", "), a.Reason)
	if err := addVersionEvent(ctx, tx, at, a.Actor, a.Via, "version.rerecord", a.Routine, a.Version, detail); err != nil {
		return res, err
	}
	return res, tx.Commit()
}
