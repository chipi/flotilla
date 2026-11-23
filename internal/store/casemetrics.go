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

// caseFleets names each case's fleet: the fleet of the routine whose detection opened it ("bugfix/intake" -> bugfix), or for a
// case a hook opened (its actor has no slash) the case's project, as the hook declares it.
const caseFleets = `WITH opener AS (SELECT case_id, MIN(seq) AS seq FROM case_events WHERE kind = 'case.detected' GROUP BY case_id),
	f AS (SELECT c.id, c.kind, c.status, c.resolution, CASE
			WHEN instr(e.actor, '/') > 0 THEN substr(e.actor, 1, instr(e.actor, '/') - 1)
			WHEN c.project <> '' THEN c.project
			ELSE 'unknown' END AS fleet
		FROM cases c LEFT JOIN opener o ON o.case_id = c.id LEFT JOIN case_events e ON e.seq = o.seq)`

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
