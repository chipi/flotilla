package store

import "context"

// CaseCount is how many open cases one fleet has of one kind in one status.
type CaseCount struct {
	Fleet, Kind, Status string
	N                   int
}

// CaseMove is how many times one fleet's cases of one kind moved from one status to another by one role, since the store began:
// a case opening is the move from "" to detected. Resolution is the case's, set only on a move to closed.
type CaseMove struct {
	Fleet, Kind, From, To, By, Resolution string
	N                                     int
}

// caseFleets is the cases with the fleet kept on each (caseFleetOf); "unknown" for a hook's case whose hook declared no project.
const caseFleets = `WITH f AS (SELECT id, kind, status, resolution, CASE WHEN fleet <> '' THEN fleet ELSE 'unknown' END AS fleet FROM cases)`

// CaseMetrics returns the open cases by fleet, kind and status, and every status move cases have made by fleet, kind, from, to and
// role. Case events are never deleted, so a move's count only grows: a counter, read from the store, that a restart cannot lose.
func (s *Store) CaseMetrics(ctx context.Context) ([]CaseCount, []CaseMove, error) {
	rows, err := s.db.QueryContext(ctx, caseFleets+`
		SELECT fleet, kind, status, COUNT(*) FROM f WHERE status <> 'closed' GROUP BY 1, 2, 3 ORDER BY 1, 2, 3`)
	if err != nil {
		return nil, nil, err
	}
	var open []CaseCount
	for rows.Next() {
		var c CaseCount
		if err := rows.Scan(&c.Fleet, &c.Kind, &c.Status, &c.N); err != nil {
			rows.Close()
			return nil, nil, err
		}
		open = append(open, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	rows, err = s.db.QueryContext(ctx, caseFleets+`
		SELECT f.fleet, f.kind, e.from_status, e.to_status, e.by_role, CASE WHEN e.to_status = 'closed' THEN f.resolution ELSE '' END, COUNT(*)
		FROM case_events e JOIN f ON f.id = e.case_id
		WHERE e.kind LIKE 'case.%' AND e.to_status <> ''
		GROUP BY 1, 2, 3, 4, 5, 6 ORDER BY 1, 2, 3, 4, 5, 6`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var moves []CaseMove
	for rows.Next() {
		var m CaseMove
		if err := rows.Scan(&m.Fleet, &m.Kind, &m.From, &m.To, &m.By, &m.Resolution, &m.N); err != nil {
			return nil, nil, err
		}
		moves = append(moves, m)
	}
	return open, moves, rows.Err()
}

// LedgerCount is how many live decisions one fleet's routines wrote to the ledger with one disposition, since the store began.
type LedgerCount struct {
	Fleet, Disposition string
	N                  int
}

// LedgerMetrics counts the ledger's live decisions (a shadow's, the ledger's stage "shadow", left out) by fleet, the namespace's
// routine's, and disposition: what a fleet that files through the ledger, not cases (triage), decided. Entries are never deleted.
func (s *Store) LedgerMetrics(ctx context.Context) ([]LedgerCount, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT CASE WHEN instr(namespace, '/') > 0 THEN substr(namespace, 1, instr(namespace, '/') - 1)
			ELSE namespace END, coalesce(json_extract(row, '$.disposition'), ''), COUNT(*)
		FROM ledger_entries WHERE kind = 'decision' AND coalesce(json_extract(row, '$.stage'), '') <> 'shadow'
		GROUP BY 1, 2 ORDER BY 1, 2`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LedgerCount
	for rows.Next() {
		var c LedgerCount
		if err := rows.Scan(&c.Fleet, &c.Disposition, &c.N); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
