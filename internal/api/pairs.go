package api

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/chipi/flotilla/internal/store"
)

// A shadow version's pairs (v2 step 2, design section 4): per subject, what the shadow version and the live version each decided on the
// same frozen input. flotilla shows them and counts them; it does not say which is better. The JSON is what the installation's comparisons
// and evals read, filtered by version and time.

func (s *Server) pairRoutes(m *http.ServeMux) {
	m.Handle("GET /v1/routines/{fleet}/{routine}/pairs", s.auth(ActRead, s.pairsList))
}

// PairsOf reads a shadow version's pairs, optionally in a window (RFC 3339 times, the live runs' starts). show narrows the list: all
// (the default), differ (paired and different) or unpaired.
func (s *Server) PairsOf(ctx context.Context, c Caller, routineID string, version int, since, until, show string) (store.PairSet, error) {
	if err := s.Check(ctx, c, ActRead); err != nil {
		return store.PairSet{}, err
	}
	if _, err := s.routineByID(routineID); err != nil {
		return store.PairSet{}, err
	}
	if version < 1 {
		return store.PairSet{}, problem(400, "version is vN: v1, v2, ...")
	}
	norm := func(name, v string) (string, error) {
		if v == "" {
			return "", nil
		}
		t, err := time.Parse(time.RFC3339Nano, v)
		if err != nil {
			return "", problem(400, "%s %q: an RFC 3339 time", name, v)
		}
		return t.UTC().Format(store.TimeFormat), nil
	}
	var err error
	if since, err = norm("since", since); err != nil {
		return store.PairSet{}, err
	}
	if until, err = norm("until", until); err != nil {
		return store.PairSet{}, err
	}
	ps, err := s.Store.Pairs(ctx, store.PairQuery{Routine: routineID, Version: version, Since: since, Until: until})
	if err != nil {
		return ps, err
	}
	return FilterPairs(ps, show)
}

// FilterPairs keeps the pairs a view shows: differ (both decided, differently) or unpaired (one side only); the counts stay the whole set's.
func FilterPairs(ps store.PairSet, show string) (store.PairSet, error) {
	switch show {
	case "", "all":
		return ps, nil
	case "differ", "unpaired":
	default:
		return ps, problem(400, "show is all, differ or unpaired")
	}
	kept := []store.Pair{}
	for _, p := range ps.Pairs {
		paired := p.Live != nil && p.Shadow != nil
		if (show == "differ" && paired && !p.Same) || (show == "unpaired" && !paired) {
			kept = append(kept, p)
		}
	}
	ps.Pairs = kept
	return ps, nil
}

func (s *Server) pairsList(w http.ResponseWriter, r *http.Request, c Caller) {
	q := r.URL.Query()
	n, _ := strconv.Atoi(strings.TrimPrefix(q.Get("version"), "v"))
	ps, err := s.PairsOf(r.Context(), c, routineID(r), n, q.Get("since"), q.Get("until"), q.Get("show"))
	if err != nil {
		writeAction(w, err, 0, nil)
		return
	}
	writeJSON(w, 200, ps)
}
