package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// A live run's reads (RFC-0014 section 4.8): what the run's stations read through MCP servers, one record line each
// (runtime/reads.py: station, tool, args, result, ok, plus chars, cut, at and duration_s). The input is frozen when the run starts
// and the reads happen during it, so they are a second write after the run, keyed by the live run's id as the input is, and joined
// to the input when it is read. The table is keyed by a live run's id only: a shadow run never writes it.

// InputReads is the record of a live run's reads. Complete is false when the worker could not read the whole record (a line it could
// not parse, a file it could not open, more lines than it returns): a replay of an incomplete record would miss reads, so it is
// not served as one.
type InputReads struct {
	Lines    []json.RawMessage
	Complete bool
}

// RecordInputReads keeps a live run's reads under its id, replacing an earlier record of the same run. Each line must be a JSON
// object; a line that is not is refused, so a record is never half of what the worker sent.
func (s *Store) RecordInputReads(ctx context.Context, runID string, lines []json.RawMessage, complete bool) error {
	if runID == "" {
		return errors.New("reads need the live run's id")
	}
	for i, l := range lines {
		var obj map[string]json.RawMessage
		if json.Unmarshal(l, &obj) != nil || obj == nil {
			return fmt.Errorf("reads: line %d is not a JSON object", i)
		}
	}
	raw, err := json.Marshal(nonNilRaw(lines))
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO input_reads (run_id, lines, complete) VALUES (?, ?, ?)
		ON CONFLICT(run_id) DO UPDATE SET lines = excluded.lines, complete = excluded.complete`, runID, string(raw), complete)
	return err
}

func nonNilRaw(l []json.RawMessage) []json.RawMessage {
	if l == nil {
		return []json.RawMessage{}
	}
	return l
}

// InputReadsOf is a live run's recorded reads; ok is false when the run recorded none (a run without reads, or one whose record
// was not kept).
func (s *Store) InputReadsOf(ctx context.Context, runID string) (r InputReads, ok bool, err error) {
	var raw string
	// shadow-ok: keyed by a live run's id; a shadow run never writes input_reads
	err = s.rdb.QueryRowContext(ctx, `SELECT lines, complete FROM input_reads WHERE run_id = ?`, runID).Scan(&raw, &r.Complete)
	if errors.Is(err, sql.ErrNoRows) {
		return InputReads{}, false, nil
	}
	if err != nil {
		return InputReads{}, false, err
	}
	if err := json.Unmarshal([]byte(raw), &r.Lines); err != nil {
		return InputReads{}, false, fmt.Errorf("input_reads %s: %w", runID, err)
	}
	return r, true, nil
}
