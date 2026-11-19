package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/chipi/flotilla/internal/runtoken"
	"github.com/chipi/flotilla/internal/store"
)

// The fleets' ledger (RFC-0002 §9 step 5), for a worker's routine. Every call names its namespace
// (?ns=, the routine's ledger). A write carries the run id that made it.

const maxLedgerBody = 1 << 20     // one row, with room
const maxLedgerImport = 256 << 20 // a namespace's whole history, once

// ledgerNS is the namespace of a request. A run's own token names it: the routine key recorded on the run when it started, and
// whatever ?ns= says is ignored (and logged when it differs), so no run can read or write under another routine's or version's
// name. Any other caller names it in ?ns=.
func (s *Server) ledgerNS(w http.ResponseWriter, r *http.Request, who Caller) (string, bool) {
	ns := r.URL.Query().Get("ns")
	if who.Run != nil {
		if own := who.Run.Key(); ns != own {
			if ns != "" {
				s.Log.Warn("a run named another ledger namespace; its own is used", "run_id", who.Run.ID, "own", own, "named", ns)
			}
			ns = own
		}
		return ns, true
	}
	if ns == "" || len(ns) > 200 {
		writeJSON(w, 400, map[string]string{"error": "ns (the ledger's namespace) is required"})
		return "", false
	}
	return ns, true
}

// ledgerRunID is the run id a ledger write is recorded under: a run's own token fixes it, so a row never carries another run's id.
func (s *Server) ledgerRunID(who Caller, claimed string) string {
	if who.Run == nil {
		return claimed
	}
	if claimed != "" && claimed != who.Run.ID {
		s.Log.Warn("a run wrote under another run's id; its own is used", "run_id", who.Run.ID, "claimed", claimed)
	}
	return who.Run.ID
}

func readBody(w http.ResponseWriter, r *http.Request, max int64, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, max)).Decode(v); err != nil {
		writeJSON(w, 400, map[string]string{"error": "body: " + err.Error()})
		return false
	}
	return true
}

func (s *Server) ledgerFilings(w http.ResponseWriter, r *http.Request, who Caller) {
	ns, ok := s.ledgerNS(w, r, who)
	if !ok {
		return
	}
	keys := map[string]string{}
	for k, v := range r.URL.Query() {
		if k != "ns" && len(v) > 0 {
			keys[k] = v[0]
		}
	}
	var fs []store.Filing
	var err error
	if in, shadow, ok := s.shadowInput(w, r, who); !ok {
		return
	} else if shadow {
		fs, err = s.Store.FilingsAsOf(r.Context(), who.Run.Routine, ns, in.MarkFilings, keys)
	} else {
		fs, err = s.Store.Filings(r.Context(), ns, keys)
	}
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	if fs == nil {
		fs = []store.Filing{}
	}
	writeJSON(w, 200, map[string]any{"filings": fs})
}

func (s *Server) ledgerWriteFiling(w http.ResponseWriter, r *http.Request, who Caller) {
	ns, ok := s.ledgerNS(w, r, who)
	if !ok {
		return
	}
	var body struct {
		Row   map[string]string `json:"row"`
		RunID string            `json:"run_id"`
	}
	if !readBody(w, r, maxLedgerBody, &body) {
		return
	}
	seq, err := s.Store.WriteFiling(r.Context(), ns, body.Row, s.ledgerRunID(who, body.RunID))
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"seq": seq})
}

func (s *Server) ledgerEntries(w http.ResponseWriter, r *http.Request, who Caller) {
	ns, ok := s.ledgerNS(w, r, who)
	if !ok {
		return
	}
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	limit := 5000
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 && v <= 5000 {
		limit = v
	}
	var es []store.Entry
	var err error
	if in, shadow, ok := s.shadowInput(w, r, who); !ok {
		return
	} else if shadow {
		es, err = s.Store.EntriesAsOf(r.Context(), who.Run.Routine, ns, in.MarkEntries, after, limit)
	} else {
		es, err = s.Store.Entries(r.Context(), ns, after, limit)
	}
	if err != nil {
		writeErr(w, err)
		return
	}
	if es == nil {
		es = []store.Entry{}
	}
	writeJSON(w, 200, map[string]any{"entries": es})
}

func (s *Server) ledgerAppendEntry(w http.ResponseWriter, r *http.Request, who Caller) {
	ns, ok := s.ledgerNS(w, r, who)
	if !ok {
		return
	}
	var body struct {
		Kind  string            `json:"kind"`
		Row   map[string]string `json:"row"`
		RunID string            `json:"run_id"`
	}
	if !readBody(w, r, maxLedgerBody, &body) {
		return
	}
	seq, err := s.Store.AppendEntry(r.Context(), ns, body.Kind, body.Row, s.ledgerRunID(who, body.RunID))
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	if who.Run != nil && who.Run.Shadow && body.Kind == "decision" {
		if err := s.recordShadowDecision(r.Context(), who.Run, body.Row); err != nil {
			s.Log.Error("a shadow's decision was not recorded as a pair record", "run_id", who.Run.ID, "err", err)
			writeErr(w, err)
			return
		}
	}
	writeJSON(w, 200, map[string]any{"seq": seq})
}

func (s *Server) ledgerCounts(w http.ResponseWriter, r *http.Request, who Caller) {
	ns, ok := s.ledgerNS(w, r, who)
	if !ok {
		return
	}
	if in, shadow, ok := s.shadowInput(w, r, who); !ok {
		return
	} else if shadow {
		c, err := s.shadowCounts(r.Context(), who.Run, ns, in)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, c)
		return
	}
	c, err := s.Store.LedgerCounts(r.Context(), ns)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, c)
}

func (s *Server) ledgerImport(w http.ResponseWriter, r *http.Request, who Caller) {
	if who.Run != nil {
		writeJSON(w, 403, map[string]string{"error": "a run does not import a ledger"})
		return
	}
	ns, ok := s.ledgerNS(w, r, who)
	if !ok {
		return
	}
	var body struct {
		Filings []map[string]string `json:"filings"`
		Entries []store.Entry       `json:"entries"`
	}
	if !readBody(w, r, maxLedgerImport, &body) {
		return
	}
	if len(body.Filings) == 0 && len(body.Entries) == 0 {
		writeJSON(w, 400, map[string]string{"error": errors.New("nothing to import").Error()})
		return
	}
	c, err := s.Store.ImportLedger(r.Context(), ns, body.Filings, body.Entries)
	if err != nil {
		writeJSON(w, 409, map[string]string{"error": err.Error()})
		return
	}
	_ = s.Store.Event(r.Context(), who.Name, "api", "ledger.import", ns,
		strconv.Itoa(c.Filings)+" filings, "+strconv.Itoa(c.Decisions)+" decisions, "+strconv.Itoa(c.Recurrences)+" recurrences")
	writeJSON(w, 200, c)
}

// shadowInput is the input a shadow caller reads its ledger as of: the live run's recorded marks. shadow is false for every
// other caller; ok is false (and the answer written) when a shadow's input is gone.
func (s *Server) shadowInput(w http.ResponseWriter, r *http.Request, who Caller) (store.Input, bool, bool) {
	if who.Run == nil || !who.Run.Shadow {
		return store.Input{}, false, true
	}
	in, err := s.Store.Input(r.Context(), who.Run.InputKey)
	if err != nil {
		writeJSON(w, 409, map[string]string{"error": "the shadow's input is not recorded: " + err.Error()})
		return in, true, false
	}
	return in, true, true
}

// shadowCounts are the counts of the ledger a shadow sees: the live rows as of its input's marks plus its own.
func (s *Server) shadowCounts(ctx context.Context, run *runtoken.Run, own string, in store.Input) (store.LedgerCounts, error) {
	var c store.LedgerCounts
	fs, err := s.Store.FilingsAsOf(ctx, run.Routine, own, in.MarkFilings, nil)
	if err != nil {
		return c, err
	}
	c.Filings = len(fs)
	es, err := s.Store.EntriesAsOf(ctx, run.Routine, own, in.MarkEntries, 0, 0)
	if err != nil {
		return c, err
	}
	for _, e := range es {
		if e.Kind == "recurrence" {
			c.Recurrences++
		} else {
			c.Decisions++
		}
	}
	return c, nil
}

// recordShadowDecision is a shadow's ledger decision as a pair record: the input's key and the decision's subject.
func (s *Server) recordShadowDecision(ctx context.Context, run *runtoken.Run, row map[string]string) error {
	payload, _ := json.Marshal(row)
	_, err := s.Store.RecordShadow(ctx, store.ShadowRecord{RunID: run.ID, Routine: run.Key(), Version: run.Version, VersionHash: s.versionHash(ctx, run),
		InputKey: run.InputKey, Subject: store.DecisionSubject(row, row["occurrence_id"], row["fingerprint"]), Kind: "decision", Payload: payload})
	return err
}

// versionHash is the recorded hash of the version a run is, "" when it cannot be read.
func (s *Server) versionHash(ctx context.Context, run *runtoken.Run) string {
	vs, err := s.Store.Versions(ctx, run.Routine)
	if err != nil {
		return ""
	}
	for _, v := range vs {
		if v.Version == run.Version {
			return v.Hash
		}
	}
	return ""
}
