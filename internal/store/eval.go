package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

// EvalResult is one measured number for a routine's prompt on a model (a replay's or an eval's result:
// triage cost per decision, say). N is the cases it ran over, Repeats the runs it was averaged from.
// Results are appended, never edited: the newest for a key is the current one.
type EvalResult struct {
	ID         int64   `json:"id"`
	Routine    string  `json:"routine"`
	PromptSHA  string  `json:"prompt_sha"`
	Model      string  `json:"model"`
	Metric     string  `json:"metric"`
	Value      float64 `json:"value"`
	N          int64   `json:"n"`
	Repeats    int64   `json:"repeats"`
	Source     string  `json:"source"`
	RecordedAt string  `json:"recorded_at"`
	RecordedBy string  `json:"recorded_by"`
}

const evalCols = `id, routine, prompt_sha, model, metric, value, n, repeats, source, recorded_at, recorded_by`

// RecordEval appends a result; its time is now.
func (s *Store) RecordEval(ctx context.Context, e EvalResult) (int64, error) {
	if strings.TrimSpace(e.Routine) == "" || strings.TrimSpace(e.PromptSHA) == "" ||
		strings.TrimSpace(e.Model) == "" || strings.TrimSpace(e.Metric) == "" {
		return 0, errors.New("an eval result needs a routine, a prompt sha, a model and a metric")
	}
	if e.RecordedBy == "" {
		return 0, errors.New("an eval result records who recorded it")
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO eval_results (routine, prompt_sha, model, metric, value, n, repeats, source,
		recorded_at, recorded_by) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.Routine, e.PromptSHA, e.Model, e.Metric, e.Value, e.N, e.Repeats, e.Source, s.now().Format(ts), e.RecordedBy)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func scanEvals(rows *sql.Rows) ([]EvalResult, error) {
	defer rows.Close()
	var out []EvalResult
	for rows.Next() {
		var e EvalResult
		if err := rows.Scan(&e.ID, &e.Routine, &e.PromptSHA, &e.Model, &e.Metric, &e.Value, &e.N, &e.Repeats, &e.Source,
			&e.RecordedAt, &e.RecordedBy); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// LatestEval is, for each metric measured for a routine, prompt and model, its newest result (by metric name).
func (s *Store) LatestEval(ctx context.Context, routine, promptSHA, model string) ([]EvalResult, error) {
	rows, err := s.rdb.QueryContext(ctx, `SELECT `+evalCols+` FROM eval_results WHERE id IN (
		SELECT MAX(id) FROM eval_results WHERE routine = ? AND prompt_sha = ? AND model = ? GROUP BY metric)
		ORDER BY metric`, routine, promptSHA, model)
	if err != nil {
		return nil, err
	}
	return scanEvals(rows)
}

// Evals lists results, newest first; routine "" is every routine.
func (s *Store) Evals(ctx context.Context, routine string, limit int) ([]EvalResult, error) {
	q, args := `SELECT `+evalCols+` FROM eval_results`, []any{}
	if routine != "" {
		q += ` WHERE routine = ?`
		args = append(args, routine)
	}
	rows, err := s.rdb.QueryContext(ctx, q+` ORDER BY id DESC LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	return scanEvals(rows)
}
