// Package store is flotillad's state: runs, an append-only event log, controls (pause, STOP,
// autonomy) and day spend, in one SQLite file. Only flotillad opens it; workers report through the API.
//
// The decisions ledger arrives in phase 3; phase 2 records runs.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// schema is applied in order; a database records how many steps it has. Steps are append-only:
// never edit one that has shipped, add the next.
var schema = []string{
	`CREATE TABLE runs (
		id TEXT PRIMARY KEY,
		job TEXT NOT NULL,
		spec_hash TEXT NOT NULL,
		trigger TEXT NOT NULL,
		actor TEXT NOT NULL,
		stage TEXT NOT NULL,
		started_at TEXT NOT NULL,
		ended_at TEXT,
		status TEXT NOT NULL,
		exit_code INTEGER,
		spend_usd REAL NOT NULL DEFAULT 0,
		duration_s REAL,
		trace_id TEXT,
		detail TEXT NOT NULL DEFAULT ''
	);
	CREATE INDEX runs_job_started ON runs(job, started_at);
	CREATE TABLE events (
		seq INTEGER PRIMARY KEY AUTOINCREMENT,
		at TEXT NOT NULL,
		actor TEXT NOT NULL,
		via TEXT NOT NULL,
		kind TEXT NOT NULL,
		subject TEXT NOT NULL,
		detail TEXT NOT NULL DEFAULT ''
	);
	CREATE TABLE controls (
		subject TEXT NOT NULL,
		key TEXT NOT NULL,
		value TEXT NOT NULL,
		actor TEXT NOT NULL,
		at TEXT NOT NULL,
		PRIMARY KEY (subject, key)
	);
	CREATE TABLE spend (
		day TEXT NOT NULL,
		subject TEXT NOT NULL,
		usd REAL NOT NULL,
		PRIMARY KEY (day, subject)
	);`,
	// ADR-0007: "routine" replaces "job".
	`ALTER TABLE runs RENAME COLUMN job TO routine;
	DROP INDEX runs_job_started;
	CREATE INDEX runs_routine_started ON runs(routine, started_at);`,
	// ADR-0008: one autonomy scale; a routine's "stage" is its autonomy, and "live" is "auto".
	`ALTER TABLE runs RENAME COLUMN stage TO autonomy;
	UPDATE runs SET autonomy = 'auto' WHERE autonomy = 'live';
	UPDATE controls SET key = 'autonomy' WHERE key = 'stage';
	UPDATE controls SET value = 'auto' WHERE key = 'autonomy' AND value = 'live';`,
	// RFC-0002 §9 step 5: the fleets' ledger (ledger.go). A namespace per ledger (a routine's: live and a
	// candidate never share one). Filings are the dedup memory (the triage fleet's filed.tsv): every write
	// is a new row and the newest row of a fingerprint is the current one, so nothing is rewritten; its
	// keys are rows of ledger_keys. Entries are the decision ledger (dispositions.tsv): decisions and
	// recurrences in one sequence, each row kept whole as the fleet wrote it.
	`CREATE TABLE ledger_filings (
		seq INTEGER PRIMARY KEY AUTOINCREMENT,
		namespace TEXT NOT NULL,
		fingerprint TEXT NOT NULL,
		row TEXT NOT NULL,
		imported INTEGER NOT NULL DEFAULT 0,
		run_id TEXT NOT NULL DEFAULT '',
		written_at TEXT NOT NULL
	);
	CREATE INDEX ledger_filings_fp ON ledger_filings(namespace, fingerprint, seq);
	CREATE TABLE ledger_keys (
		filing_seq INTEGER NOT NULL REFERENCES ledger_filings(seq),
		namespace TEXT NOT NULL,
		kind TEXT NOT NULL,
		value TEXT NOT NULL
	);
	CREATE INDEX ledger_keys_kv ON ledger_keys(namespace, kind, value);
	CREATE TABLE ledger_entries (
		seq INTEGER PRIMARY KEY AUTOINCREMENT,
		namespace TEXT NOT NULL,
		kind TEXT NOT NULL,
		occurrence_id TEXT NOT NULL DEFAULT '',
		fingerprint TEXT NOT NULL DEFAULT '',
		row TEXT NOT NULL,
		imported INTEGER NOT NULL DEFAULT 0,
		run_id TEXT NOT NULL DEFAULT '',
		written_at TEXT NOT NULL
	);
	CREATE INDEX ledger_entries_occ ON ledger_entries(namespace, occurrence_id, seq);
	CREATE INDEX ledger_entries_fp ON ledger_entries(namespace, fingerprint, seq);`,
	// ADR-0014: people (users.go). Only token hashes are kept; a revoked user stays, so a name is never
	// reused. A session row holds the hash of its cookie's id.
	`CREATE TABLE users (
		name TEXT PRIMARY KEY,
		role TEXT NOT NULL,
		token_sha256 TEXT NOT NULL UNIQUE,
		created_at TEXT NOT NULL,
		revoked_at TEXT
	);
	CREATE TABLE sessions (
		id_sha256 TEXT PRIMARY KEY,
		user_name TEXT NOT NULL REFERENCES users(name),
		csrf TEXT NOT NULL,
		created_at TEXT NOT NULL,
		last_seen TEXT NOT NULL
	);
	CREATE INDEX sessions_user ON sessions(user_name);`,
}

// Run statuses. "interrupted" is a run flotillad found unfinished when it started.
const (
	StatusRunning     = "running"
	StatusOK          = "ok"
	StatusError       = "error"
	StatusTimeout     = "timeout"
	StatusSkipped     = "skipped"
	StatusInterrupted = "interrupted"
)

type Store struct {
	db  *sql.DB
	now func() time.Time
}

type Run struct {
	ID        string   `json:"id"`
	Routine   string   `json:"routine"`
	SpecHash  string   `json:"spec_hash"`
	Trigger   string   `json:"trigger"`
	Actor     string   `json:"actor"`
	Autonomy  string   `json:"autonomy"`
	StartedAt string   `json:"started_at"`
	EndedAt   *string  `json:"ended_at"`
	Status    string   `json:"status"`
	ExitCode  *int     `json:"exit_code"`
	SpendUSD  float64  `json:"spend_usd"`
	DurationS *float64 `json:"duration_s"`
	TraceID   string   `json:"trace_id"`
	Detail    string   `json:"detail"`
}

type Event struct {
	Seq     int64  `json:"seq"`
	At      string `json:"at"`
	Actor   string `json:"actor"`
	Via     string `json:"via"`
	Kind    string `json:"kind"`
	Subject string `json:"subject"`
	Detail  string `json:"detail"`
}

type Control struct {
	Subject string `json:"subject"`
	Key     string `json:"key"`
	Value   string `json:"value"`
	Actor   string `json:"actor"`
	At      string `json:"at"`
}

const ts = time.RFC3339Nano

// Open creates or migrates the database at path.
func Open(path string) (*Store, error) {
	// temp_store(2): temporary tables and indexes in memory. flotillad's container has a read-only root
	// and no writable temp directory, so a statement that spills (schema step 3's UPDATE over the mini's
	// runs, 2026-11-11) failed with "disk I/O error (6410)". The store is small.
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=temp_store(2)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // one writer; SQLite serialises anyway, this keeps "database is locked" away
	s := &Store{db: db, now: func() time.Time { return time.Now().UTC() }}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS schema_version (n INTEGER NOT NULL)`); err != nil {
		return err
	}
	var n int
	if err := s.db.QueryRow(`SELECT COALESCE(MAX(n), 0) FROM schema_version`).Scan(&n); err != nil {
		return err
	}
	if n > len(schema) {
		return fmt.Errorf("database schema %d is newer than this flotillad (%d): refusing to run an older binary on it", n, len(schema))
	}
	for i := n; i < len(schema); i++ {
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(schema[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("schema step %d: %w", i+1, err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_version (n) VALUES (?)`, i+1); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// Event appends to the audit log. Nothing updates or deletes events.
func (s *Store) Event(ctx context.Context, actor, via, kind, subject, detail string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO events (at, actor, via, kind, subject, detail) VALUES (?, ?, ?, ?, ?, ?)`,
		s.now().Format(ts), actor, via, kind, subject, detail)
	return err
}

func (s *Store) Events(ctx context.Context, limit int) ([]Event, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT seq, at, actor, via, kind, subject, detail FROM events ORDER BY seq DESC LIMIT ?`, limit)
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

// SetControl sets one control and records who did it, in one transaction.
func (s *Store) SetControl(ctx context.Context, subject, key, value, actor, via string) error {
	return s.SetControlBecause(ctx, subject, key, value, actor, via, "")
}

// SetControlBecause is SetControl with the reason a loosening control needs (ADR-0014): the event's
// detail is the value, then the reason.
func (s *Store) SetControlBecause(ctx context.Context, subject, key, value, actor, via, reason string) error {
	detail := value
	if reason != "" {
		detail = value + ": " + reason
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	at := s.now().Format(ts)
	if _, err := tx.ExecContext(ctx, `INSERT INTO controls (subject, key, value, actor, at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(subject, key) DO UPDATE SET value = excluded.value, actor = excluded.actor, at = excluded.at`,
		subject, key, value, actor, at); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO events (at, actor, via, kind, subject, detail) VALUES (?, ?, ?, ?, ?, ?)`,
		at, actor, via, "control."+key, subject, detail); err != nil {
		return err
	}
	return tx.Commit()
}

// Control returns one control's value, or "" when it was never set.
func (s *Store) Control(ctx context.Context, subject, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM controls WHERE subject = ? AND key = ?`, subject, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

func (s *Store) Controls(ctx context.Context) ([]Control, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT subject, key, value, actor, at FROM controls ORDER BY subject, key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Control
	for rows.Next() {
		var c Control
		if err := rows.Scan(&c.Subject, &c.Key, &c.Value, &c.Actor, &c.At); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) StartRun(ctx context.Context, r Run) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO runs (id, routine, spec_hash, trigger, actor, autonomy, started_at, status, trace_id, detail)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.Routine, r.SpecHash, r.Trigger, r.Actor, r.Autonomy, r.StartedAt, r.Status, r.TraceID, r.Detail)
	return err
}

// FinishRun closes a running run. A run already closed is left alone and reported.
func (s *Store) FinishRun(ctx context.Context, id, status string, exitCode *int, spend, durationS float64, detail string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE runs SET ended_at = ?, status = ?, exit_code = ?, spend_usd = ?, duration_s = ?, detail = ?
		WHERE id = ? AND status = ?`,
		s.now().Format(ts), status, exitCode, spend, durationS, detail, id, StatusRunning)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("run %s is not running", id)
	}
	return nil
}

// SkippedRun records a trigger that did not start a run, with the reason, so a skip is visible.
func (s *Store) SkippedRun(ctx context.Context, r Run) error {
	now := s.now().Format(ts)
	r.StartedAt, r.Status = now, StatusSkipped
	_, err := s.db.ExecContext(ctx, `INSERT INTO runs (id, routine, spec_hash, trigger, actor, autonomy, started_at, ended_at, status, trace_id, detail)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.Routine, r.SpecHash, r.Trigger, r.Actor, r.Autonomy, now, now, r.Status, r.TraceID, r.Detail)
	return err
}

// MarkInterrupted closes runs left running by a previous flotillad and returns how many.
func (s *Store) MarkInterrupted(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE runs SET status = ?, ended_at = ?, detail = 'flotillad restarted while it ran'
		WHERE status = ?`, StatusInterrupted, s.now().Format(ts), StatusRunning)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (s *Store) Runs(ctx context.Context, routine string, limit int) ([]Run, error) {
	q := `SELECT id, routine, spec_hash, trigger, actor, autonomy, started_at, ended_at, status, exit_code, spend_usd, duration_s, COALESCE(trace_id, ''), detail
		FROM runs`
	var args []any
	if routine != "" {
		q += ` WHERE routine = ?`
		args = append(args, routine)
	}
	q += ` ORDER BY started_at DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		var r Run
		if err := rows.Scan(&r.ID, &r.Routine, &r.SpecHash, &r.Trigger, &r.Actor, &r.Autonomy, &r.StartedAt, &r.EndedAt,
			&r.Status, &r.ExitCode, &r.SpendUSD, &r.DurationS, &r.TraceID, &r.Detail); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// LastSuccess is the end time of the routine's newest ok run, zero when it has none.
func (s *Store) LastSuccess(ctx context.Context, routine string) (time.Time, error) {
	var at sql.NullString
	if err := s.db.QueryRowContext(ctx, `SELECT MAX(ended_at) FROM runs WHERE routine = ? AND status = ?`, routine, StatusOK).Scan(&at); err != nil {
		return time.Time{}, err
	}
	if !at.Valid {
		return time.Time{}, nil
	}
	return time.Parse(ts, at.String)
}

// AddSpend adds to a subject's (a routine id's) spend for a local day ("2006-01-02") and returns the new total.
func (s *Store) AddSpend(ctx context.Context, day, subject string, usd float64) (float64, error) {
	if _, err := s.db.ExecContext(ctx, `INSERT INTO spend (day, subject, usd) VALUES (?, ?, ?)
		ON CONFLICT(day, subject) DO UPDATE SET usd = usd + excluded.usd`, day, subject, usd); err != nil {
		return 0, err
	}
	return s.Spend(ctx, day, subject)
}

func (s *Store) Spend(ctx context.Context, day, subject string) (float64, error) {
	var v float64
	err := s.db.QueryRowContext(ctx, `SELECT usd FROM spend WHERE day = ? AND subject = ?`, day, subject).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return v, err
}

// Backup writes a consistent copy of the database to path (SQLite VACUUM INTO).
func (s *Store) Backup(ctx context.Context, path string) error {
	_, err := s.db.ExecContext(ctx, `VACUUM INTO ?`, path)
	return err
}
