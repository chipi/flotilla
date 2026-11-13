package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"

	"github.com/chipi/flotilla/internal/store"
)

// ADR-0014: people. An admin manages users. The callers-file operator token may add the first admin, or
// an admin again (break-glass), and nothing else here. The HTTP handlers and the UI call the same
// functions below.

var userNameRe = regexp.MustCompile(`^[a-z][a-z0-9._-]{1,39}$`)

// reservedNames are actors flotillad writes itself; no person may take one.
var reservedNames = map[string]bool{"flotillad": true, "operator": true, "admin": true, "system": true}

func (s *Server) userRoutes(m *http.ServeMux) {
	m.Handle("GET /v1/users", s.auth(ActUsers, s.listUsers))
	m.Handle("POST /v1/users", s.auth(ActUsers, s.addUser))
	m.Handle("POST /v1/users/{name}/role", s.auth(ActUsers, s.setRole))
	m.Handle("POST /v1/users/{name}/rotate", s.auth(ActUsers, s.rotateUser))
	m.Handle("POST /v1/users/{name}/revoke", s.auth(ActUsers, s.revokeUser))
	m.Handle("POST /v1/users/{name}/sessions/end", s.auth(ActUsers, s.endSessions))
}

// newToken is 256 bits from crypto/rand, hex, with a prefix that says what it is.
func newToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "flt_" + hex.EncodeToString(b), nil
}

// isAdmin: only a person holding admin manages users; the callers-file token reaches AddUser's
// bootstrap only.
func isAdmin(c Caller) bool { return c.User && c.Role == RoleAdmin }

func needAdmin(c Caller) error {
	if isAdmin(c) {
		return nil
	}
	return problem(403, "only an admin manages users")
}

func (s *Server) nameTaken(name string) bool {
	if reservedNames[name] || strings.HasPrefix(name, "worker") {
		return true
	}
	for _, c := range s.Callers {
		if c.Name == name {
			return true
		}
	}
	return false
}

// userProblem turns the store's errors into answers the caller can act on.
func userProblem(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, store.ErrNoSuchUser):
		return problem(404, "%s", err.Error())
	case errors.Is(err, store.ErrUserExists), errors.Is(err, store.ErrLastAdmin):
		return problem(409, "%s", err.Error())
	case errors.Is(err, store.ErrBadRole), strings.Contains(err.Error(), "first user must be an admin"):
		return problem(400, "%s", err.Error())
	}
	return err
}

// Users lists every user (admins only).
func (s *Server) Users(ctx context.Context, c Caller) ([]store.User, error) {
	if err := needAdmin(c); err != nil {
		return nil, err
	}
	return s.Store.Users(ctx)
}

// AddUser adds a person and returns their token, shown once. An admin adds anyone; the callers-file
// operator token adds an admin only (the first one, or break-glass).
func (s *Server) AddUser(ctx context.Context, c Caller, via, name, role string) (string, string, error) {
	bootstrap := !c.User && c.Role == RoleOperator
	if !bootstrap {
		if err := needAdmin(c); err != nil {
			return "", "", err
		}
	}
	if !userNameRe.MatchString(name) {
		return "", "", problem(400, "a name is 2 to 40 characters: a-z first, then a-z 0-9 . _ -")
	}
	if s.nameTaken(name) {
		return "", "", problem(409, "that name is reserved or a caller's")
	}
	tok, err := newToken()
	if err != nil {
		return "", "", err
	}
	if err := userProblem(s.Store.AddUser(ctx, name, role, HashToken(tok), c.Name, via, bootstrap)); err != nil {
		return "", "", err
	}
	s.Log.Info("user added", "user", name, "role", role, "by", c.Name, "bootstrap", bootstrap)
	return name, tok, nil
}

func (s *Server) SetUserRole(ctx context.Context, c Caller, via, name, role string) error {
	if err := needAdmin(c); err != nil {
		return err
	}
	return userProblem(s.Store.SetRole(ctx, name, role, c.Name, via))
}

// RotateUser gives a user a new token (returned once) and ends their sessions.
func (s *Server) RotateUser(ctx context.Context, c Caller, via, name string) (string, error) {
	if err := needAdmin(c); err != nil {
		return "", err
	}
	tok, err := newToken()
	if err != nil {
		return "", err
	}
	if err := userProblem(s.Store.RotateToken(ctx, name, HashToken(tok), c.Name, via)); err != nil {
		return "", err
	}
	return tok, nil
}

func (s *Server) RevokeUser(ctx context.Context, c Caller, via, name string) error {
	if err := needAdmin(c); err != nil {
		return err
	}
	return userProblem(s.Store.RevokeUser(ctx, name, c.Name, via))
}

// EndUserSessions ends another user's sessions (admins).
func (s *Server) EndUserSessions(ctx context.Context, c Caller, via, name string) error {
	if err := needAdmin(c); err != nil {
		return err
	}
	_, err := s.Store.EndSessions(ctx, name, c.Name, via)
	return err
}

// --- the HTTP handlers ---

func (s *Server) listUsers(w http.ResponseWriter, r *http.Request, c Caller) {
	us, err := s.Users(r.Context(), c)
	if err != nil {
		writeAction(w, err, 0, nil)
		return
	}
	writeJSON(w, 200, map[string]any{"users": us})
}

func (s *Server) addUser(w http.ResponseWriter, r *http.Request, c Caller) {
	var body struct {
		Name string `json:"name"`
		Role string `json:"role"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]string{"error": "body: {\"name\": \"...\", \"role\": \"admin|operator|viewer\"}"})
		return
	}
	name, tok, err := s.AddUser(r.Context(), c, "api", body.Name, body.Role)
	if err != nil {
		writeAction(w, err, 0, nil)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 201, map[string]string{"name": name, "role": body.Role, "token": tok,
		"note": "the token is shown once; flotilla keeps only its hash"})
}

func (s *Server) setRole(w http.ResponseWriter, r *http.Request, c Caller) {
	var body struct {
		Role string `json:"role"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]string{"error": "body: {\"role\": \"admin|operator|viewer\"}"})
		return
	}
	err := s.SetUserRole(r.Context(), c, "api", r.PathValue("name"), body.Role)
	writeAction(w, err, 200, map[string]string{"name": r.PathValue("name"), "role": body.Role, "by": c.Name})
}

func (s *Server) rotateUser(w http.ResponseWriter, r *http.Request, c Caller) {
	tok, err := s.RotateUser(r.Context(), c, "api", r.PathValue("name"))
	if err != nil {
		writeAction(w, err, 0, nil)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, map[string]string{"name": r.PathValue("name"), "token": tok,
		"note": "the token is shown once; the old one and its sessions are gone"})
}

func (s *Server) revokeUser(w http.ResponseWriter, r *http.Request, c Caller) {
	err := s.RevokeUser(r.Context(), c, "api", r.PathValue("name"))
	writeAction(w, err, 200, map[string]string{"name": r.PathValue("name"), "revoked_by": c.Name})
}

func (s *Server) endSessions(w http.ResponseWriter, r *http.Request, c Caller) {
	err := s.EndUserSessions(r.Context(), c, "api", r.PathValue("name"))
	writeAction(w, err, 200, map[string]string{"name": r.PathValue("name"), "sessions_ended_by": c.Name})
}
