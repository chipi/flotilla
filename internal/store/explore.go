package store

import (
	"context"
	"fmt"
	"strings"
)

// The ledger explorer (the operator, 2026-11-23): the store's three record streams read as one, for finding out by hand what
// happened when nothing else says. Read only. platform is the events table (who changed what, through which door), case is
// each case's timeline, ledger is the fleets' own ledgers (the triage cycle's decisions and recurrences). Runs and model calls
// have pages of their own and are not a stream here.

// Explore streams.
const (
	StreamPlatform = "platform"
	StreamCase     = "case"
	StreamLedger   = "ledger"
)

// exploreUnion is the three streams in one shape: stream, seq, at, kind, actor, via, subject, detail, raw, case_id, run_id.
// A case event's via is the role that moved it and its detail leads with the status change; a ledger entry's kind is its
// namespace and kind, its subject the fingerprint, its detail the occurrence id, its raw the row as written.
const exploreUnion = `
	SELECT 'platform' AS stream, seq, at, kind, actor, via, subject, detail, '' AS raw, '' AS case_id,
		CASE WHEN kind LIKE 'run.%' THEN subject ELSE '' END AS run_id
	FROM events
	UNION ALL
	SELECT 'case', seq, at, kind, actor, by_role, case_id,
		CASE WHEN to_status <> '' THEN from_status || ' -> ' || to_status || CASE WHEN detail <> '' THEN ': ' || detail ELSE '' END ELSE detail END,
		data, case_id, ''
	FROM case_events
	UNION ALL
	SELECT 'ledger', seq, written_at, namespace || ' ' || kind, namespace, 'ledger', fingerprint, occurrence_id, row, '', run_id
	FROM ledger_entries`

// ExploreQuery is one explorer view. Streams empty is all three. Ref follows one thing everywhere: a row whose subject, case,
// run, actor or occurrence is exactly Ref. Text is a substring of the kind, actor, subject, detail or raw record. From and To
// bound the time (store format, inclusive). Sort is at (the default), kind, actor or stream.
type ExploreQuery struct {
	Streams       []string
	Kind, Actor   string
	Ref, Text     string
	From, To      string
	Sort          string
	Desc          bool
	Limit, Offset int
}

// ExploreRow is one record, as stored.
type ExploreRow struct {
	Stream               string
	Seq                  int64
	At, Kind, Actor, Via string
	Subject, Detail, Raw string
	CaseID, RunID        string
}

// KindCount is how many rows one stream's kind has under a view's other filters.
type KindCount struct {
	Stream, Kind string
	N            int
}

// ExploreSorts are the columns an explorer view sorts by.
var ExploreSorts = map[string]string{"at": "at", "kind": "kind", "actor": "actor", "stream": "stream"}

func (q ExploreQuery) where(withKind bool) (string, []any) {
	var conds []string
	var args []any
	if len(q.Streams) > 0 {
		conds = append(conds, "stream IN ("+strings.TrimSuffix(strings.Repeat("?,", len(q.Streams)), ",")+")")
		for _, s := range q.Streams {
			args = append(args, s)
		}
	}
	if withKind && q.Kind != "" {
		conds = append(conds, "kind = ?")
		args = append(args, q.Kind)
	}
	if q.Actor != "" {
		conds = append(conds, "actor = ?")
		args = append(args, q.Actor)
	}
	if q.Ref != "" {
		conds = append(conds, "(subject = ? OR case_id = ? OR run_id = ? OR actor = ? OR (stream = 'ledger' AND detail = ?))")
		args = append(args, q.Ref, q.Ref, q.Ref, q.Ref, q.Ref)
	}
	if q.Text != "" {
		like := "%" + likeEscape(q.Text) + "%"
		conds = append(conds, `(kind LIKE ? ESCAPE '\' OR actor LIKE ? ESCAPE '\' OR subject LIKE ? ESCAPE '\' OR detail LIKE ? ESCAPE '\' OR raw LIKE ? ESCAPE '\')`)
		args = append(args, like, like, like, like, like)
	}
	if q.From != "" {
		conds = append(conds, "at >= ?")
		args = append(args, q.From)
	}
	if q.To != "" {
		conds = append(conds, "at <= ?")
		args = append(args, q.To)
	}
	if len(conds) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

// Explore reads one view: its page of rows, how many rows match, and the count of each stream's kinds under the view's other
// filters (so a kind list says what choosing it would show).
func (s *Store) Explore(ctx context.Context, q ExploreQuery) ([]ExploreRow, int, []KindCount, error) {
	where, args := q.where(true)
	var total int
	if err := s.rdb.QueryRowContext(ctx, `SELECT count(*) FROM (`+exploreUnion+`)`+where, args...).Scan(&total); err != nil {
		return nil, 0, nil, err
	}
	col, ok := ExploreSorts[q.Sort]
	if !ok {
		col = "at"
	}
	dir := "ASC"
	if q.Desc {
		dir = "DESC"
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.rdb.QueryContext(ctx, `SELECT stream, seq, at, kind, actor, via, subject, detail, raw, case_id, run_id FROM (`+exploreUnion+`)`+where+
		fmt.Sprintf(" ORDER BY %s %s, at %s, seq %s LIMIT ? OFFSET ?", col, dir, dir, dir), append(args, limit, q.Offset)...)
	if err != nil {
		return nil, 0, nil, err
	}
	var out []ExploreRow
	for rows.Next() {
		var r ExploreRow
		if err := rows.Scan(&r.Stream, &r.Seq, &r.At, &r.Kind, &r.Actor, &r.Via, &r.Subject, &r.Detail, &r.Raw, &r.CaseID, &r.RunID); err != nil {
			rows.Close()
			return nil, 0, nil, err
		}
		out = append(out, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, 0, nil, err
	}
	kwhere, kargs := q.where(false)
	krows, err := s.rdb.QueryContext(ctx, `SELECT stream, kind, count(*) FROM (`+exploreUnion+`)`+kwhere+` GROUP BY stream, kind ORDER BY stream, count(*) DESC, kind`, kargs...)
	if err != nil {
		return nil, 0, nil, err
	}
	defer krows.Close()
	var kinds []KindCount
	for krows.Next() {
		var k KindCount
		if err := krows.Scan(&k.Stream, &k.Kind, &k.N); err != nil {
			return nil, 0, nil, err
		}
		kinds = append(kinds, k)
	}
	return out, total, kinds, krows.Err()
}
