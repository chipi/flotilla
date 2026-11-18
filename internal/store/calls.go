package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
)

// Kinds of a run_calls row. A step is something the harness did between model calls (a verdict, a gate's outcome, an
// advisor's pin): no alias, model, tokens or cost, its text in Detail and its Role the one it concerns ("harness" for a
// hand-off, "gates" for a gate's outcome). Sums of tokens or cost, and counts of calls, skip steps (CallsSince does).
const (
	KindCall = "call"
	KindStep = "step"
)

// MaxToolArg is the longest Arg of a ToolCall kept, in characters.
const MaxToolArg = 160

// ToolCall is one tool the model called in a turn: its name, its main argument (a command, a path, a pattern; scrubbed
// and cut at MaxToolArg by whoever recorded it) and whether it worked. OK is nil when the result is not known.
type ToolCall struct {
	Name string `json:"name"`
	Arg  string `json:"arg,omitempty"`
	OK   *bool  `json:"ok,omitempty"`
}

// ParseTools reads a ModelCall's Tools column in either form it was ever written in: the old array of names
// (["bash","read"]) or the array of objects ([{"name":"bash","arg":"ls","ok":true}]), mixed too. "" and anything that
// is not a JSON array read as none.
func ParseTools(s string) []ToolCall {
	var raw []json.RawMessage
	if strings.TrimSpace(s) == "" || json.Unmarshal([]byte(s), &raw) != nil {
		return nil
	}
	var out []ToolCall
	for _, r := range raw {
		var name string
		if json.Unmarshal(r, &name) == nil {
			if name != "" {
				out = append(out, ToolCall{Name: name})
			}
			continue
		}
		var t ToolCall
		if json.Unmarshal(r, &t) == nil && t.Name != "" {
			if rs := []rune(t.Arg); len(rs) > MaxToolArg {
				t.Arg = string(rs[:MaxToolArg])
			}
			out = append(out, t)
		}
	}
	return out
}

// NormalizeTools is what a worker reported as a call's tools, in the form the column keeps: the array of objects,
// "" when the turn called none. An old worker's array of names becomes objects with just the name.
func NormalizeTools(raw []byte) string {
	tools := ParseTools(string(raw))
	if len(tools) == 0 {
		return ""
	}
	b, err := json.Marshal(tools)
	if err != nil {
		return ""
	}
	return string(b)
}

// ToolCalls is the call's tools, parsed from either stored form.
func (c ModelCall) ToolCalls() []ToolCall { return ParseTools(c.Tools) }

// IsStep reports whether the row is a harness step, not a model call.
func (c ModelCall) IsStep() bool { return c.Kind == KindStep }

// ModelCall is one model call of a run, as the model client recorded it (runtime/model_client.py).
// Tokens and cost are nil when the endpoint did not report them: never counted as zero.
type ModelCall struct {
	RunID        string   `json:"run_id"`
	Routine      string   `json:"routine"`
	At           string   `json:"at"`
	Alias        string   `json:"alias"`
	Endpoint     string   `json:"endpoint"`
	Model        string   `json:"model"`
	Deployment   string   `json:"deployment"`
	TokensIn     *int64   `json:"tokens_in"`
	TokensOut    *int64   `json:"tokens_out"`
	CostUSD      *float64 `json:"cost_usd"`
	CostSource   string   `json:"cost_source"`
	LatencyS     *float64 `json:"latency_s"`
	Error        string   `json:"error"`
	Check        string   `json:"check,omitempty"`        // a contract check's name (G15); "" for an ordinary call
	CheckOK      *bool    `json:"check_ok,omitempty"`     // its result
	Detail       string   `json:"detail,omitempty"`       // why it failed, or what it saw
	FinishReason string   `json:"finish_reason"`          // why the model stopped ("length" = cut at the token cap); "" when unknown
	Step         *int64   `json:"step"`                   // the turn within a harness step/decision, 1-based; nil when not given
	DecisionKey  string   `json:"decision_key,omitempty"` // what the call served (triage: the signal's fingerprint)
	Role         string   `json:"role,omitempty"`         // the harness role that made the call (triage, verdict, coder, advisor, reviewer); "" when unknown
	Episode      string   `json:"episode,omitempty"`      // an id grouping the calls of one agent session within the run
	EpisodeSeq   *int64   `json:"episode_seq"`            // the episode's order within the run, 1-based; nil when not given
	Tools        string   `json:"tools,omitempty"`        // JSON array of the tools the model called in this turn, [{"name","arg","ok"}] (old rows: ["name", ...]; ParseTools reads both); "" when none
	Kind         string   `json:"kind"`                   // KindCall or KindStep; rows from before schema 25 are calls
	Version      int      `json:"version,omitempty"`      // the routine's version of the run that made the call; set when read
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
		kind := c.Kind
		if kind == "" {
			kind = KindCall
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO run_calls (run_id, routine, at, alias, endpoint, model, deployment,
			tokens_in, tokens_out, cost_usd, cost_source, latency_s, error, check_name, check_ok, detail, finish_reason, step, decision_key, role, episode, episode_seq, tools, kind, version)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, `+runVersion+`)`,
			c.RunID, c.Routine, at, c.Alias, c.Endpoint, c.Model, c.Deployment, c.TokensIn, c.TokensOut, c.CostUSD,
			c.CostSource, c.LatencyS, c.Error, c.Check, c.CheckOK, c.Detail, c.FinishReason, c.Step, nullIfEmpty(c.DecisionKey),
			c.Role, c.Episode, c.EpisodeSeq, c.Tools, kind, c.RunID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

const callCols = `run_id, routine, at, alias, endpoint, model, deployment, tokens_in, tokens_out, cost_usd, cost_source, latency_s, error,
	check_name, check_ok, detail, finish_reason, step, decision_key, role, episode, episode_seq, tools, kind, version`

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func scanCalls(rows *sql.Rows) ([]ModelCall, error) {
	defer rows.Close()
	var out []ModelCall
	for rows.Next() {
		var c ModelCall
		var tin, tout sql.NullInt64
		var cost, lat sql.NullFloat64
		var ok sql.NullBool
		var step sql.NullInt64
		var key sql.NullString
		var eseq sql.NullInt64
		if err := rows.Scan(&c.RunID, &c.Routine, &c.At, &c.Alias, &c.Endpoint, &c.Model, &c.Deployment, &tin, &tout,
			&cost, &c.CostSource, &lat, &c.Error, &c.Check, &ok, &c.Detail, &c.FinishReason, &step, &key, &c.Role, &c.Episode, &eseq, &c.Tools, &c.Kind, &c.Version); err != nil {
			return nil, err
		}
		if ok.Valid {
			c.CheckOK = &ok.Bool
		}
		if step.Valid {
			c.Step = &step.Int64
		}
		c.DecisionKey = key.String
		if eseq.Valid {
			c.EpisodeSeq = &eseq.Int64
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

// RunCalls returns each run's model calls and harness steps, in the order they were recorded.
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

// CallsSince returns every model call since a time, oldest first. Harness steps are left out: this is what
// aggregates of calls, tokens and cost are made from.
func (s *Store) CallsSince(ctx context.Context, since string) ([]ModelCall, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+callCols+` FROM run_calls WHERE at >= ? AND kind <> 'step' ORDER BY id`, since)
	if err != nil {
		return nil, err
	}
	return scanCalls(rows)
}
