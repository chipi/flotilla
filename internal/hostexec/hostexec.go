// Package hostexec runs a signed execution on its host (ADR-0006 §4 and §7, security T13 and T14): the
// host agent's actions role. Whatever flotillad signed, the host runs only what its own config allows,
// within its ceiling and rate limits, once per execution id.
//
// In order, any "no" refuses: the envelope verifies (internal/envelope); an execution id seen before
// returns its recorded result when the bytes are identical (a resend after a lost result) and is refused
// as a forgery when they differ; the action is in this host's config and its parameters pass
// (hostcfg.Argv, the only way a command is built); the approval is within this host's ceiling; the
// action is under its hourly limit here. Then `started` is committed before anything runs; the local
// preconditions are checked (no longer holding: precondition_false, nothing runs); the argv runs without
// a shell, with the action's timeout (past it: unknown); the result is recorded. After a restart, a
// `started` with no result is unknown and never runs again. Every outcome is a line in the audit log
// (internal/hostaudit), and `started` is logged before the store records it: nothing runs unlogged.
package hostexec

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/chipi/flotilla/internal/envelope"
	"github.com/chipi/flotilla/internal/hostaudit"
	"github.com/chipi/flotilla/internal/hostcfg"
	_ "modernc.org/sqlite"
)

// Statuses of an execution on the host.
const (
	Done              = "done"
	Failed            = "failed"
	Unknown           = "unknown"
	PreconditionFalse = "precondition_false"
	Refused           = "refused"
	started           = "started"
)

// timeFormat is how times are stored: fixed-width, because the rate limit compares them as strings
// (the same layout as flotillad's store.TimeFormat; the agent does not link flotillad's store).
const timeFormat = "2006-01-02T15:04:05.000000000Z07:00"

// OutputMax caps what is kept of a command's output; its hash covers all of it.
const OutputMax = 16 * 1024

// Result is what the host reports for an execution.
type Result struct {
	ExecutionID string `json:"execution_id"`
	Status      string `json:"status"`
	ExitCode    int    `json:"exit_code"`
	Output      string `json:"output,omitempty"`
	OutputSHA   string `json:"output_sha256,omitempty"`
	Detail      string `json:"detail,omitempty"`
	Resent      bool   `json:"resent,omitempty"` // the recorded result of an identical resend; nothing ran
	ChainHead   string `json:"chain_head"`       // the audit log's head after this result (ADR-0006 §8)
}

// Runner runs an argv, without a shell, within timeout. timedOut reports the timeout; err is a failure
// to start it at all.
type Runner interface {
	Run(ctx context.Context, argv []string, timeout time.Duration) (exit int, output []byte, timedOut bool, err error)
}

// Checker evaluates a local precondition (ADR-0006 §7: files, launchctl print, systemctl, docker
// inspect). It reports whether it holds, and why.
type Checker interface {
	Check(ctx context.Context, p hostcfg.Pre) (holds bool, detail string, err error)
}

// Executor is the actions role on one host.
type Executor struct {
	Cfg   *hostcfg.Config
	Run   Runner
	Check Checker
	Audit *hostaudit.Log
	Now   func() time.Time

	db *sql.DB
	mu sync.Mutex // one execution decided at a time
}

// Open opens (or creates) the host's execution store and marks every execution left started by a
// previous run unknown: it never runs again (ADR-0006 §7).
func Open(path string) (*sql.DB, int64, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=temp_store(2)")
	if err != nil {
		return nil, 0, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS executions (
		id TEXT PRIMARY KEY,
		payload BLOB NOT NULL,
		action TEXT NOT NULL,
		argv TEXT NOT NULL,
		status TEXT NOT NULL,
		exit_code INTEGER NOT NULL DEFAULT 0,
		output TEXT NOT NULL DEFAULT '',
		output_sha256 TEXT NOT NULL DEFAULT '',
		detail TEXT NOT NULL DEFAULT '',
		started_at TEXT NOT NULL,
		ended_at TEXT
	)`); err != nil {
		db.Close()
		return nil, 0, err
	}
	res, err := db.Exec(`UPDATE executions SET status = ?, detail = 'the host agent stopped before its result was recorded', ended_at = started_at
		WHERE status = ?`, Unknown, started)
	if err != nil {
		db.Close()
		return nil, 0, err
	}
	n, _ := res.RowsAffected()
	return db, n, nil
}

// New is an Executor over an opened store and audit log.
func New(db *sql.DB, audit *hostaudit.Log, cfg *hostcfg.Config, run Runner, check Checker) *Executor {
	return &Executor{Cfg: cfg, Run: run, Check: check, Audit: audit, Now: time.Now, db: db}
}

func refused(id, why string) Result { return Result{ExecutionID: id, Status: Refused, Detail: why} }

// Handle decides and, when everything allows it, runs one execution received as raw bytes. Every
// outcome is a line in the audit log, and the result carries the log's chain head after it.
func (x *Executor) Handle(ctx context.Context, raw []byte) Result {
	x.mu.Lock()
	defer x.mu.Unlock()
	r, e := x.decide(ctx, raw)
	ev := hostaudit.Ended
	switch {
	case r.Resent:
		ev = hostaudit.Resent
	case r.Status == Refused:
		ev = hostaudit.Refused
	}
	if _, err := x.Audit.Append(hostaudit.Entry{Time: x.Now().UTC(), Event: ev, ExecutionID: r.ExecutionID, CaseID: e.CaseID,
		Action: e.Action, Approved: e.Approved, Status: r.Status, ExitCode: r.ExitCode, OutputSHA: r.OutputSHA, Detail: r.Detail}); err != nil {
		r.Detail += " (not in the audit log: " + err.Error() + ")"
	}
	r.ChainHead = x.Audit.Head()
	return r
}

func (x *Executor) decide(ctx context.Context, raw []byte) (Result, envelope.Execution) {
	now := x.Now().UTC()
	e, payload, err := envelope.Verify(raw, x.Cfg.Keys(), x.Cfg.Host, now)
	if err != nil {
		return refused(e.ID, err.Error()), e
	}
	if r, seen, err := x.recorded(ctx, e.ID, payload); err != nil {
		return refused(e.ID, "the store: "+err.Error()), e
	} else if seen {
		return r, e
	}
	argv, err := x.Cfg.Argv(e.Action, e.Params)
	if err != nil {
		return refused(e.ID, err.Error()), e
	}
	switch {
	case x.Cfg.Ceiling == "shadow":
		return refused(e.ID, "this host accepts nothing (its ceiling is shadow)"), e
	case e.Approved == "auto" && x.Cfg.Ceiling != "auto":
		return refused(e.ID, "approved automatically, above this host's ceiling ("+x.Cfg.Ceiling+")"), e
	}
	a := x.Cfg.Actions[e.Action]
	var n int
	// what ran or may have run counts; a precondition that no longer held ran nothing
	if err := x.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM executions WHERE action = ? AND started_at >= ? AND status <> ?`,
		e.Action, now.Add(-time.Hour).Format(timeFormat), PreconditionFalse).Scan(&n); err != nil {
		return refused(e.ID, "the store: "+err.Error()), e
	}
	if n >= a.PerHour {
		return refused(e.ID, fmt.Sprintf("%s ran %d times in the last hour here, its limit", e.Action, n)), e
	}
	// nothing runs that the audit log does not show starting
	if _, err := x.Audit.Append(hostaudit.Entry{Time: now, Event: hostaudit.Started, ExecutionID: e.ID, CaseID: e.CaseID,
		Action: e.Action, Approved: e.Approved, Argv: argv}); err != nil {
		return refused(e.ID, "the audit log: "+err.Error()), e
	}
	argvJSON, _ := json.Marshal(argv)
	if _, err := x.db.ExecContext(ctx, `INSERT INTO executions (id, payload, action, argv, status, started_at) VALUES (?, ?, ?, ?, ?, ?)`,
		e.ID, payload, e.Action, string(argvJSON), started, now.Format(timeFormat)); err != nil {
		return refused(e.ID, "the store: "+err.Error()), e
	}
	r := x.execute(ctx, e, a, argv)
	if _, err := x.db.ExecContext(ctx, `UPDATE executions SET status = ?, exit_code = ?, output = ?, output_sha256 = ?, detail = ?, ended_at = ?
		WHERE id = ?`, r.Status, r.ExitCode, r.Output, r.OutputSHA, r.Detail, x.Now().UTC().Format(timeFormat), e.ID); err != nil {
		r.Detail = "ran, but the result could not be recorded: " + err.Error()
	}
	return r, e
}

func (x *Executor) execute(ctx context.Context, e envelope.Execution, a hostcfg.Action, argv []string) Result {
	r := Result{ExecutionID: e.ID}
	for _, p := range a.Pre {
		holds, detail, err := x.Check.Check(ctx, p)
		if err != nil {
			r.Status, r.Detail = Unknown, fmt.Sprintf("precondition %s could not be checked: %v", p.Kind, err)
			return r
		}
		if !holds {
			r.Status, r.Detail = PreconditionFalse, fmt.Sprintf("%s no longer holds: %s", p.Kind, detail)
			return r
		}
	}
	exit, out, timedOut, err := x.Run.Run(ctx, argv, time.Duration(a.Timeout))
	sum := sha256.Sum256(out)
	r.ExitCode, r.OutputSHA = exit, hex.EncodeToString(sum[:])
	if len(out) > OutputMax {
		out = out[len(out)-OutputMax:]
	}
	r.Output = string(out)
	switch {
	case err != nil:
		r.Status, r.Detail = Failed, "did not start: "+err.Error()
	case timedOut:
		r.Status, r.Detail = Unknown, fmt.Sprintf("past its timeout (%s): the target stays held", time.Duration(a.Timeout))
	case exit == 0:
		r.Status = Done
	default:
		r.Status, r.Detail = Failed, fmt.Sprintf("exit %d", exit)
	}
	return r
}

// recorded is the check of a seen execution id: the same signed bytes get the recorded result back (and
// nothing runs); different bytes under a seen id are a forgery.
func (x *Executor) recorded(ctx context.Context, id string, payload []byte) (Result, bool, error) {
	var stored []byte
	var r Result
	err := x.db.QueryRowContext(ctx, `SELECT payload, status, exit_code, output, output_sha256, detail FROM executions WHERE id = ?`, id).
		Scan(&stored, &r.Status, &r.ExitCode, &r.Output, &r.OutputSHA, &r.Detail)
	if errors.Is(err, sql.ErrNoRows) {
		return Result{}, false, nil
	}
	if err != nil {
		return Result{}, false, err
	}
	if string(stored) != string(payload) {
		return refused(id, "a different execution under an id already seen: refused as a forgery"), true, nil
	}
	r.ExecutionID, r.Resent = id, true
	return r, true, nil
}
