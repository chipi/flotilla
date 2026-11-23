package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

// Reads for the UI (ADR-0014, slice 1): one run, and the decision ledger newest-first.

// Run returns one run, or nil when there is none with that id.
func (s *Store) Run(ctx context.Context, id string) (*Run, error) {
	var r Run
	// shadow-ok: one run by id, whichever kind it is (a run page, a token's run)
	err := s.rdb.QueryRowContext(ctx, `SELECT `+runCols+` FROM runs WHERE id = ?`, id).
		Scan(&r.ID, &r.Routine, &r.SpecHash, &r.Trigger, &r.Actor, &r.Autonomy, &r.StartedAt, &r.EndedAt,
			&r.Status, &r.ExitCode, &r.SpendUSD, &r.DurationS, &r.TraceID, &r.Detail, &r.Version, &r.Shadow, &r.InputKey)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// Namespaces lists the ledger's live namespaces (a routine's: live and a candidate never share one). A shadow version's own
// namespace ("<fleet>/<routine>@v<n>") is not one: it is read as a pair, never as a ledger of its own.
func (s *Store) Namespaces(ctx context.Context) ([]string, error) {
	rows, err := s.rdb.QueryContext(ctx, `SELECT namespace FROM ledger_entries WHERE instr(namespace, '@v') = 0 GROUP BY namespace
		UNION SELECT namespace FROM ledger_filings WHERE instr(namespace, '@v') = 0 GROUP BY namespace ORDER BY namespace`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// Decision is one decision with how often its fingerprint recurred after it, before the next decision
// on that fingerprint. About 85% of a ledger's entries are recurrences, so History shows them as a count.
type Decision struct {
	Seq         int64             `json:"seq"`
	Row         map[string]string `json:"row"`
	Recurrences int               `json:"recurrences"`
}

// RecentDecisions returns a namespace's decisions newest first, those with seq below before (0: from
// the newest), at most limit.
func (s *Store) RecentDecisions(ctx context.Context, ns string, before int64, limit int) ([]Decision, error) {
	q := `SELECT e.seq, e.row,
		(SELECT COUNT(*) FROM ledger_entries r WHERE r.namespace = e.namespace AND r.kind = 'recurrence'
			AND r.fingerprint = e.fingerprint AND r.seq > e.seq
			AND r.seq < COALESCE((SELECT MIN(n.seq) FROM ledger_entries n WHERE n.namespace = e.namespace
				AND n.kind = 'decision' AND n.fingerprint = e.fingerprint AND n.seq > e.seq), 9223372036854775807))
		FROM ledger_entries e WHERE e.namespace = ? AND e.kind = 'decision'`
	args := []any{ns}
	if before > 0 {
		q += ` AND e.seq < ?`
		args = append(args, before)
	}
	q += ` ORDER BY e.seq DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.rdb.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Decision
	for rows.Next() {
		var d Decision
		var raw string
		if err := rows.Scan(&d.Seq, &raw, &d.Recurrences); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(raw), &d.Row); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
