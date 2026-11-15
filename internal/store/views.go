package store

import (
	"context"
	"database/sql"
	"strings"
	"time"
)

// The UI's read queries (ADR-0014): one page of a table at a time, and the counts its summaries need.

const runCols = `id, routine, spec_hash, trigger, actor, autonomy, started_at, ended_at, status, exit_code, spend_usd, duration_s, COALESCE(trace_id, ''), detail`

func scanRuns(rows *sql.Rows) ([]Run, error) {
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

// RunQuery is one page of the runs table. Sort is a column name from runSorts; Problems keeps runs
// that did not end ok (skipped runs too: a skipped run did not do its work).
type RunQuery struct {
	Routine  string
	Fleet    string
	Status   string
	Problems bool
	Spend    bool // with model spend
	Since    string
	Q        string
	Sort     string
	Desc     bool
	Offset   int
	Limit    int
}

var runSorts = map[string]string{"run": "id", "routine": "routine", "trigger": "trigger", "started": "started_at",
	"took": "duration_s", "status": "status", "cost": "spend_usd"}

// RunSorts says whether a column can be sorted.
func RunSorts(col string) bool { _, ok := runSorts[col]; return ok }

// RunsPage returns a page of runs and how many match in all.
func (s *Store) RunsPage(ctx context.Context, q RunQuery) ([]Run, int, error) {
	where := ` WHERE 1 = 1`
	var args []any
	if q.Routine != "" {
		where += ` AND routine = ?`
		args = append(args, q.Routine)
	}
	if q.Fleet != "" {
		where += ` AND routine LIKE ? ESCAPE '\'`
		args = append(args, likeEscape(q.Fleet)+"/%")
	}
	if q.Status != "" {
		where += ` AND status = ?`
		args = append(args, q.Status)
	}
	if q.Problems {
		where += ` AND status NOT IN (?, ?)`
		args = append(args, StatusOK, StatusRunning)
	}
	if q.Spend {
		where += ` AND spend_usd > 0`
	}
	if q.Since != "" {
		where += ` AND started_at >= ?`
		args = append(args, q.Since)
	}
	if q.Q != "" {
		where += ` AND (id LIKE ? ESCAPE '\' OR routine LIKE ? ESCAPE '\' OR trigger LIKE ? ESCAPE '\' OR status LIKE ? ESCAPE '\')`
		like := "%" + likeEscape(q.Q) + "%"
		args = append(args, like, like, like, like)
	}
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM runs`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	col, ok := runSorts[q.Sort]
	if !ok {
		col = "started_at"
	}
	dir := " ASC"
	if q.Desc {
		dir = " DESC"
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+runCols+` FROM runs`+where+` ORDER BY `+col+dir+`, started_at DESC LIMIT ? OFFSET ?`,
		append(args, limit, q.Offset)...)
	if err != nil {
		return nil, 0, err
	}
	out, err := scanRuns(rows)
	return out, total, err
}

func likeEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// RoutineDay is one routine's runs on one day.
type RoutineDay struct {
	Routine string
	Day     string // YYYY-MM-DD, UTC
	Runs    int
	Failed  int // ended neither ok, running nor skipped
	Spend   float64
	Last    string // the newest start that day
}

// RunDays counts runs per routine and day since a time.
func (s *Store) RunDays(ctx context.Context, since string) ([]RoutineDay, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT routine, substr(started_at, 1, 10), COUNT(*),
		SUM(CASE WHEN status IN (?, ?, ?) THEN 1 ELSE 0 END), COALESCE(SUM(spend_usd), 0), MAX(started_at)
		FROM runs WHERE started_at >= ? GROUP BY routine, substr(started_at, 1, 10) ORDER BY 2`,
		StatusError, StatusTimeout, StatusInterrupted, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RoutineDay
	for rows.Next() {
		var d RoutineDay
		if err := rows.Scan(&d.Routine, &d.Day, &d.Runs, &d.Failed, &d.Spend, &d.Last); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// CaseEnds is a case's first and newest events.
type CaseEnds struct {
	First, Last CaseEvent
}

// CaseEventEnds returns every case's first and newest events.
func (s *Store) CaseEventEnds(ctx context.Context) (map[string]CaseEnds, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT seq, case_id, at, kind, actor, by_role, from_status, to_status, detail, data FROM case_events
		WHERE seq IN (SELECT MIN(seq) FROM case_events GROUP BY case_id) OR seq IN (SELECT MAX(seq) FROM case_events GROUP BY case_id)
		ORDER BY seq`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]CaseEnds{}
	for rows.Next() {
		var e CaseEvent
		if err := rows.Scan(&e.Seq, &e.CaseID, &e.At, &e.Kind, &e.Actor, &e.By, &e.From, &e.To, &e.Detail, &e.Data); err != nil {
			return nil, err
		}
		c, seen := out[e.CaseID]
		if !seen {
			c.First = e
		}
		c.Last = e
		out[e.CaseID] = c
	}
	return out, rows.Err()
}

// CaseEventsSince returns every case's events since a time, newest first, at most limit.
func (s *Store) CaseEventsSince(ctx context.Context, since string, limit int) ([]CaseEvent, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT seq, case_id, at, kind, actor, by_role, from_status, to_status, detail, data
		FROM case_events WHERE at >= ? ORDER BY seq DESC LIMIT ?`, since, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CaseEvent
	for rows.Next() {
		var e CaseEvent
		if err := rows.Scan(&e.Seq, &e.CaseID, &e.At, &e.Kind, &e.Actor, &e.By, &e.From, &e.To, &e.Detail, &e.Data); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// RunCases returns the cases each run planned for.
func (s *Store) RunCases(ctx context.Context, runIDs []string) (map[string][]string, error) {
	out := map[string][]string{}
	if len(runIDs) == 0 {
		return out, nil
	}
	ph := strings.TrimSuffix(strings.Repeat("?, ", len(runIDs)), ", ")
	args := make([]any, len(runIDs))
	for i, id := range runIDs {
		args[i] = id
	}
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT run_id, case_id FROM plans WHERE run_id IN (`+ph+`) ORDER BY id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var run, c string
		if err := rows.Scan(&run, &c); err != nil {
			return nil, err
		}
		out[run] = append(out[run], c)
	}
	return out, rows.Err()
}

// HostExecutionsSince returns every host's executions sent since a time, newest first.
func (s *Store) HostExecutionsSince(ctx context.Context, since string) ([]HostExecution, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, case_id, plan_id, host, action, approved, status, exit_code, output_sha256,
		detail, chain_head, sent_at, ended_at FROM host_executions WHERE sent_at >= ? ORDER BY sent_at DESC`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []HostExecution
	for rows.Next() {
		var x HostExecution
		var ended sql.NullString
		if err := rows.Scan(&x.ID, &x.CaseID, &x.PlanID, &x.Host, &x.Action, &x.Approved, &x.Status, &x.ExitCode, &x.OutputSHA256,
			&x.Detail, &x.ChainHead, &x.SentAt, &ended); err != nil {
			return nil, err
		}
		if ended.Valid {
			x.EndedAt = &ended.String
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// visitGap is how long Now must go unseen for the next look to count as a new visit.
const visitGap = time.Hour

// SeenNow records that a user looked at Now and returns the end of their previous visit: the last
// time they looked before a gap of an hour or more ("" before their second visit). Reloading Now
// within a visit keeps the same answer.
func (s *Store) SeenNow(ctx context.Context, user string) (string, error) {
	var seen, since string
	if err := s.db.QueryRowContext(ctx, `SELECT now_seen_at, now_since FROM users WHERE name = ?`, user).Scan(&seen, &since); err != nil {
		if err == sql.ErrNoRows {
			return "", nil
		}
		return "", err
	}
	now := s.now()
	if t, err := time.Parse(time.RFC3339Nano, seen); err == nil && now.Sub(t) >= visitGap {
		since = seen
	}
	_, err := s.db.ExecContext(ctx, `UPDATE users SET now_seen_at = ?, now_since = ? WHERE name = ?`, now.Format(ts), since, user)
	return since, err
}

// LastLook is the end of a user's previous visit to Now, as SeenNow last answered it, without recording a look.
func (s *Store) LastLook(ctx context.Context, user string) (string, error) {
	var since string
	err := s.db.QueryRowContext(ctx, `SELECT now_since FROM users WHERE name = ?`, user).Scan(&since)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return since, err
}
