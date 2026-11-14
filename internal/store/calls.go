package store

import (
	"context"
	"database/sql"
)

// ModelCall is one model call of a run, as the model client recorded it (runtime/model_client.py).
// Tokens and cost are nil when the endpoint did not report them: never counted as zero.
type ModelCall struct {
	RunID      string   `json:"run_id"`
	Routine    string   `json:"routine"`
	At         string   `json:"at"`
	Alias      string   `json:"alias"`
	Endpoint   string   `json:"endpoint"`
	Model      string   `json:"model"`
	Deployment string   `json:"deployment"`
	TokensIn   *int64   `json:"tokens_in"`
	TokensOut  *int64   `json:"tokens_out"`
	CostUSD    *float64 `json:"cost_usd"`
	CostSource string   `json:"cost_source"`
	LatencyS   *float64 `json:"latency_s"`
	Error      string   `json:"error"`
	Check      string   `json:"check,omitempty"`    // a contract check's name (G15); "" for an ordinary call
	CheckOK    *bool    `json:"check_ok,omitempty"` // its result
	Detail     string   `json:"detail,omitempty"`   // why it failed, or what it saw
}

// RecordCalls keeps a run's model calls, in one transaction.
func (s *Store) RecordCalls(ctx context.Context, calls []ModelCall) error {
	if len(calls) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, c := range calls {
		at := c.At
		if at == "" {
			at = s.now().Format(ts)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO run_calls (run_id, routine, at, alias, endpoint, model, deployment,
			tokens_in, tokens_out, cost_usd, cost_source, latency_s, error, check_name, check_ok, detail)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			c.RunID, c.Routine, at, c.Alias, c.Endpoint, c.Model, c.Deployment, c.TokensIn, c.TokensOut, c.CostUSD,
			c.CostSource, c.LatencyS, c.Error, c.Check, c.CheckOK, c.Detail); err != nil {
			return err
		}
	}
	return tx.Commit()
}

const callCols = `run_id, routine, at, alias, endpoint, model, deployment, tokens_in, tokens_out, cost_usd, cost_source, latency_s, error,
	check_name, check_ok, detail`

func scanCalls(rows *sql.Rows) ([]ModelCall, error) {
	defer rows.Close()
	var out []ModelCall
	for rows.Next() {
		var c ModelCall
		var tin, tout sql.NullInt64
		var cost, lat sql.NullFloat64
		var ok sql.NullBool
		if err := rows.Scan(&c.RunID, &c.Routine, &c.At, &c.Alias, &c.Endpoint, &c.Model, &c.Deployment, &tin, &tout,
			&cost, &c.CostSource, &lat, &c.Error, &c.Check, &ok, &c.Detail); err != nil {
			return nil, err
		}
		if ok.Valid {
			c.CheckOK = &ok.Bool
		}
		if tin.Valid {
			c.TokensIn = &tin.Int64
		}
		if tout.Valid {
			c.TokensOut = &tout.Int64
		}
		if cost.Valid {
			c.CostUSD = &cost.Float64
		}
		if lat.Valid {
			c.LatencyS = &lat.Float64
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// RunCalls returns each run's model calls, in the order they were made.
func (s *Store) RunCalls(ctx context.Context, runIDs []string) (map[string][]ModelCall, error) {
	out := map[string][]ModelCall{}
	if len(runIDs) == 0 {
		return out, nil
	}
	args := make([]any, len(runIDs))
	ph := ""
	for i, id := range runIDs {
		args[i] = id
		if i > 0 {
			ph += ", "
		}
		ph += "?"
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+callCols+` FROM run_calls WHERE run_id IN (`+ph+`) ORDER BY id`, args...)
	if err != nil {
		return nil, err
	}
	cs, err := scanCalls(rows)
	for _, c := range cs {
		out[c.RunID] = append(out[c.RunID], c)
	}
	return out, err
}

// CallsSince returns every model call since a time, oldest first.
func (s *Store) CallsSince(ctx context.Context, since string) ([]ModelCall, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+callCols+` FROM run_calls WHERE at >= ? ORDER BY id`, since)
	if err != nil {
		return nil, err
	}
	return scanCalls(rows)
}
