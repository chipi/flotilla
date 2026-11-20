package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/chipi/flotilla/internal/spec"
)

// A record of each routine's own fields (docs/adr/ADR-0020 section 3): what routine.yaml held at the last start. A start compares what it loaded
// with the record; a field that moved is one routine.update event (old and new value), and the record follows. Nothing is refused for a field:
// they are not frozen, and a normal fleets deploy changes them.

// FieldsSchema is the schema step that added routine_snapshots.
const FieldsSchema = 33

// RoutineChange is what RecordRoutineFields found for one routine: First when there was no snapshot (one is written, no event), Changes the
// per-field diff lines when it moved.
type RoutineChange struct {
	Routine string
	First   bool
	Changes []string
}

// RecordRoutineFields writes the snapshot of each routine (fields: routine id to spec.Routine.RoutineFields) in one transaction. A routine with none gets
// one, silently (there is nothing to compare it with); one whose hash moved gets the new snapshot and one routine.update event, subject the routine,
// detail the per-field diff; an unchanged one writes nothing. It returns the routines that were first-recorded or changed, by id.
func (s *Store) RecordRoutineFields(ctx context.Context, fields map[string]map[string]string, actor string) ([]RoutineChange, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	ids := make([]string, 0, len(fields))
	for id := range fields {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	at := s.now().Format(ts)
	var out []RoutineChange
	for _, id := range ids {
		now := fields[id]
		hash := spec.FieldsHash(now)
		body, _ := json.Marshal(now)
		var oldHash, oldBody string
		err := tx.QueryRowContext(ctx, `SELECT hash, fields FROM routine_snapshots WHERE routine = ?`, id).Scan(&oldHash, &oldBody)
		switch {
		case err == sql.ErrNoRows:
			if _, err := tx.ExecContext(ctx, `INSERT INTO routine_snapshots (routine, hash, fields, updated_at) VALUES (?, ?, ?, ?)`, id, hash, string(body), at); err != nil {
				return nil, err
			}
			out = append(out, RoutineChange{Routine: id, First: true})
		case err != nil:
			return nil, err
		case oldHash != hash:
			var old map[string]string
			_ = json.Unmarshal([]byte(oldBody), &old)
			diff := spec.DiffFields(old, now)
			if _, err := tx.ExecContext(ctx, `UPDATE routine_snapshots SET hash = ?, fields = ?, updated_at = ? WHERE routine = ?`, hash, string(body), at, id); err != nil {
				return nil, err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO events (at, actor, via, kind, subject, detail) VALUES (?, ?, 'deploy', 'routine.update', ?, ?)`,
				at, actor, id, strings.Join(diff, "; ")); err != nil {
				return nil, err
			}
			out = append(out, RoutineChange{Routine: id, Changes: diff})
		}
	}
	return out, tx.Commit()
}

// FieldsRead is the recorded routine fields of a store read without writing it (flotillad -check). Exists is false for no file; Schema older than
// FieldsSchema reads as no snapshots.
type FieldsRead struct {
	Exists bool
	Schema int
	Fields map[string]map[string]string
}

// ReadRoutineFields reads the snapshots of the store at path read-only: nothing is created, migrated or written.
func ReadRoutineFields(path string) (FieldsRead, error) {
	out := FieldsRead{Fields: map[string]map[string]string{}}
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
	if out.Schema < FieldsSchema {
		return out, nil
	}
	rows, err := db.Query(`SELECT routine, fields FROM routine_snapshots`)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, body string
		if err := rows.Scan(&id, &body); err != nil {
			return out, err
		}
		m := map[string]string{}
		_ = json.Unmarshal([]byte(body), &m)
		out.Fields[id] = m
	}
	return out, rows.Err()
}

// RoutineChanges is the newest routine.update events of one routine, newest first: what its own fields changed to, with when and by whom.
func (s *Store) RoutineChanges(ctx context.Context, routine string, limit int) ([]Event, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT seq, at, actor, via, kind, subject, detail FROM events WHERE kind = 'routine.update' AND subject = ?
		ORDER BY seq DESC LIMIT ?`, routine, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.Seq, &e.At, &e.Actor, &e.Via, &e.Kind, &e.Subject, &e.Detail); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
