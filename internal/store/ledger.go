package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// The fleets' ledger (RFC-0002 §9 step 5). A row is the fleet's own record, kept whole as a JSON object
// of its columns (for the triage fleet: filed.tsv's and dispositions.tsv's columns), so the TSVs render
// back from the store byte for byte. The store indexes what lookups need: a filing's fingerprint and its
// keys, an entry's kind, occurrence and fingerprint.

// FilingKeyKinds are the keys a filing is looked up by, in the triage fleet's order (filing.ledger_lookup).
var FilingKeyKinds = []string{"fingerprint", "group_key", "norm_key", "alert_key"}

// Filing is a current filed row: the newest written for its fingerprint.
type Filing struct {
	Seq      int64             `json:"seq"`
	Row      map[string]string `json:"row"`
	Imported bool              `json:"imported"`
}

// Entry is a decision-ledger row: kind "decision" or "recurrence".
type Entry struct {
	Seq      int64             `json:"seq"`
	Kind     string            `json:"kind"`
	Row      map[string]string `json:"row"`
	Imported bool              `json:"imported"`
}

var EntryKinds = map[string]bool{"decision": true, "recurrence": true}

func rowJSON(row map[string]string) (string, error) {
	if row == nil {
		return "", errors.New("a ledger row is required")
	}
	b, err := json.Marshal(row)
	return string(b), err
}

// WriteFiling records a filing: a new row that becomes its fingerprint's current one.
func (s *Store) WriteFiling(ctx context.Context, ns string, row map[string]string, runID string) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	seq, err := writeFiling(ctx, tx, ns, row, false, runID, s.now().Format(ts))
	if err != nil {
		return 0, err
	}
	return seq, tx.Commit()
}

func writeFiling(ctx context.Context, tx *sql.Tx, ns string, row map[string]string, imported bool, runID, at string) (int64, error) {
	fp := row["fingerprint"]
	if ns == "" || fp == "" {
		return 0, errors.New("a filing needs a namespace and a fingerprint")
	}
	raw, err := rowJSON(row)
	if err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO ledger_filings (namespace, fingerprint, row, imported, run_id, written_at)
		VALUES (?, ?, ?, ?, ?, ?)`, ns, fp, raw, imported, runID, at)
	if err != nil {
		return 0, err
	}
	seq, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	for _, k := range FilingKeyKinds {
		if v := row[k]; v != "" { // an empty key never matches (filing.ledger_lookup)
			if _, err := tx.ExecContext(ctx, `INSERT INTO ledger_keys (filing_seq, namespace, kind, value) VALUES (?, ?, ?, ?)`,
				seq, ns, k, v); err != nil {
				return 0, err
			}
		}
	}
	return seq, nil
}

// Filings returns the current filings, oldest first (the order of filed.tsv): all of them when keys is
// empty, else those whose own keys match any of keys (kind -> value; empty values are ignored).
func (s *Store) Filings(ctx context.Context, ns string, keys map[string]string) ([]Filing, error) {
	q := `SELECT f.seq, f.row, f.imported FROM ledger_filings f
		WHERE f.namespace = ? AND f.seq = (SELECT MAX(g.seq) FROM ledger_filings g WHERE g.namespace = f.namespace AND g.fingerprint = f.fingerprint)`
	args := []any{ns}
	for k := range keys {
		if !contains(FilingKeyKinds, k) {
			return nil, fmt.Errorf("unknown filing key %q", k)
		}
	}
	if len(keys) > 0 {
		var match []string
		args = append(args, ns) // the subquery's namespace, before its key pairs
		for _, k := range FilingKeyKinds {
			if v := keys[k]; v != "" {
				match = append(match, `(kind = ? AND value = ?)`)
				args = append(args, k, v)
			}
		}
		if len(match) == 0 {
			return nil, nil
		}
		q += ` AND f.seq IN (SELECT filing_seq FROM ledger_keys WHERE namespace = ? AND (` + strings.Join(match, " OR ") + `))`
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

// AppendEntry records a decision or a recurrence.
func (s *Store) AppendEntry(ctx context.Context, ns, kind string, row map[string]string, runID string) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	seq, err := appendEntry(ctx, tx, ns, kind, row, false, runID, s.now().Format(ts))
	if err != nil {
		return 0, err
	}
	return seq, tx.Commit()
}

func appendEntry(ctx context.Context, tx *sql.Tx, ns, kind string, row map[string]string, imported bool, runID, at string) (int64, error) {
	if ns == "" || !EntryKinds[kind] {
		return 0, fmt.Errorf("an entry needs a namespace and a kind (decision or recurrence), got %q", kind)
	}
	raw, err := rowJSON(row)
	if err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO ledger_entries (namespace, kind, occurrence_id, fingerprint, row, imported, run_id, written_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, ns, kind, row["occurrence_id"], row["fingerprint"], raw, imported, runID, at)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// Entries returns entries in order, after the given seq, at most limit (0: no limit).
func (s *Store) Entries(ctx context.Context, ns string, afterSeq int64, limit int) ([]Entry, error) {
	q := `SELECT seq, kind, row, imported FROM ledger_entries WHERE namespace = ? AND seq > ? ORDER BY seq`
	args := []any{ns, afterSeq}
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

// LedgerCounts are a namespace's current filings and its entries by kind.
type LedgerCounts struct {
	Filings     int `json:"filings"`
	Decisions   int `json:"decisions"`
	Recurrences int `json:"recurrences"`
}

func (s *Store) LedgerCounts(ctx context.Context, ns string) (LedgerCounts, error) {
	var c LedgerCounts
	err := s.db.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(DISTINCT fingerprint) FROM ledger_filings WHERE namespace = ?),
		(SELECT COUNT(*) FROM ledger_entries WHERE namespace = ? AND kind = 'decision'),
		(SELECT COUNT(*) FROM ledger_entries WHERE namespace = ? AND kind = 'recurrence')`, ns, ns, ns).
		Scan(&c.Filings, &c.Decisions, &c.Recurrences)
	return c, err
}

// ImportLedger loads a namespace's history once, in one transaction, marked imported: the filings in
// their ledger order, the entries in theirs. It refuses a namespace that already has rows.
func (s *Store) ImportLedger(ctx context.Context, ns string, filings []map[string]string, entries []Entry) (LedgerCounts, error) {
	c, err := s.LedgerCounts(ctx, ns)
	if err != nil {
		return c, err
	}
	if c != (LedgerCounts{}) {
		return c, fmt.Errorf("ledger %s already has rows (%d filings, %d decisions, %d recurrences): an import is once", ns, c.Filings, c.Decisions, c.Recurrences)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return c, err
	}
	defer tx.Rollback()
	at := s.now().Format(ts)
	for i, f := range filings {
		if _, err := writeFiling(ctx, tx, ns, f, true, "", at); err != nil {
			return c, fmt.Errorf("filing %d: %w", i+1, err)
		}
	}
	for i, e := range entries {
		if _, err := appendEntry(ctx, tx, ns, e.Kind, e.Row, true, "", at); err != nil {
			return c, fmt.Errorf("entry %d: %w", i+1, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return c, err
	}
	return s.LedgerCounts(ctx, ns)
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}
