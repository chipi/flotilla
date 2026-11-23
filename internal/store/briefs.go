package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// The morning brief (G11): one per day, written once and kept as written; a person's "more" or "less" on
// an item is recorded against the item's kind, the evidence a future brief playbook change is proposed on.

// Brief is one day's brief as it was written.
type Brief struct {
	Day       string         `json:"day"` // YYYY-MM-DD in the installation's timezone
	WrittenAt string         `json:"written_at"`
	WrittenBy string         `json:"written_by"`
	Headline  string         `json:"headline"`
	Summary   string         `json:"summary"`
	Sections  []BriefSection `json:"sections"`
}

// SummaryKind is the event a monitor's summary lines are kept as: subject the routine, detail the JSON
// of a Summary. The brief written after the run shows them under the section the routine's spec names
// (RFC-0012 §4).
const SummaryKind = "routine.summary"

// Summary is one monitor run's summary lines.
type Summary struct {
	Run     string   `json:"run"`
	Section string   `json:"section"`
	Lines   []string `json:"lines"`
}

type BriefSection struct {
	Title string      `json:"title"`
	Items []BriefItem `json:"items"`
}

// BriefItem is one line; Kind is what a mark counts against ("needs:failed_backup", "wrong:stale").
type BriefItem struct {
	Text string `json:"text"`
	Case string `json:"case,omitempty"`
	Href string `json:"href,omitempty"`
	Kind string `json:"kind"`
}

var ErrBriefExists = errors.New("that day already has a brief")

// SaveBrief keeps a day's brief; a day is written once.
func (s *Store) SaveBrief(ctx context.Context, b Brief) error {
	body, err := json.Marshal(b)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO briefs (day, written_at, body) VALUES (?, ?, ?)`, b.Day, b.WrittenAt, string(body))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrBriefExists
	}
	return nil
}

// BriefOn is a day's brief, or nil.
func (s *Store) BriefOn(ctx context.Context, day string) (*Brief, error) {
	var body string
	err := s.rdb.QueryRowContext(ctx, `SELECT body FROM briefs WHERE day = ?`, day).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var b Brief
	return &b, json.Unmarshal([]byte(body), &b)
}

// Briefs is every brief, newest first, at most limit.
func (s *Store) Briefs(ctx context.Context, limit int) ([]Brief, error) {
	rows, err := s.rdb.QueryContext(ctx, `SELECT body FROM briefs ORDER BY day DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Brief
	for rows.Next() {
		var body string
		if err := rows.Scan(&body); err != nil {
			return nil, err
		}
		var b Brief
		if err := json.Unmarshal([]byte(body), &b); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// Brief marks.
const (
	MarkMore = "more"
	MarkLess = "less"
)

// MarkBriefItem keeps a person's mark on one item of a day's brief; a second mark replaces the first.
func (s *Store) MarkBriefItem(ctx context.Context, day string, item int, user, mark string) error {
	if mark != MarkMore && mark != MarkLess {
		return fmt.Errorf("a mark is more or less, not %q", mark)
	}
	b, err := s.BriefOn(ctx, day)
	if err != nil {
		return err
	}
	if b == nil {
		return fmt.Errorf("no brief on %s", day)
	}
	kind := ""
	n := 0
	for _, sec := range b.Sections {
		for _, it := range sec.Items {
			if n == item {
				kind = it.Kind
			}
			n++
		}
	}
	if item < 0 || item >= n {
		return fmt.Errorf("the brief of %s has no item %d", day, item)
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO brief_marks (day, item, kind, user_name, mark, at) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(day, item, user_name) DO UPDATE SET mark = excluded.mark, at = excluded.at`,
		day, item, kind, user, mark, s.now().Format(ts))
	return err
}

// BriefMarks is a person's marks on a day's brief, by item.
func (s *Store) BriefMarks(ctx context.Context, day, user string) (map[int]string, error) {
	rows, err := s.rdb.QueryContext(ctx, `SELECT item, mark FROM brief_marks WHERE day = ? AND user_name = ?`, day, user)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int]string{}
	for rows.Next() {
		var i int
		var m string
		if err := rows.Scan(&i, &m); err != nil {
			return nil, err
		}
		out[i] = m
	}
	return out, rows.Err()
}

// MarksByKind counts everyone's marks per item kind since a time: [more, less].
func (s *Store) MarksByKind(ctx context.Context, since string) (map[string][2]int, error) {
	rows, err := s.rdb.QueryContext(ctx, `SELECT kind, mark, COUNT(*) FROM brief_marks WHERE at >= ? GROUP BY kind, mark`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][2]int{}
	for rows.Next() {
		var k, m string
		var n int
		if err := rows.Scan(&k, &m, &n); err != nil {
			return nil, err
		}
		v := out[k]
		if m == MarkMore {
			v[0] += n
		} else {
			v[1] += n
		}
		out[k] = v
	}
	return out, rows.Err()
}
