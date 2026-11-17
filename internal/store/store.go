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
	"os"
	"strings"
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
	// ADR-0005: cases (cases.go). One open case per canonical key is the partial unique index, so two
	// detections of one key at once cannot both open a case. The timeline is append-only, closed or not.
	`CREATE TABLE cases (
		id TEXT PRIMARY KEY,
		kind TEXT NOT NULL,
		target TEXT NOT NULL,
		canonical_key TEXT NOT NULL,
		project TEXT NOT NULL DEFAULT '',
		urgency INTEGER NOT NULL DEFAULT 0,
		status TEXT NOT NULL,
		resolution TEXT NOT NULL DEFAULT '',
		holder TEXT NOT NULL DEFAULT '',
		opened_at TEXT NOT NULL,
		status_at TEXT NOT NULL,
		closed_at TEXT
	);
	CREATE UNIQUE INDEX cases_one_open ON cases(canonical_key) WHERE status <> 'closed';
	CREATE INDEX cases_status ON cases(status, status_at);
	CREATE TABLE case_keys (
		case_id TEXT NOT NULL REFERENCES cases(id),
		kind TEXT NOT NULL,
		value TEXT NOT NULL,
		PRIMARY KEY (case_id, kind, value)
	);
	CREATE INDEX case_keys_lookup ON case_keys(kind, value);
	CREATE TABLE case_events (
		seq INTEGER PRIMARY KEY AUTOINCREMENT,
		case_id TEXT NOT NULL REFERENCES cases(id),
		at TEXT NOT NULL,
		kind TEXT NOT NULL,
		actor TEXT NOT NULL,
		by_role TEXT NOT NULL DEFAULT '',
		from_status TEXT NOT NULL DEFAULT '',
		to_status TEXT NOT NULL DEFAULT '',
		detail TEXT NOT NULL DEFAULT ''
	);
	CREATE INDEX case_events_case ON case_events(case_id, seq);`,
	// RFC-0004 §5.1 (plans.go): each plan policy decided, with its hash and every reason; the per-target
	// lock (one case at a time touches a target: the primary key); holds on a host or a target.
	`CREATE TABLE plans (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		case_id TEXT NOT NULL REFERENCES cases(id),
		run_id TEXT NOT NULL,
		action TEXT NOT NULL,
		params TEXT NOT NULL,
		plan_hash TEXT NOT NULL,
		outcome TEXT NOT NULL,
		autonomy TEXT NOT NULL,
		would TEXT NOT NULL DEFAULT '',
		reasons TEXT NOT NULL,
		decided_at TEXT NOT NULL
	);
	CREATE INDEX plans_case ON plans(case_id, id);
	CREATE TABLE target_locks (
		target TEXT PRIMARY KEY,
		case_id TEXT NOT NULL REFERENCES cases(id),
		taken_at TEXT NOT NULL
	);
	CREATE TABLE holds (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		scope TEXT NOT NULL,
		until TEXT NOT NULL,
		actor TEXT NOT NULL,
		reason TEXT NOT NULL,
		created_at TEXT NOT NULL,
		lifted_at TEXT,
		lifted_by TEXT
	);
	CREATE INDEX holds_scope ON holds(scope, until);`,
	// RFC-0004 §3.5: the job holding a case (cases.holder) holds it until its lease ends.
	`ALTER TABLE cases ADD COLUMN lease_until TEXT NOT NULL DEFAULT '';`,
	// ADR-0006 §3 and §10: hosts, their enrolment and their connection. A host's current certificate is
	// the only one accepted (renewal replaces it); revoking clears it. An enrolment token is kept as its
	// hash, used once. The connection columns are what flotillad last heard (hello, heartbeat, result).
	`CREATE TABLE hosts (
		name TEXT PRIMARY KEY,
		cert_sha256 TEXT NOT NULL DEFAULT '',
		cert_not_after TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL,
		enrolled_at TEXT,
		revoked_at TEXT,
		connected INTEGER NOT NULL DEFAULT 0,
		version TEXT NOT NULL DEFAULT '',
		connected_at TEXT NOT NULL DEFAULT '',
		last_contact TEXT NOT NULL DEFAULT '',
		clock_offset_ms INTEGER NOT NULL DEFAULT 0,
		chain_head TEXT NOT NULL DEFAULT ''
	);
	CREATE TABLE host_enrolments (
		token_sha256 TEXT PRIMARY KEY,
		host TEXT NOT NULL,
		created_by TEXT NOT NULL,
		created_at TEXT NOT NULL,
		expires_at TEXT NOT NULL,
		used_at TEXT
	);
	CREATE TABLE host_executions (
		id TEXT PRIMARY KEY,
		case_id TEXT NOT NULL,
		plan_id INTEGER NOT NULL,
		host TEXT NOT NULL,
		action TEXT NOT NULL,
		approved TEXT NOT NULL,
		envelope BLOB NOT NULL,
		status TEXT NOT NULL,
		exit_code INTEGER NOT NULL DEFAULT 0,
		output_sha256 TEXT NOT NULL DEFAULT '',
		detail TEXT NOT NULL DEFAULT '',
		chain_head TEXT NOT NULL DEFAULT '',
		sent_at TEXT NOT NULL,
		ended_at TEXT
	);
	CREATE INDEX host_executions_case ON host_executions(case_id);`,
	// The UI's Now (mockup v3): "since you looked" is the end of the user's previous visit (views.go).
	`ALTER TABLE users ADD COLUMN now_seen_at TEXT NOT NULL DEFAULT '';
	ALTER TABLE users ADD COLUMN now_since TEXT NOT NULL DEFAULT '';`,
	// Each model call of a run (calls.go): which model, its tokens and cost, as the model client recorded it.
	`CREATE TABLE run_calls (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		run_id TEXT NOT NULL,
		routine TEXT NOT NULL,
		at TEXT NOT NULL,
		alias TEXT NOT NULL DEFAULT '',
		endpoint TEXT NOT NULL DEFAULT '',
		model TEXT NOT NULL DEFAULT '',
		deployment TEXT NOT NULL DEFAULT '',
		tokens_in INTEGER,
		tokens_out INTEGER,
		cost_usd REAL,
		cost_source TEXT NOT NULL DEFAULT '',
		latency_s REAL,
		error TEXT NOT NULL DEFAULT ''
	);
	CREATE INDEX run_calls_run ON run_calls(run_id);
	CREATE INDEX run_calls_at ON run_calls(at);`,
	// G3: a detection's one-line problem sentence, which the UI shows as the case's title.
	`ALTER TABLE cases ADD COLUMN title TEXT NOT NULL DEFAULT '';`,
	// G8 (feedback.go): a person's verdict on a case's diagnosis, and the notes they propose, which take
	// effect only once activated.
	`CREATE TABLE case_feedback (
		case_id TEXT NOT NULL REFERENCES cases(id),
		user_name TEXT NOT NULL,
		verdict TEXT NOT NULL,
		at TEXT NOT NULL,
		PRIMARY KEY (case_id, user_name)
	);
	CREATE TABLE notes (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		scope TEXT NOT NULL,
		text TEXT NOT NULL,
		from_case TEXT NOT NULL DEFAULT '',
		by_user TEXT NOT NULL,
		status TEXT NOT NULL,
		created_at TEXT NOT NULL,
		decided_at TEXT NOT NULL DEFAULT '',
		decided_by TEXT NOT NULL DEFAULT ''
	);
	CREATE INDEX notes_status ON notes(status, id);`,
	// G10: what a host's agent reported of its own config at its last hello (for people only).
	`ALTER TABLE hosts ADD COLUMN ceiling TEXT NOT NULL DEFAULT '';
	ALTER TABLE hosts ADD COLUMN allowlist TEXT NOT NULL DEFAULT '';`,
	// G14: the recurrence rule (recurrence.go): the earlier case a new one comes back after.
	`ALTER TABLE cases ADD COLUMN recurrence_of TEXT NOT NULL DEFAULT '';
	CREATE INDEX cases_kind_target ON cases(kind, target, closed_at);`,
	// G15: a contract check's result, kept on its call's row (runtime/contract_routine.py).
	`ALTER TABLE run_calls ADD COLUMN check_name TEXT NOT NULL DEFAULT '';
	ALTER TABLE run_calls ADD COLUMN check_ok INTEGER;
	ALTER TABLE run_calls ADD COLUMN detail TEXT NOT NULL DEFAULT '';`,
	// G7: what a run read into a model's context, and how far each was trusted (runtime/loaded.py).
	`CREATE TABLE run_loaded (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		run_id TEXT NOT NULL,
		routine TEXT NOT NULL,
		at TEXT NOT NULL,
		kind TEXT NOT NULL,
		trust TEXT NOT NULL,
		title TEXT NOT NULL,
		source TEXT NOT NULL,
		sha256 TEXT NOT NULL DEFAULT ''
	);
	CREATE INDEX run_loaded_run ON run_loaded(run_id);`,
	// G11: the morning brief, one per day as written, and people's marks on its items (briefs.go).
	`CREATE TABLE briefs (
		day TEXT PRIMARY KEY,
		written_at TEXT NOT NULL,
		body TEXT NOT NULL
	);
	CREATE TABLE brief_marks (
		day TEXT NOT NULL,
		item INTEGER NOT NULL,
		kind TEXT NOT NULL,
		user_name TEXT NOT NULL,
		mark TEXT NOT NULL,
		at TEXT NOT NULL,
		PRIMARY KEY (day, item, user_name)
	);
	CREATE INDEX brief_marks_at ON brief_marks(at);`,
	// G9: each plan asked in Slack, with its single-use nonce and the answer (slack.go).
	`CREATE TABLE slack_asks (
		nonce TEXT PRIMARY KEY,
		case_id TEXT NOT NULL,
		plan_hash TEXT NOT NULL,
		asked_at TEXT NOT NULL,
		sent_at TEXT NOT NULL DEFAULT '',
		decided_at TEXT NOT NULL DEFAULT '',
		decided_by TEXT NOT NULL DEFAULT '',
		outcome TEXT NOT NULL DEFAULT '',
		UNIQUE (case_id, plan_hash)
	);
	CREATE INDEX slack_asks_decided ON slack_asks(outcome, decided_at);`,
	// ADR-0016: a detection's structured data (its measure, a proposal) on the case's timeline.
	`ALTER TABLE case_events ADD COLUMN data TEXT NOT NULL DEFAULT '';`,
	// A detection that repeats the case's last signal exactly (an issue still open, unchanged) is counted on
	// the case, not written as another event: a case's timeline no longer grows by one event an hour.
	`ALTER TABLE cases ADD COLUMN repeats INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE cases ADD COLUMN repeat_at TEXT NOT NULL DEFAULT '';`,
	// RFC-0013 §2: a case passes through routines in sequence (its stage, how often it changed), and its
	// acting runs share one budget and one time limit (what they have spent, in dollars and seconds).
	`ALTER TABLE cases ADD COLUMN stage TEXT NOT NULL DEFAULT '';
	ALTER TABLE cases ADD COLUMN stage_changes INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE cases ADD COLUMN spent_usd REAL NOT NULL DEFAULT 0;
	ALTER TABLE cases ADD COLUMN spent_s REAL NOT NULL DEFAULT 0;`,
	// What a model call recorded about its step: why it stopped, which turn of a harness step it was, and the decision
	// it served (run_calls); and what an eval measured for a routine, prompt and model (eval_results, eval.go).
	`ALTER TABLE run_calls ADD COLUMN finish_reason TEXT NOT NULL DEFAULT '';
	ALTER TABLE run_calls ADD COLUMN step INTEGER;
	ALTER TABLE run_calls ADD COLUMN decision_key TEXT;
	CREATE TABLE eval_results (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		routine TEXT NOT NULL,
		prompt_sha TEXT NOT NULL,
		model TEXT NOT NULL,
		metric TEXT NOT NULL,
		value REAL NOT NULL,
		n INTEGER NOT NULL DEFAULT 0,
		repeats INTEGER NOT NULL DEFAULT 0,
		source TEXT NOT NULL DEFAULT '',
		recorded_at TEXT NOT NULL,
		recorded_by TEXT NOT NULL
	);
	CREATE INDEX eval_results_key ON eval_results(routine, prompt_sha, model);`,
}

// Run statuses. "interrupted" is a run flotillad found unfinished when it started; "cancelled" is a run
// someone stopped in flight (a STOP of its fleet, or `flotilla cancel`): neither ok nor a failure.
const (
	StatusRunning     = "running"
	StatusOK          = "ok"
	StatusError       = "error"
	StatusTimeout     = "timeout"
	StatusSkipped     = "skipped"
	StatusInterrupted = "interrupted"
	StatusCancelled   = "cancelled"
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

// TimeFormat is how every time is stored (always UTC). SQL compares stored times as strings, so the
// layout is fixed-width: RFC3339Nano drops trailing zeros, and within one second a later time sorted as
// earlier ('.' before 'Z'). time.Parse with time.RFC3339Nano reads it, and rows stored before it.
const TimeFormat = "2006-01-02T15:04:05.000000000Z07:00"

const ts = TimeFormat

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

// errNewer is the refusal of a store ahead of this binary. deploy.sh reads the "(N)" for the newest
// schema this binary accepts, so the wording is part of its contract.
func errNewer(n int) error {
	return fmt.Errorf("database schema %d is newer than this flotillad (%d): refusing to run an older binary on it", n, len(schema))
}

// CheckSchema is what flotillad -check does with the store: it opens it read-only and returns the
// refusal Open would give when the store is ahead of this binary. It never creates, migrates or
// writes, so a deploy's check (and a rollback's, before anything is stopped) leaves the store as it
// was. A missing store is a first install.
func CheckSchema(path string) error {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return err
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT COALESCE(MAX(n), 0) FROM schema_version`).Scan(&n); err != nil {
		if strings.Contains(err.Error(), "no such table") {
			return nil // a file that never had a schema is migrated at start
		}
		return fmt.Errorf("store: %w", err)
	}
	if n > len(schema) {
		return errNewer(n)
	}
	return nil
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
		return errNewer(n)
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

// LatestEvent is the newest event of kind on subject, or nil.
func (s *Store) LatestEvent(ctx context.Context, kind, subject string) (*Event, error) {
	var e Event
	err := s.db.QueryRowContext(ctx, `SELECT seq, at, actor, via, kind, subject, detail FROM events WHERE kind = ? AND subject = ?
		ORDER BY seq DESC LIMIT 1`, kind, subject).Scan(&e.Seq, &e.At, &e.Actor, &e.Via, &e.Kind, &e.Subject, &e.Detail)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &e, nil
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

// ControlAt is one control's value and when it was last set ("", "" when it was never set).
func (s *Store) ControlAt(ctx context.Context, subject, key string) (string, string, error) {
	var v, at string
	err := s.db.QueryRowContext(ctx, `SELECT value, at FROM controls WHERE subject = ? AND key = ?`, subject, key).Scan(&v, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", nil
	}
	return v, at, err
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
