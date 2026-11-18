package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
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
const runVersion = `COALESCE((SELECT version FROM runs WHERE id = ?), 1)`

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
}

// LoadedVersion is a version as flotillad loaded it from the fleets folder.
type LoadedVersion struct {
	Version  int
	Hash     string
	Manifest map[string]string
}

const versionCols = `routine, version, hash, manifest, state, deployed_at, live_at, state_by, state_at, state_why, retired_kind, autonomy_at_promote`

func scanVersion(sc interface{ Scan(...any) error }) (RoutineVersion, error) {
	var v RoutineVersion
	var manifest string
	if err := sc.Scan(&v.Routine, &v.Version, &v.Hash, &manifest, &v.State, &v.DeployedAt, &v.LiveAt, &v.StateBy, &v.StateAt,
		&v.StateWhy, &v.RetiredKind, &v.AutonomyAtPromote); err != nil {
		return v, err
	}
	_ = json.Unmarshal([]byte(manifest), &v.Manifest)
	return v, nil
}

type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func versionsOf(ctx context.Context, q queryer, where string, args ...any) ([]RoutineVersion, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+versionCols+` FROM routine_versions `+where+` ORDER BY routine, version`, args...)
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

// PairsFor is how many compared pairs a version has (decisions it and the live version both made on one
// input). Shadows record nothing until build step 2, so it is 0.
func (s *Store) PairsFor(ctx context.Context, routine string, version int) (int, error) {
	return 0, nil
}

func addVersionEvent(ctx context.Context, tx *sql.Tx, at, actor, via, kind, routine string, version int, detail string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO events (at, actor, via, kind, subject, detail) VALUES (?, ?, ?, ?, ?, ?)`,
		at, actor, via, kind, fmt.Sprintf("%s@v%d", routine, version), detail)
	return err
}

// EnsureVersions records what flotillad loaded, at its start: the first versions of a routine with none
// recorded (the lowest goes live, "deploy"; any others arrive as shadows), and a new number of a routine
// that has some always as a shadow. A shadow that changed on disk gets its new hash; a live, previous or
// retired version is never touched here (its freeze is checked by the caller). It reports what it did.
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
		}
	}
	return res, tx.Commit()
}

// EnsureResult is what EnsureVersions changed: the version that went live as the routine's first, new
// shadows, and shadows whose hash moved.
type EnsureResult struct {
	Live    int
	Shadows []int
	Updated []int
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
	// WithoutPairs: the operator promotes with no compared pairs. A reason is then required (always, until
	// shadows record pairs in build step 2).
	WithoutPairs bool
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
	if pairs == 0 && strings.TrimSpace(a.Reason) == "" {
		return fmt.Errorf("%w: v%d has no compared pairs: give a reason to promote it without", ErrVersionInvalid, a.Version)
	}
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
	if _, err := tx.ExecContext(ctx, `UPDATE routine_versions SET state = 'live', live_at = ?, state_by = ?, state_at = ?, state_why = ?, autonomy_at_promote = ?
		WHERE routine = ? AND version = ?`, at, a.Actor, at, a.Reason, a.Autonomy, a.Routine, a.Version); err != nil {
		return err
	}
	detail := "autonomy " + a.Autonomy
	if hasLive {
		detail += fmt.Sprintf("; v%d is previous", live.Version)
	}
	if pairs == 0 {
		detail += "; no compared pairs"
	}
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
	for _, id := range planIDs {
		if _, err := tx.ExecContext(ctx, `UPDATE plans SET rolled_back_at = ?, rolled_back_by = ? WHERE id = ?`, at, a.Actor, id); err != nil {
			return res, err
		}
	}
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
