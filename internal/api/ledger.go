package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/chipi/flotilla/internal/store"
)

// The fleets' ledger (RFC-0002 §9 step 5), for a worker's routine. Every call names its namespace
// (?ns=, the routine's ledger). A write carries the run id that made it.

const maxLedgerBody = 1 << 20     // one row, with room
const maxLedgerImport = 256 << 20 // a namespace's whole history, once

func ledgerNS(w http.ResponseWriter, r *http.Request) (string, bool) {
	ns := r.URL.Query().Get("ns")
	if ns == "" || len(ns) > 200 {
		writeJSON(w, 400, map[string]string{"error": "ns (the ledger's namespace) is required"})
		return "", false
	}
	return ns, true
}

func readBody(w http.ResponseWriter, r *http.Request, max int64, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, max)).Decode(v); err != nil {
		writeJSON(w, 400, map[string]string{"error": "body: " + err.Error()})
		return false
	}
	return true
}

func (s *Server) ledgerFilings(w http.ResponseWriter, r *http.Request, _ Caller) {
	ns, ok := ledgerNS(w, r)
	if !ok {
		return
	}
	keys := map[string]string{}
	for k, v := range r.URL.Query() {
		if k != "ns" && len(v) > 0 {
			keys[k] = v[0]
		}
	}
	fs, err := s.Store.Filings(r.Context(), ns, keys)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	if fs == nil {
		fs = []store.Filing{}
	}
	writeJSON(w, 200, map[string]any{"filings": fs})
}

func (s *Server) ledgerWriteFiling(w http.ResponseWriter, r *http.Request, _ Caller) {
	ns, ok := ledgerNS(w, r)
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
	seq, err := s.Store.WriteFiling(r.Context(), ns, body.Row, body.RunID)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"seq": seq})
}

func (s *Server) ledgerEntries(w http.ResponseWriter, r *http.Request, _ Caller) {
	ns, ok := ledgerNS(w, r)
	if !ok {
		return
	}
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	limit := 5000
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 && v <= 5000 {
		limit = v
	}
	es, err := s.Store.Entries(r.Context(), ns, after, limit)
	if err != nil {
		writeErr(w, err)
		return
	}
	if es == nil {
		es = []store.Entry{}
	}
	writeJSON(w, 200, map[string]any{"entries": es})
}

func (s *Server) ledgerAppendEntry(w http.ResponseWriter, r *http.Request, _ Caller) {
	ns, ok := ledgerNS(w, r)
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
	seq, err := s.Store.AppendEntry(r.Context(), ns, body.Kind, body.Row, body.RunID)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"seq": seq})
}

func (s *Server) ledgerCounts(w http.ResponseWriter, r *http.Request, _ Caller) {
	ns, ok := ledgerNS(w, r)
	if !ok {
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
	ns, ok := ledgerNS(w, r)
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
