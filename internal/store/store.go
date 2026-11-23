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
	// Who made a call and in which agent session: the role, the episode (an id grouping one session's calls) and its
	// order within the run, and the tools the model called in that turn, for drawing a run's flow between roles.
	`ALTER TABLE run_calls ADD COLUMN role TEXT NOT NULL DEFAULT '';
	ALTER TABLE run_calls ADD COLUMN episode TEXT NOT NULL DEFAULT '';
	ALTER TABLE run_calls ADD COLUMN episode_seq INTEGER;
	ALTER TABLE run_calls ADD COLUMN tools TEXT NOT NULL DEFAULT '';`,
	// What a run_calls row is: a model call ('call') or a step the harness took between calls ('step': a verdict, a gate's outcome, a pin;
	// no model, tokens or cost), so a run's flow can show both. Aggregates over calls skip 'step' rows.
	`ALTER TABLE run_calls ADD COLUMN kind TEXT NOT NULL DEFAULT 'call';`,
	// Routine versions (v2 step 1, versions.go): which version of a routine is live, shadow, previous or retired, with
	// the hash it was recorded with and who moved it, when and why. A version file is the routine's folder v<n>/; the
	// manifest maps each file to its sha256 so a freeze failure can name what changed. One live and one previous per
	// routine are the partial unique indexes.
	`CREATE TABLE routine_versions (
		routine TEXT NOT NULL,
		version INTEGER NOT NULL,
		hash TEXT NOT NULL,
		manifest TEXT NOT NULL DEFAULT '{}',
		state TEXT NOT NULL CHECK (state IN ('live', 'shadow', 'previous', 'retired')),
		deployed_at TEXT NOT NULL,
		live_at TEXT NOT NULL DEFAULT '',
		state_by TEXT NOT NULL DEFAULT '',
		state_at TEXT NOT NULL DEFAULT '',
		state_why TEXT NOT NULL DEFAULT '',
		retired_kind TEXT NOT NULL DEFAULT '',
		autonomy_at_promote TEXT NOT NULL DEFAULT '',
		PRIMARY KEY (routine, version)
	);
	CREATE UNIQUE INDEX routine_versions_one_live ON routine_versions(routine) WHERE state = 'live';
	CREATE UNIQUE INDEX routine_versions_one_previous ON routine_versions(routine) WHERE state = 'previous';`,
	// Every record says which version made it: runs, calls, what a run loaded, plans, ledger rows (the version of the run
	// that wrote them; 1 for what came before versions) and case events (0 for an operator's or policy's, the acting
	// routine's live version for a routine's). A plan from a version that was rolled back is marked, so it cannot be approved.
	`ALTER TABLE runs ADD COLUMN version INTEGER NOT NULL DEFAULT 1;
	ALTER TABLE run_calls ADD COLUMN version INTEGER NOT NULL DEFAULT 1;
	ALTER TABLE run_loaded ADD COLUMN version INTEGER NOT NULL DEFAULT 1;
	ALTER TABLE plans ADD COLUMN version INTEGER NOT NULL DEFAULT 1;
	ALTER TABLE plans ADD COLUMN rolled_back_at TEXT;
	ALTER TABLE plans ADD COLUMN rolled_back_by TEXT;
	ALTER TABLE ledger_filings ADD COLUMN version INTEGER NOT NULL DEFAULT 1;
	ALTER TABLE ledger_entries ADD COLUMN version INTEGER NOT NULL DEFAULT 1;
	ALTER TABLE case_events ADD COLUMN version INTEGER NOT NULL DEFAULT 0;
	UPDATE case_events SET version = 1 WHERE actor LIKE '%/%';
	CREATE INDEX runs_routine_version ON runs(routine, version);`,
	// The lowest autonomy level, shadow, is renamed record (the word shadow belongs to versions). The stored values move
	// once: autonomy controls (routines and actions), runs and plans, a plan's policy outcome, a case's resolution, and a
	// host's reported ceiling. The events log and the case timeline's text are history and stay as written.
	`UPDATE controls SET value = 'record' WHERE key = 'autonomy' AND value = 'shadow';
	UPDATE runs SET autonomy = 'record' WHERE autonomy = 'shadow';
	UPDATE plans SET autonomy = 'record' WHERE autonomy = 'shadow';
	UPDATE plans SET outcome = 'record' WHERE outcome = 'shadow';
	UPDATE cases SET resolution = 'plan_recorded' WHERE resolution = 'shadow_plan_recorded';
	UPDATE hosts SET ceiling = 'record' WHERE ceiling = 'shadow';`,
	// What a promote recorded (v2 step 1): the acceptance in force and the evidence given, as JSON, on the version it made live.
	`ALTER TABLE routine_versions ADD COLUMN promote_evidence TEXT NOT NULL DEFAULT '';`,
	// Shadows with no effect (v2 step 2). A run says whether it is a shadow and which live run's input it was given; the
	// frozen input of a live run is kept under the live run's id (what flotillad handed it, the ledger's high-water mark,
	// the live case keys, the commit, the state snapshot). All additive: runs gains two columns with constant defaults.
	`ALTER TABLE runs ADD COLUMN shadow INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE runs ADD COLUMN input_key TEXT NOT NULL DEFAULT '';
	CREATE INDEX runs_routine_shadow ON runs(routine, shadow);
	CREATE TABLE run_inputs (
		input_key TEXT PRIMARY KEY,
		routine TEXT NOT NULL,
		kind TEXT NOT NULL,
		version INTEGER NOT NULL,
		trigger TEXT NOT NULL,
		actor TEXT NOT NULL,
		started_at TEXT NOT NULL,
		handed TEXT NOT NULL DEFAULT '{}',
		ledger_mark_filings INTEGER NOT NULL DEFAULT 0,
		ledger_mark_entries INTEGER NOT NULL DEFAULT 0,
		live_case_keys TEXT NOT NULL DEFAULT '[]',
		commit_repo TEXT NOT NULL DEFAULT '',
		commit_sha TEXT NOT NULL DEFAULT '',
		snapshot_paths TEXT NOT NULL DEFAULT '[]',
		snapshot_state TEXT NOT NULL DEFAULT 'none' CHECK (snapshot_state IN ('none', 'held', 'deleted')),
		pinned INTEGER NOT NULL DEFAULT 0,
		schema_version INTEGER NOT NULL DEFAULT 1
	);
	CREATE INDEX run_inputs_routine ON run_inputs(routine, started_at);`,
	// What a shadow did, kept apart from everything a live run writes: the queue of shadow runs (one per input and
	// version, with why one did not run) and the records of what a shadow would have written, each with its pairing key.
	`CREATE TABLE shadow_runs (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		input_key TEXT NOT NULL,
		routine TEXT NOT NULL,
		version INTEGER NOT NULL,
		version_hash TEXT NOT NULL DEFAULT '',
		status TEXT NOT NULL CHECK (status IN ('queued', 'running', 'done', 'not_run', 'cancelled')),
		reason TEXT NOT NULL DEFAULT '',
		run_id TEXT NOT NULL DEFAULT '',
		queued_at TEXT NOT NULL,
		started_at TEXT NOT NULL DEFAULT '',
		live_started_at TEXT NOT NULL DEFAULT '',
		ended_at TEXT NOT NULL DEFAULT '',
		UNIQUE (input_key, routine, version)
	);
	CREATE INDEX shadow_runs_status ON shadow_runs(status, queued_at);
	CREATE TABLE shadow_records (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		run_id TEXT NOT NULL,
		routine TEXT NOT NULL,
		version INTEGER NOT NULL,
		version_hash TEXT NOT NULL DEFAULT '',
		input_key TEXT NOT NULL,
		subject TEXT NOT NULL DEFAULT '',
		kind TEXT NOT NULL,
		payload TEXT NOT NULL DEFAULT '{}',
		created_at TEXT NOT NULL
	);
	CREATE INDEX shadow_records_input ON shadow_records(input_key, subject);
	CREATE INDEX shadow_records_routine ON shadow_records(routine, version);`,
	// call_texts holds bodies only: it has no run or routine key, and a body is reached only through a run's calls (their texts column names the
	// hash), so every reader of it is already guarded by the shadow rule on run_calls.
	// Stations (v2 build step 3): the station that made a call, the rendered texts it read (name, hash, length: JSON), and each distinct text once,
	// by its hash. A text's body is masked and capped when it is written; one over the cap, or past a run's budget, has its hash and length only.
	`ALTER TABLE run_calls ADD COLUMN station TEXT NOT NULL DEFAULT '';
	ALTER TABLE run_calls ADD COLUMN texts TEXT NOT NULL DEFAULT '';
	CREATE TABLE call_texts (
		sha256 TEXT PRIMARY KEY,
		bytes INTEGER NOT NULL,
		text TEXT,
		redacted INTEGER NOT NULL DEFAULT 0,
		over_cap INTEGER NOT NULL DEFAULT 0
	);`,
	// The routine's own fields as a record (ADR-0020 section 3): what routine.yaml (or a flat spec) held at the last start, as canonical JSON with a hash,
	// so a start can say which field moved (a routine.update event) and -check can say it before the deploy. Not a freeze: nothing is refused for them.
	`CREATE TABLE routine_snapshots (
		routine TEXT PRIMARY KEY,
		hash TEXT NOT NULL,
		fields TEXT NOT NULL,
		updated_at TEXT NOT NULL
	);`,
	// RFC-0014 §4.2: the delivery ids a hook has accepted, so a retry or a replay inside the signature's window opens no second
	// case. A row is dropped once it is older than twice the window; the case it opened stays.
	`CREATE TABLE hook_deliveries (
		hook TEXT NOT NULL,
		delivery TEXT NOT NULL,
		at TEXT NOT NULL,
		case_id TEXT NOT NULL DEFAULT '',
		PRIMARY KEY (hook, delivery)
	);`,
	// A pairs answer reads the live run's decisions per input by run_id (pairs.go liveSides); with no index that was a scan of the whole
	// ledger per input (2026-11-22, the installation's store, 204 inputs: 3.3 s natively and past the CLI's 30 s in the container; 0.15 s with it).
	`CREATE INDEX ledger_entries_run ON ledger_entries(run_id, kind, seq);`,
	// A case's fleet, kept when it opens (cases.go caseFleetOf) so the UI and the metrics read one answer: the opening routine's
	// fleet, or for a case a hook opened (no slash in its actor) its project. Back-filled by the same rule.
	`ALTER TABLE cases ADD COLUMN fleet TEXT NOT NULL DEFAULT '';
	UPDATE cases SET fleet = COALESCE((SELECT CASE WHEN instr(e.actor, '/') > 0 THEN substr(e.actor, 1, instr(e.actor, '/') - 1) ELSE '' END
		FROM case_events e WHERE e.case_id = cases.id AND e.kind = 'case.detected' ORDER BY e.seq LIMIT 1), '');
	UPDATE cases SET fleet = project WHERE fleet = '';
	CREATE INDEX events_kind_at ON events(kind, at);`, // the hooks' activity (hooks.go HookActivity) reads events by kind and time
	// RFC-0014 §4.6: the directory's copy, so a directory outage never stops a case. A person is keyed by email; person_ids maps
	// each tool's id (a Slack user, a code-host login) to that email, one person per id. Who someone is, never what they may do.
	`CREATE TABLE people (
		email TEXT PRIMARY KEY,
		name TEXT NOT NULL,
		ids TEXT NOT NULL,
		updated_at TEXT NOT NULL
	);
	CREATE TABLE person_ids (
		tool TEXT NOT NULL,
		id TEXT NOT NULL,
		email TEXT NOT NULL REFERENCES people(email) ON DELETE CASCADE,
		PRIMARY KEY (tool, id)
	);`,
	// RFC-0014 §4.8: what a live run read through MCP servers, one JSON list of record lines per run, written after the run (the input
	// is frozen at dispatch, before the reads happen) and merged into the input's recorded_tool_results when the input is read.
	`CREATE TABLE input_reads (
		run_id TEXT PRIMARY KEY,
		lines TEXT NOT NULL DEFAULT '[]',
		complete INTEGER NOT NULL DEFAULT 1
	);`,
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
	db  *sql.DB // the one writer, and every transaction
	rdb *sql.DB // reads outside a transaction: query_only, readConns at once
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
	Version   int      `json:"version"` // the routine's version that made the run (1 when not set)
	// A shadow run (v2 step 2): Routine is "<fleet>/<routine>@v<n>", Shadow is true, and InputKey is the live run whose
	// input it was given. A live run has InputKey "" (it is its own input's key).
	Shadow   bool   `json:"shadow,omitempty"`
	InputKey string `json:"input_key,omitempty"`
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
	// and no writable temp directory, so a statement that spills (schema step 3's UPDATE over the host's
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
	// Reads outside a transaction get their own connections (2026-11-23): with every read queued on the one writer's
	// connection, a page's reads waited behind the scheduler's and each other (the pairs answer, 83 ms natively on the
	// installation's store, took 9 s live). WAL lets readers run beside the writer, each seeing the last commit;
	// query_only makes a write sent here an error rather than a second writer.
	rdb, err := sql.Open("sqlite", "file:"+path+"?_pragma=query_only(1)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=temp_store(2)")
	if err != nil {
		db.Close()
		return nil, err
	}
	rdb.SetMaxOpenConns(readConns)
	s.rdb = rdb
	return s, nil
}

// readConns is how many reads run at once beside the writer.
const readConns = 4

// PoolStats is how long reads and writes have waited for a connection since start: the wait the read pool removes.
type PoolStats struct {
	ReadWaits, WriteWaits         int64
	ReadWaitSec, WriteWaitSec     float64
	ReadInUse, ReadOpen, WriteUse int
}

// Pools reads both pools' counters (database/sql's own).
func (s *Store) Pools() PoolStats {
	r, w := s.rdb.Stats(), s.db.Stats()
	return PoolStats{ReadWaits: r.WaitCount, WriteWaits: w.WaitCount, ReadWaitSec: r.WaitDuration.Seconds(), WriteWaitSec: w.WaitDuration.Seconds(),
		ReadInUse: r.InUse, ReadOpen: r.OpenConnections, WriteUse: w.InUse}
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

func (s *Store) Close() error {
	if s.rdb != nil {
		s.rdb.Close()
	}
	return s.db.Close()
}

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
	err := s.rdb.QueryRowContext(ctx, `SELECT seq, at, actor, via, kind, subject, detail FROM events WHERE kind = ? AND subject = ?
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
	rows, err := s.rdb.QueryContext(ctx, `SELECT seq, at, actor, via, kind, subject, detail FROM events ORDER BY seq DESC LIMIT ?`, limit)
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
	err := s.rdb.QueryRowContext(ctx, `SELECT value, at FROM controls WHERE subject = ? AND key = ?`, subject, key).Scan(&v, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", nil
	}
	return v, at, err
}

// Control returns one control's value, or "" when it was never set.
func (s *Store) Control(ctx context.Context, subject, key string) (string, error) {
	var v string
	err := s.rdb.QueryRowContext(ctx, `SELECT value FROM controls WHERE subject = ? AND key = ?`, subject, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

func (s *Store) Controls(ctx context.Context) ([]Control, error) {
	rows, err := s.rdb.QueryContext(ctx, `SELECT subject, key, value, actor, at FROM controls ORDER BY subject, key`)
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
	_, err := s.db.ExecContext(ctx, `INSERT INTO runs (id, routine, spec_hash, trigger, actor, autonomy, started_at, status, trace_id, detail, version, shadow, input_key)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.Routine, r.SpecHash, r.Trigger, r.Actor, r.Autonomy, r.StartedAt, r.Status, r.TraceID, r.Detail, versionOrOne(r.Version), r.Shadow, r.InputKey)
	return err
}

// FinishRun closes a running run. A run already closed is left alone and reported.
func (s *Store) FinishRun(ctx context.Context, id, status string, exitCode *int, spend, durationS float64, detail string) error {
	// shadow-ok: by run id; a shadow run ends the same way
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

// EndRun is FinishRun and AddSpend in one transaction: a reader never sees a run ended without its spend in the day's
// total (another run's budget check, the UI, a test waiting for the end). A spend of 0 adds nothing. It returns the day's
// total for subject.
func (s *Store) EndRun(ctx context.Context, id, status string, exitCode *int, spend, durationS float64, detail, day, subject string) (float64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	// shadow-ok: by run id; a shadow run ends the same way, and its spend subject carries @v
	res, err := tx.ExecContext(ctx, `UPDATE runs SET ended_at = ?, status = ?, exit_code = ?, spend_usd = ?, duration_s = ?, detail = ?
		WHERE id = ? AND status = ?`,
		s.now().Format(ts), status, exitCode, spend, durationS, detail, id, StatusRunning)
	if err != nil {
		return 0, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return 0, fmt.Errorf("run %s is not running", id)
	}
	if spend > 0 {
		if _, err := tx.ExecContext(ctx, `INSERT INTO spend (day, subject, usd) VALUES (?, ?, ?)
			ON CONFLICT(day, subject) DO UPDATE SET usd = usd + excluded.usd`, day, subject, spend); err != nil {
			return 0, err
		}
	}
	var total float64
	// shadow-ok: keyed by subject; a live routine's key never matches a shadow's
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT usd FROM spend WHERE day = ? AND subject = ?), 0)`, day, subject).Scan(&total); err != nil {
		return 0, err
	}
	return total, tx.Commit()
}

// SkippedRun records a trigger that did not start a run, with the reason, so a skip is visible.
func (s *Store) SkippedRun(ctx context.Context, r Run) error {
	now := s.now().Format(ts)
	r.StartedAt, r.Status = now, StatusSkipped
	_, err := s.db.ExecContext(ctx, `INSERT INTO runs (id, routine, spec_hash, trigger, actor, autonomy, started_at, ended_at, status, trace_id, detail, version, shadow, input_key)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.Routine, r.SpecHash, r.Trigger, r.Actor, r.Autonomy, now, now, r.Status, r.TraceID, r.Detail, versionOrOne(r.Version), r.Shadow, r.InputKey)
	return err
}

// RunRef names a run and its routine.
type RunRef struct{ ID, Routine, StartedAt string }

// RunningRuns lists the runs recorded as running: at flotillad's start, the ones the previous flotillad
// left in flight, read before MarkInterrupted closes them. Shadow runs are in it: a restart orphans them too.
func (s *Store) RunningRuns(ctx context.Context) ([]RunRef, error) {
	// shadow-ok: every orphaned run must be found and cancelled, a shadow's included
	rows, err := s.rdb.QueryContext(ctx, `SELECT id, routine, started_at FROM runs WHERE status = ? ORDER BY started_at`, StatusRunning)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RunRef
	for rows.Next() {
		var r RunRef
		if err := rows.Scan(&r.ID, &r.Routine, &r.StartedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// MarkInterrupted closes runs left running by a previous flotillad and returns how many.
func (s *Store) MarkInterrupted(ctx context.Context) (int64, error) {
	// shadow-ok: a restart interrupts every run in flight, a shadow's included
	res, err := s.db.ExecContext(ctx, `UPDATE runs SET status = ?, ended_at = ?, detail = 'flotillad restarted while it ran'
		WHERE status = ?`, StatusInterrupted, s.now().Format(ts), StatusRunning)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// Runs lists the newest runs, of one routine ("" for all). Live runs only: a shadow's runs are read by naming its
// key ("<fleet>/<routine>@v<n>"), never by a live routine's.
func (s *Store) Runs(ctx context.Context, routine string, limit int) ([]Run, error) {
	// shadow-ok: liveRuns is added below unless the routine named is a shadow's key
	q := `SELECT ` + runCols + ` FROM runs WHERE 1 = 1`
	var args []any
	if routine != "" {
		q += ` AND routine = ?`
		args = append(args, routine)
	}
	if !IsShadowKey(routine) {
		q += ` AND ` + liveRuns
	}
	q += ` ORDER BY started_at DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.rdb.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	return scanRuns(rows)
}

// LastSuccess is the end time of the routine's newest ok run, zero when it has none.
func (s *Store) LastSuccess(ctx context.Context, routine string) (time.Time, error) {
	var at sql.NullString
	if err := s.rdb.QueryRowContext(ctx, `SELECT MAX(ended_at) FROM runs WHERE routine = ? AND status = ? AND `+liveRuns, routine, StatusOK).Scan(&at); err != nil {
		return time.Time{}, err
	}
	if !at.Valid {
		return time.Time{}, nil
	}
	return time.Parse(ts, at.String)
}

// AddSpend adds to a subject's (a routine id's) spend for a local day ("2006-01-02") and returns the new total. A
// shadow's subject is its own key ("<fleet>/<routine>@v<n>"), so its spend never lands on the live routine's day.
func (s *Store) AddSpend(ctx context.Context, day, subject string, usd float64) (float64, error) {
	// shadow-ok: keyed by subject; a shadow's subject carries @v and is never a live routine's
	if _, err := s.db.ExecContext(ctx, `INSERT INTO spend (day, subject, usd) VALUES (?, ?, ?)
		ON CONFLICT(day, subject) DO UPDATE SET usd = usd + excluded.usd`, day, subject, usd); err != nil {
		return 0, err
	}
	return s.Spend(ctx, day, subject)
}

func (s *Store) Spend(ctx context.Context, day, subject string) (float64, error) {
	var v float64
	// shadow-ok: keyed by subject; a live routine's key never matches a shadow's
	err := s.rdb.QueryRowContext(ctx, `SELECT usd FROM spend WHERE day = ? AND subject = ?`, day, subject).Scan(&v)
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
