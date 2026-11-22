package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/chipi/flotilla/internal/hook"
	"github.com/chipi/flotilla/internal/store"
)

// HooksHandler is the hooks' own listener (hooks_listen): POST /hooks/{name} and /healthz, nothing else, so
// the plumbing tool's network reaches no other route of flotillad's.
func (s *Server) HooksHandler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"ok": true})
	})
	m.HandleFunc("POST /hooks/{name}", s.hookCall)
	return m
}

// hookCall is POST /hooks/{name} (RFC-0014 §4.2): signed with the hook's secret, never a bearer token. A call opens one case of the hook's kind, or attaches to the open one with the
// same key; it never starts a routine. Every call to a declared hook is in the events, accepted or refused,
// except the ones its rate cuts off, which are logged: a flood must not grow the store.
func (s *Server) hookCall(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	h := s.Hooks[name]
	if h == nil {
		writeJSON(w, 404, map[string]string{"error": "no such hook"})
		return
	}
	now := time.Now()
	if !h.Allow(now) {
		s.Log.Warn("hook call over its rate", "hook", name)
		writeJSON(w, 429, map[string]string{"error": "over the hook's rate"})
		return
	}
	actor := "hook:" + name
	refuse := func(code int, why string) {
		if err := s.Store.Event(r.Context(), actor, "hook", "hook.refused", actor, why); err != nil {
			s.Log.Error("record a refused hook call", "hook", name, "err", err)
		}
		writeJSON(w, code, map[string]string{"error": why})
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, h.MaxBodyBytes()))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			refuse(413, "the body is over the hook's cap")
			return
		}
		refuse(400, "the body could not be read")
		return
	}
	delivery := r.Header.Get(hook.HeaderDelivery)
	if err := h.Verify(r.Header.Get(hook.HeaderTimestamp), delivery, r.Header.Get(hook.HeaderSignature), body, now); err != nil {
		refuse(401, err.Error())
		return
	}
	p, err := h.Parse(body)
	if err != nil {
		refuse(400, err.Error())
		return
	}
	data, err := json.Marshal(p.Data)
	if err != nil {
		refuse(400, "the payload could not be kept")
		return
	}
	detail := "delivery " + delivery
	if len(p.Ignored) > 0 {
		detail += "; fields not declared, ignored: " + strings.Join(p.Ignored, ", ")
	}
	k := store.Key{Kind: actor, Value: p.Target}
	res, err := s.Store.AcceptHookCall(r.Context(), store.HookCall{Hook: name, Delivery: delivery, Detail: detail, Keep: 2 * hook.Window, Close: h.CloseOnArrival,
		Case: store.NewCase{Title: h.Title, Kind: h.Kind, Target: p.Target, Project: h.Project, Urgency: h.Urgency, Keys: []store.Key{k}, Canonical: k, Data: string(data)}})
	if err != nil {
		s.Log.Error("hook call", "hook", name, "delivery", delivery, "err", err)
		if res.CaseID == "" {
			writeJSON(w, 500, map[string]string{"error": "the case could not be opened"})
			return
		}
	}
	how := "attached"
	switch {
	case res.Replay:
		how = "replay"
	case res.Opened:
		how = "opened"
	}
	if err := s.Store.Event(r.Context(), actor, "hook", "hook.accepted", res.CaseID, how+"; "+detail); err != nil {
		s.Log.Error("record an accepted hook call", "hook", name, "err", err)
	}
	s.Log.Info("hook call", "hook", name, "delivery", delivery, "case", res.CaseID, "how", how)
	code := 202
	if res.Replay {
		code = 200
	}
	writeJSON(w, code, map[string]any{"case": res.CaseID, "how": how})
}
