package api

import (
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
// an admin again when none is left (break-glass), and nothing else here.

var userNameRe = regexp.MustCompile(`^[a-z][a-z0-9._-]{1,39}$`)

// reservedNames are actors flotillad writes itself; no person may take one.
var reservedNames = map[string]bool{"flotillad": true, "operator": true, "admin": true, "system": true}

func (s *Server) userRoutes(m *http.ServeMux) {
	m.Handle("GET /v1/users", s.auth(ActUsers, s.listUsers))
	m.Handle("POST /v1/users", s.auth(ActUsers, s.addUser))
	m.Handle("POST /v1/users/{name}/role", s.auth(ActUsers, s.setRole))
	m.Handle("POST /v1/users/{name}/rotate", s.auth(ActUsers, s.rotateUser))
	m.Handle("POST /v1/users/{name}/revoke", s.auth(ActUsers, s.revokeUser))
}

// newToken is 256 bits from crypto/rand, hex, with a prefix that says what it is.
func newToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "flt_" + hex.EncodeToString(b), nil
}

// isAdmin: only a person holding admin manages users; the callers-file token reaches addUser's bootstrap only.
func isAdmin(c Caller) bool { return c.User && c.Role == RoleAdmin }

func (s *Server) refuseNonAdmin(w http.ResponseWriter, c Caller) bool {
	if isAdmin(c) {
		return false
	}
	writeJSON(w, 403, map[string]string{"error": "only an admin manages users"})
	return true
}

func (s *Server) listUsers(w http.ResponseWriter, r *http.Request, c Caller) {
	if s.refuseNonAdmin(w, c) {
		return
	}
	us, err := s.Store.Users(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"users": us})
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

func (s *Server) addUser(w http.ResponseWriter, r *http.Request, c Caller) {
	var body struct {
		Name string `json:"name"`
		Role string `json:"role"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]string{"error": "body: {\"name\": \"...\", \"role\": \"admin|operator|viewer\"}"})
		return
	}
	bootstrap := !c.User && c.Role == RoleOperator
	if !bootstrap && s.refuseNonAdmin(w, c) {
		return
	}
	if !userNameRe.MatchString(body.Name) {
		writeJSON(w, 400, map[string]string{"error": "a name is 2 to 40 characters: a-z first, then a-z 0-9 . _ -"})
		return
	}
	if s.nameTaken(body.Name) {
		writeJSON(w, 409, map[string]string{"error": "that name is reserved or a caller's"})
		return
	}
	tok, err := newToken()
	if err != nil {
		writeErr(w, err)
		return
	}
	err = s.Store.AddUser(r.Context(), body.Name, body.Role, HashToken(tok), c.Name, "api", bootstrap)
	if !s.userErr(w, err) {
		return
	}
	s.Log.Info("user added", "user", body.Name, "role", body.Role, "by", c.Name, "bootstrap", bootstrap)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 201, map[string]string{"name": body.Name, "role": body.Role, "token": tok,
		"note": "the token is shown once; flotilla keeps only its hash"})
}

func (s *Server) setRole(w http.ResponseWriter, r *http.Request, c Caller) {
	if s.refuseNonAdmin(w, c) {
		return
	}
	var body struct {
		Role string `json:"role"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]string{"error": "body: {\"role\": \"admin|operator|viewer\"}"})
		return
	}
	if !s.userErr(w, s.Store.SetRole(r.Context(), r.PathValue("name"), body.Role, c.Name, "api")) {
		return
	}
	writeJSON(w, 200, map[string]string{"name": r.PathValue("name"), "role": body.Role, "by": c.Name})
}

func (s *Server) rotateUser(w http.ResponseWriter, r *http.Request, c Caller) {
	if s.refuseNonAdmin(w, c) {
		return
	}
	tok, err := newToken()
	if err != nil {
		writeErr(w, err)
		return
	}
	if !s.userErr(w, s.Store.RotateToken(r.Context(), r.PathValue("name"), HashToken(tok), c.Name, "api")) {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, map[string]string{"name": r.PathValue("name"), "token": tok,
		"note": "the token is shown once; the old one and its sessions are gone"})
}

func (s *Server) revokeUser(w http.ResponseWriter, r *http.Request, c Caller) {
	if s.refuseNonAdmin(w, c) {
		return
	}
	if !s.userErr(w, s.Store.RevokeUser(r.Context(), r.PathValue("name"), c.Name, "api")) {
		return
	}
	writeJSON(w, 200, map[string]string{"name": r.PathValue("name"), "revoked_by": c.Name})
}

// userErr writes the store's error as an HTTP answer; true when there was none.
func (s *Server) userErr(w http.ResponseWriter, err error) bool {
	switch {
	case err == nil:
		return true
	case errors.Is(err, store.ErrNoSuchUser):
		writeJSON(w, 404, map[string]string{"error": err.Error()})
	case errors.Is(err, store.ErrUserExists):
		writeJSON(w, 409, map[string]string{"error": err.Error()})
	case errors.Is(err, store.ErrLastAdmin):
		writeJSON(w, 409, map[string]string{"error": err.Error()})
	case errors.Is(err, store.ErrBadRole):
		writeJSON(w, 400, map[string]string{"error": err.Error()})
	default:
		if strings.Contains(err.Error(), "first user must be an admin") {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
		} else {
			writeErr(w, err)
		}
	}
	return false
}
