package store

import "context"

// RoutineCallsSince returns one routine's model calls since a time, oldest first. It reads every row the
// routine wrote to run_calls (contract checks and harness step rows included); the caller filters.
func (s *Store) RoutineCallsSince(ctx context.Context, routine, since string) ([]ModelCall, error) {
	// shadow-ok: one exact routine key; a live key never matches a shadow's rows, and a shadow's key asks for them
	rows, err := s.rdb.QueryContext(ctx, `SELECT `+callCols+` FROM run_calls WHERE routine = ? AND at >= ? ORDER BY at, id`, routine, since)
	if err != nil {
		return nil, err
	}
	return scanCalls(rows)
}
