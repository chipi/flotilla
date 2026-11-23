package store

import (
	"context"
	"fmt"
)

// Loaded is one thing a run read into a model's context (G7): a playbook or skill (trust "source"), a
// memory (an earlier case, a note: "memory"), or outside data (a signal, a probe: "untrusted").
type Loaded struct {
	RunID   string `json:"run_id"`
	Routine string `json:"routine"`
	At      string `json:"at"`
	Kind    string `json:"kind"`
	Trust   string `json:"trust"`
	Title   string `json:"title"`
	Source  string `json:"source"`
	SHA256  string `json:"sha256"`
	Version int    `json:"version,omitempty"` // the routine's version of the run that read it; set when read
}

// Trust levels.
const (
	TrustSource    = "source"
	TrustMemory    = "memory"
	TrustUntrusted = "untrusted"
)

// RecordLoaded keeps what a run read, in one transaction. A line with an unknown trust is refused, so
// nothing reads as trusted by a typo.
func (s *Store) RecordLoaded(ctx context.Context, items []Loaded) error {
	if len(items) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, l := range items {
		if l.Trust != TrustSource && l.Trust != TrustMemory && l.Trust != TrustUntrusted {
			return fmt.Errorf("loaded: trust %q is not source, memory or untrusted", l.Trust)
		}
		at := l.At
		if at == "" {
			at = s.now().Format(ts)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO run_loaded (run_id, routine, at, kind, trust, title, source, sha256, version)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, `+runVersion+`)`, l.RunID, l.Routine, at, l.Kind, l.Trust, l.Title, l.Source, l.SHA256, l.RunID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// RunLoaded is what each of these runs read, in the order read.
func (s *Store) RunLoaded(ctx context.Context, runIDs []string) (map[string][]Loaded, error) {
	out := map[string][]Loaded{}
	for _, id := range runIDs {
		rows, err := s.rdb.QueryContext(ctx, `SELECT run_id, routine, at, kind, trust, title, source, sha256, version FROM run_loaded
			WHERE run_id = ? ORDER BY id`, id)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var l Loaded
			if err := rows.Scan(&l.RunID, &l.Routine, &l.At, &l.Kind, &l.Trust, &l.Title, &l.Source, &l.SHA256, &l.Version); err != nil {
				rows.Close()
				return nil, err
			}
			out[id] = append(out[id], l)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}
