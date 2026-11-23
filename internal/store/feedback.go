package store

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/chipi/flotilla/internal/cases"
)

// A person's verdict on a case's diagnosis (G8), and notes: what a person proposes agents should know.
// A note takes effect only once someone activates it; until then it is "proposed".

const (
	VerdictRight = "right"
	VerdictWrong = "wrong"

	NoteProposed = "proposed"
	NoteActive   = "active"
	NoteDropped  = "dropped"
)

type Feedback struct {
	CaseID  string `json:"case_id"`
	User    string `json:"user"`
	Verdict string `json:"verdict"`
	At      string `json:"at"`
}

type Note struct {
	ID        int64  `json:"id"`
	Scope     string `json:"scope"`
	Text      string `json:"text"`
	FromCase  string `json:"from_case"`
	By        string `json:"by"`
	Status    string `json:"status"`
	CreatedAt string `json:"created_at"`
	DecidedAt string `json:"decided_at"`
	DecidedBy string `json:"decided_by"`
}

// SetVerdict keeps a person's verdict on a case; a second answer replaces their first.
func (s *Store) SetVerdict(ctx context.Context, caseID, user, verdict string) error {
	if verdict != VerdictRight && verdict != VerdictWrong {
		return fmt.Errorf("a verdict is right or wrong, not %q", verdict)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	at := s.now().Format(ts)
	if _, err := tx.ExecContext(ctx, `INSERT INTO case_feedback (case_id, user_name, verdict, at) VALUES (?, ?, ?, ?)
		ON CONFLICT(case_id, user_name) DO UPDATE SET verdict = excluded.verdict, at = excluded.at`, caseID, user, verdict, at); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO case_events (case_id, at, kind, actor, by_role, detail) VALUES (?, ?, ?, ?, ?, ?)`,
		caseID, at, "feedback."+verdict, "user:"+user, "operator", "the diagnosis was "+verdict); err != nil {
		return err
	}
	return tx.Commit()
}

// CaseFeedback is everyone's verdict on a case.
func (s *Store) CaseFeedback(ctx context.Context, caseID string) ([]Feedback, error) {
	rows, err := s.rdb.QueryContext(ctx, `SELECT case_id, user_name, verdict, at FROM case_feedback WHERE case_id = ? ORDER BY at`, caseID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Feedback
	for rows.Next() {
		var f Feedback
		if err := rows.Scan(&f.CaseID, &f.User, &f.Verdict, &f.At); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// VerdictsByAction counts the verdicts on the cases each action planned (an action's record, G8).
func (s *Store) VerdictsByAction(ctx context.Context) (map[string][2]int, error) {
	rows, err := s.rdb.QueryContext(ctx, `SELECT p.action, f.verdict, COUNT(*) FROM case_feedback f
		JOIN (SELECT DISTINCT case_id, action FROM plans) p ON p.case_id = f.case_id GROUP BY p.action, f.verdict`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][2]int{}
	for rows.Next() {
		var action, verdict string
		var n int
		if err := rows.Scan(&action, &verdict, &n); err != nil {
			return nil, err
		}
		v := out[action]
		if verdict == VerdictRight {
			v[0] += n
		} else {
			v[1] += n
		}
		out[action] = v
	}
	return out, rows.Err()
}

// ProposeNote keeps a note as proposed.
func (s *Store) ProposeNote(ctx context.Context, scope, text, fromCase, user string) (int64, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO notes (scope, text, from_case, by_user, status, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		scope, text, fromCase, user, NoteProposed, s.now().Format(ts))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// DecideNote activates or drops a proposed note (or drops an active one); anything else is refused.
func (s *Store) DecideNote(ctx context.Context, id int64, to, user string) error {
	from := NoteProposed
	switch to {
	case NoteActive:
	case NoteDropped:
		from = ""
	default:
		return fmt.Errorf("a note becomes active or dropped, not %q", to)
	}
	q := `UPDATE notes SET status = ?, decided_at = ?, decided_by = ? WHERE id = ? AND status <> ?`
	args := []any{to, s.now().Format(ts), user, id, to}
	if from != "" {
		q += ` AND status = ?`
		args = append(args, from)
	}
	res, err := s.db.ExecContext(ctx, q, args...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("note %d is not there, or not %s", id, map[string]string{NoteActive: "proposed", NoteDropped: "proposed or active"}[to])
	}
	return nil
}

// Notes lists notes in a status ("" every one), newest first.
func (s *Store) Notes(ctx context.Context, status string) ([]Note, error) {
	q := `SELECT id, scope, text, from_case, by_user, status, created_at, decided_at, decided_by FROM notes`
	var rows *sql.Rows
	var err error
	if status != "" {
		rows, err = s.rdb.QueryContext(ctx, q+` WHERE status = ? ORDER BY id DESC`, status)
	} else {
		rows, err = s.rdb.QueryContext(ctx, q+` ORDER BY id DESC`)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Note
	for rows.Next() {
		var n Note
		if err := rows.Scan(&n.ID, &n.Scope, &n.Text, &n.FromCase, &n.By, &n.Status, &n.CreatedAt, &n.DecidedAt, &n.DecidedBy); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// AnswersByAction counts the operator's answers to proposals by the action proposed: [approved, rejected]. An answer is the
// operator's move off awaiting_approval (to executing: approved; to escalated: rejected), and it belongs to the case's latest plan
// decided before it, so a case that proposed two actions in turn (a meeting's tasks, then its decisions) counts each answer once.
func (s *Store) AnswersByAction(ctx context.Context) (map[string][2]int, error) {
	rows, err := s.rdb.QueryContext(ctx, `SELECT (SELECT p.action FROM plans p WHERE p.case_id = e.case_id AND p.decided_at <= e.at ORDER BY p.id DESC LIMIT 1),
			e.to_status, COUNT(*)
		FROM case_events e
		WHERE e.from_status = ? AND e.by_role = ? AND e.to_status IN (?, ?)
		GROUP BY 1, 2`, cases.AwaitingApproval, cases.ByOperator, cases.Executing, cases.Escalated)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][2]int{}
	for rows.Next() {
		var action sql.NullString
		var to string
		var n int
		if err := rows.Scan(&action, &to, &n); err != nil {
			return nil, err
		}
		if !action.Valid {
			continue
		}
		v := out[action.String]
		if to == cases.Executing {
			v[0] += n
		} else {
			v[1] += n
		}
		out[action.String] = v
	}
	return out, rows.Err()
}
