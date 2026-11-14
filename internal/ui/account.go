package ui

import (
	"net/http"

	"github.com/chipi/flotilla/internal/api"
	"github.com/chipi/flotilla/internal/store"
)

// The account page sits behind the avatar (mockup v3 draws the avatar, not what it opens): signing out,
// and Users for an admin.

func (u *Server) accountPage(w http.ResponseWriter, r *http.Request, s *store.Session, sid string) {
	u.render(w, "account.html", 200, u.pageFor(s, "Your account", "", r, nil))
}

// notBuilt is a page the mockup draws whose data flotilla does not have yet (G11).
func (u *Server) notBuilt(title, needs string) func(http.ResponseWriter, *http.Request, *store.Session, string) {
	return func(w http.ResponseWriter, r *http.Request, s *store.Session, sid string) {
		u.render(w, "notbuilt.html", 200, u.pageFor(s, title, title, r, map[string]string{"Title": title, "Needs": needs}))
	}
}

// --- users (admin) ---

func (u *Server) usersPage(w http.ResponseWriter, r *http.Request, s *store.Session, sid string) {
	if err := u.API.Check(r.Context(), caller(s), api.ActUsers); err != nil {
		http.Error(w, "only an admin manages users", http.StatusForbidden)
		return
	}
	us, err := u.API.Store.Users(r.Context())
	if err != nil {
		http.Error(w, "users failed", 500)
		return
	}
	admins := 0
	for _, x := range us {
		if x.Role == api.RoleAdmin && x.RevokedAt == nil {
			admins++
		}
	}
	u.render(w, "users.html", 200, u.pageFor(s, "Users", "", r, map[string]any{"Users": us, "Admins": admins}))
}

// showToken renders a new token once, never cached, never in a URL.
func (u *Server) showToken(w http.ResponseWriter, r *http.Request, s *store.Session, name, tok string) {
	u.render(w, "token.html", 200, u.pageFor(s, "New token for "+name, "", r, map[string]string{"Name": name, "Token": tok}))
}

func (u *Server) userAdd(w http.ResponseWriter, r *http.Request, s *store.Session, sid string) {
	name, tok, err := u.API.AddUser(r.Context(), caller(s), "ui", r.PostFormValue("name"), r.PostFormValue("role"))
	if err != nil {
		done(w, r, "/users", err, "")
		return
	}
	u.showToken(w, r, s, name, tok)
}

func (u *Server) userAction(w http.ResponseWriter, r *http.Request, s *store.Session, sid string) {
	name := r.PathValue("name")
	switch r.PathValue("action") {
	case "role":
		role := r.PostFormValue("role")
		done(w, r, "/users", u.API.SetUserRole(r.Context(), caller(s), "ui", name, role), name+" is now "+role+".")
	case "rotate":
		tok, err := u.API.RotateUser(r.Context(), caller(s), "ui", name)
		if err != nil {
			done(w, r, "/users", err, "")
			return
		}
		u.showToken(w, r, s, name, tok)
	case "revoke":
		done(w, r, "/users", u.API.RevokeUser(r.Context(), caller(s), "ui", name), name+" is revoked; their sessions ended.")
	case "sessions":
		done(w, r, "/users", u.API.EndUserSessions(r.Context(), caller(s), "ui", name), name+"'s sessions ended.")
	default:
		http.NotFound(w, r)
	}
}
