package ui

import (
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/chipi/flotilla/internal/api"
	"github.com/chipi/flotilla/internal/store"
)

// The UI listener (ADR-0014): pages for people with a session, and /v1 for people's bearer tokens.
// It refuses the ledger, callers' and workers' tokens, a session cookie on /v1, and any other Host.

//go:embed templates
var templatesFS embed.FS

const cookieName = "__Host-flotilla"

type Server struct {
	API  *api.Server
	Host string // the one Host the proxy forwards (flotilla.<tailnet>); "" accepts any, for tests
	Log  *slog.Logger

	tmpl     *template.Template
	once     sync.Once
	mu       sync.Mutex
	failures []time.Time // sign-in failures, for the global limit
}

// Sign-in failures allowed per window before sign-in answers 429 for everyone (every request arrives
// from the proxy's address, so there is no per-client key; the tokens' 256 bits are the protection).
const (
	failLimit  = 20
	failWindow = 10 * time.Minute
)

func (u *Server) init() {
	u.once.Do(func() {
		u.tmpl = template.Must(template.New("").Funcs(funcs).ParseFS(templatesFS, "templates/*.html"))
	})
}

func (u *Server) Handler() http.Handler {
	u.init()
	m := http.NewServeMux()
	static, _ := fs.Sub(Static, "static")
	m.Handle("GET /static/", http.StripPrefix("/static/", cacheFor(http.FileServer(http.FS(static)))))
	m.HandleFunc("GET /signin", u.signinPage)
	m.HandleFunc("POST /signin", u.signin)
	m.Handle("POST /signout", u.session(u.signout))
	m.Handle("POST /signout-everywhere", u.session(u.signoutEverywhere))
	m.Handle("GET /{$}", u.session(u.now))
	m.Handle("GET /cases", u.session(u.casesPage))
	m.Handle("GET /cases/{id}", u.session(u.casePage))
	m.Handle("GET /runs", u.session(u.runsPage))
	m.Handle("GET /runs/{id}", u.session(u.runPage))
	m.Handle("GET /fleets", u.session(u.fleetsPage))
	m.Handle("GET /fleets/{fleet}", u.session(u.fleetPage))
	m.Handle("POST /routines/{fleet}/{routine}/{action}", u.session(u.routineAction))
	m.Handle("POST /fleets/{fleet}/{action}", u.session(u.fleetAction))
	m.Handle("POST /freeze", u.session(u.freezeAction(true)))
	m.Handle("POST /unfreeze", u.session(u.freezeAction(false)))
	m.Handle("GET /autonomy", u.session(u.autonomyPage))
	m.Handle("GET /history", u.session(u.historyPage))
	m.Handle("GET /users", u.session(u.usersPage))
	m.Handle("POST /users", u.session(u.userAdd))
	m.Handle("POST /users/{name}/{action}", u.session(u.userAction))
	m.Handle("/v1/", u.v1(u.API.Handler()))
	return u.hardened(m)
}

// hardened checks the Host and sets the security headers on every answer.
func (u *Server) hardened(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u.Host != "" && r.Host != u.Host {
			http.Error(w, "unknown host", http.StatusMisdirectedRequest)
			return
		}
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; frame-ancestors 'none'; form-action 'self'; base-uri 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

func cacheFor(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=3600")
		next.ServeHTTP(w, r)
	})
}

// v1 lets only a person's bearer token through to the API, never the ledger, never with the cookie.
func (u *Server) v1(apiHandler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v1/ledger") {
			http.NotFound(w, r)
			return
		}
		if _, err := r.Cookie(cookieName); err == nil {
			http.Error(w, `{"error":"the API takes a bearer token, not the UI's cookie"}`, http.StatusBadRequest)
			return
		}
		tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if ok && tok != "" {
			c, err := u.API.WhoIs(r.Context(), tok)
			if err != nil {
				http.Error(w, `{"error":"lookup failed"}`, http.StatusInternalServerError)
				return
			}
			if c != nil && !c.User {
				http.Error(w, `{"error":"on this listener the API takes a person's token only"}`, http.StatusForbidden)
				return
			}
		}
		apiHandler.ServeHTTP(w, r)
	})
}

type page struct {
	Title   string
	User    string
	Role    string
	CSRF    string
	Active  string // the page's label in the navigation
	Nav     []navGroup
	CanAct  bool // operator or admin: show the controls (the server checks every action anyway)
	IsAdmin bool
	Freeze  api.Freeze // shown on every page while it holds
	Msg     string     // an action's outcome, from the redirect after it
	Error   string
	Data    any
}

func (u *Server) render(w http.ResponseWriter, name string, code int, p page) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	if err := u.tmpl.ExecuteTemplate(w, name, p); err != nil {
		u.Log.Error("render", "template", name, "err", err)
	}
}

func randomHex() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func (u *Server) signinPage(w http.ResponseWriter, r *http.Request) {
	u.render(w, "signin.html", 200, page{Title: "Sign in"})
}

func (u *Server) limited() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	cut := time.Now().Add(-failWindow)
	keep := u.failures[:0]
	for _, t := range u.failures {
		if t.After(cut) {
			keep = append(keep, t)
		}
	}
	u.failures = keep
	return len(u.failures) >= failLimit
}

func (u *Server) failed(r *http.Request, why string) {
	u.mu.Lock()
	u.failures = append(u.failures, time.Now())
	u.mu.Unlock()
	_ = u.API.Store.Event(r.Context(), "unknown", "ui", "user.sign-in-failed", "", why)
}

// sameOrigin: a sign-in POST has no session yet, so it must come from this site's own page.
func (u *Server) sameOrigin(r *http.Request) bool {
	o := r.Header.Get("Origin")
	return o == "" || o == "https://"+r.Host || (u.Host == "" && o == "http://"+r.Host)
}

func (u *Server) signin(w http.ResponseWriter, r *http.Request) {
	if !u.sameOrigin(r) {
		http.Error(w, "cross-site sign-in refused", http.StatusForbidden)
		return
	}
	if u.limited() {
		u.render(w, "signin.html", http.StatusTooManyRequests, page{Title: "Sign in", Error: "Too many failed sign-ins. Try again in a few minutes."})
		return
	}
	tok := strings.TrimSpace(r.PostFormValue("token"))
	c, err := u.API.WhoIs(r.Context(), tok)
	if err != nil {
		http.Error(w, "lookup failed", http.StatusInternalServerError)
		return
	}
	if tok == "" || c == nil || !c.User {
		u.failed(r, "an unknown token, or not a person's")
		u.render(w, "signin.html", http.StatusUnauthorized, page{Title: "Sign in", Error: "That token is not a flotilla user's."})
		return
	}
	sid, err1 := randomHex()
	csrf, err2 := randomHex()
	if err1 != nil || err2 != nil {
		http.Error(w, "no randomness", http.StatusInternalServerError)
		return
	}
	name := strings.TrimPrefix(c.Name, "user:")
	if err := u.API.Store.StartSession(r.Context(), api.HashToken(sid), name, csrf); err != nil {
		http.Error(w, "could not start a session", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: sid, Path: "/", HttpOnly: true, Secure: true,
		SameSite: http.SameSiteStrictMode, MaxAge: int(store.SessionMaxAge.Seconds())})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

type sessionHandler func(w http.ResponseWriter, r *http.Request, s *store.Session, sid string)

// session requires a live session; a POST also needs the session's CSRF token.
func (u *Server) session(h sessionHandler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ck, err := r.Cookie(cookieName)
		if err != nil || ck.Value == "" {
			http.Redirect(w, r, "/signin", http.StatusSeeOther)
			return
		}
		sid := api.HashToken(ck.Value)
		s, err := u.API.Store.SessionByID(r.Context(), sid)
		if err != nil {
			http.Error(w, "session lookup failed", http.StatusInternalServerError)
			return
		}
		if s == nil {
			http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode, MaxAge: -1})
			http.Redirect(w, r, "/signin", http.StatusSeeOther)
			return
		}
		if r.Method == http.MethodPost && subtle.ConstantTimeCompare([]byte(r.PostFormValue("csrf")), []byte(s.CSRF)) != 1 {
			http.Error(w, "the form is stale or not this site's; reload the page", http.StatusForbidden)
			return
		}
		h(w, r, s, sid)
	})
}

func (u *Server) signout(w http.ResponseWriter, r *http.Request, s *store.Session, sid string) {
	_ = u.API.Store.EndSession(r.Context(), sid)
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode, MaxAge: -1})
	http.Redirect(w, r, "/signin", http.StatusSeeOther)
}

func (u *Server) signoutEverywhere(w http.ResponseWriter, r *http.Request, s *store.Session, sid string) {
	_, _ = u.API.Store.EndSessions(r.Context(), s.User, "user:"+s.User, "ui")
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode, MaxAge: -1})
	http.Redirect(w, r, "/signin", http.StatusSeeOther)
}
