package api

import (
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/chipi/flotilla/internal/store"
)

func (s *Server) evalRoutes(m *http.ServeMux) {
	m.Handle("GET /v1/evals", s.auth(ActRead, s.evals))
	m.Handle("POST /v1/evals", s.auth(ActEval, s.evalRecord))
}

func (s *Server) evals(w http.ResponseWriter, r *http.Request, _ Caller) {
	limit := 50
	if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 && n <= 1000 {
		limit = n
	}
	out, err := s.Store.Evals(r.Context(), r.URL.Query().Get("routine"), limit)
	if err != nil {
		writeErr(w, err)
		return
	}
	if out == nil {
		out = []store.EvalResult{}
	}
	writeJSON(w, 200, map[string]any{"evals": out})
}

// evalRecord adds one measured number for a routine's prompt on a model. An operator's or admin's: an
// agent recording the numbers its own routine is judged by would be grading itself.
func (s *Server) evalRecord(w http.ResponseWriter, r *http.Request, c Caller) {
	var body struct {
		Routine   string  `json:"routine"`
		PromptSHA string  `json:"prompt_sha"`
		Model     string  `json:"model"`
		Metric    string  `json:"metric"`
		Value     float64 `json:"value"`
		N         int64   `json:"n"`
		Repeats   int64   `json:"repeats"`
		Source    string  `json:"source"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]string{"error": `body: {"routine", "prompt_sha", "model", "metric", "value", "n", "repeats", "source"}`})
		return
	}
	if math.IsNaN(body.Value) || math.IsInf(body.Value, 0) || body.N < 0 || body.Repeats < 0 {
		writeJSON(w, 400, map[string]string{"error": "value must be a number; n and repeats may not be negative"})
		return
	}
	id, err := s.Store.RecordEval(r.Context(), store.EvalResult{Routine: body.Routine, PromptSHA: body.PromptSHA, Model: body.Model,
		Metric: body.Metric, Value: body.Value, N: body.N, Repeats: body.Repeats, Source: body.Source,
		RecordedBy: strings.TrimPrefix(c.Name, "user:")})
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	_ = s.Store.Event(r.Context(), c.Name, "api", "eval.recorded", body.Routine,
		body.Metric+"="+strconv.FormatFloat(body.Value, 'g', -1, 64)+" prompt "+body.PromptSHA+" model "+body.Model)
	writeJSON(w, 201, map[string]string{"id": strconv.FormatInt(id, 10), "by": c.Name})
}
