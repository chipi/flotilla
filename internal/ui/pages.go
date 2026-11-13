package ui

import (
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/chipi/flotilla/internal/api"
	"github.com/chipi/flotilla/internal/store"
)

// The pages of slice 1 (ADR-0014): every action goes through the api.Server functions the API uses,
// with the signed-in person as the caller, so the table and every rule apply the same way.

type navPage struct {
	Label, Href string
	Current     bool
}

type navGroup struct {
	Label, Href string
	Current     bool
	Pages       []navPage
}

// nav is the four groups (DESIGN.md: Decide, Watch, Govern, Record) with the pages that exist today.
func nav(active, role string) []navGroup {
	groups := []navGroup{
		{Label: "Decide", Pages: []navPage{{Label: "Now", Href: "/"}}},
		{Label: "Watch", Pages: []navPage{{Label: "Runs", Href: "/runs"}, {Label: "Fleets", Href: "/fleets"}}},
		{Label: "Govern", Pages: []navPage{{Label: "Autonomy", Href: "/autonomy"}}},
		{Label: "Record", Pages: []navPage{{Label: "History", Href: "/history"}}},
	}
	if role == api.RoleAdmin {
		groups[2].Pages = append(groups[2].Pages, navPage{Label: "Users", Href: "/users"})
	}
	for i := range groups {
		groups[i].Href = groups[i].Pages[0].Href
		for j := range groups[i].Pages {
			if groups[i].Pages[j].Label == active {
				groups[i].Pages[j].Current, groups[i].Current = true, true
			}
		}
	}
	return groups
}

func caller(s *store.Session) api.Caller {
	return api.Caller{Name: "user:" + s.User, Role: s.Role, User: true}
}

func (u *Server) pageFor(s *store.Session, title, active string, r *http.Request, data any) page {
	p := page{Title: title, User: s.User, Role: s.Role, CSRF: s.CSRF, Active: active, Nav: nav(active, s.Role),
		CanAct: s.Role == api.RoleOperator || s.Role == api.RoleAdmin, IsAdmin: s.Role == api.RoleAdmin,
		Msg: r.URL.Query().Get("msg"), Error: r.URL.Query().Get("err"), Data: data}
	fr, err := u.API.FreezeState(r.Context())
	if err != nil {
		u.Log.Error("reading the freeze", "err", err)
		p.Error = "flotillad could not read whether the freeze holds; check `flotilla status`"
	}
	p.Freeze = fr
	return p
}

// done redirects back with the action's outcome, as a short message on the page.
func done(w http.ResponseWriter, r *http.Request, back string, err error, ok string) {
	q := url.Values{}
	if err != nil {
		if p := api.AsProblem(err); p != nil {
			q.Set("err", p.Msg)
		} else {
			q.Set("err", "that did not work: "+err.Error())
		}
	} else {
		q.Set("msg", ok)
	}
	http.Redirect(w, r, back+"?"+q.Encode(), http.StatusSeeOther)
}

// safeBack keeps redirects on this site's own pages.
func safeBack(v, fallback string) string {
	if strings.HasPrefix(v, "/") && !strings.HasPrefix(v, "//") && !strings.ContainsAny(v, "\\\r\n") {
		return v
	}
	return fallback
}

type nowData struct {
	Wrong     []api.RoutineStatus
	Fine      int
	Failed    []store.Run
	Workers   map[string]any
	Unhealthy []string
}

func (u *Server) now(w http.ResponseWriter, r *http.Request, s *store.Session, sid string) {
	routines, workers, err := u.API.Status(r.Context())
	if err != nil {
		http.Error(w, "status failed", 500)
		return
	}
	d := nowData{Workers: workers}
	for _, x := range routines {
		if x.Enabled && (x.Stale || x.Paused || x.Stopped) {
			d.Wrong = append(d.Wrong, x)
		} else if x.Enabled {
			d.Fine++
		}
	}
	for name, h := range workers {
		if m, ok := h.(map[string]any); ok && m["ok"] != true {
			d.Unhealthy = append(d.Unhealthy, name)
		}
	}
	sort.Strings(d.Unhealthy)
	runs, _ := u.API.Store.Runs(r.Context(), "", 200)
	cut := u.API.Sched.Now().Add(-24 * time.Hour).Format(time.RFC3339)
	for _, x := range runs {
		if x.StartedAt >= cut && (x.Status == store.StatusError || x.Status == store.StatusTimeout || x.Status == store.StatusInterrupted) {
			d.Failed = append(d.Failed, x)
		}
	}
	u.render(w, "now.html", 200, u.pageFor(s, "Now", "Now", r, d))
}

func (u *Server) runsPage(w http.ResponseWriter, r *http.Request, s *store.Session, sid string) {
	routine := r.URL.Query().Get("routine")
	runs, err := u.API.Store.Runs(r.Context(), routine, 100)
	if err != nil {
		http.Error(w, "runs failed", 500)
		return
	}
	u.render(w, "runs.html", 200, u.pageFor(s, "Runs", "Runs", r, map[string]any{"Runs": runs, "Routine": routine}))
}

func (u *Server) runPage(w http.ResponseWriter, r *http.Request, s *store.Session, sid string) {
	run, err := u.API.Store.Run(r.Context(), r.PathValue("id"))
	if err != nil {
		http.Error(w, "run failed", 500)
		return
	}
	if run == nil {
		http.NotFound(w, r)
		return
	}
	u.render(w, "run.html", 200, u.pageFor(s, "Run "+run.ID, "Runs", r, run))
}

type fleetView struct {
	Name     string
	Stopped  bool
	Routines []api.RoutineStatus
}

func (u *Server) fleets(r *http.Request) ([]fleetView, error) {
	routines, _, err := u.API.Status(r.Context())
	if err != nil {
		return nil, err
	}
	by := map[string]*fleetView{}
	var order []string
	for _, x := range routines {
		f := by[x.Fleet]
		if f == nil {
			f = &fleetView{Name: x.Fleet, Stopped: x.Stopped}
			by[x.Fleet] = f
			order = append(order, x.Fleet)
		}
		f.Routines = append(f.Routines, x)
	}
	sort.Strings(order)
	var out []fleetView
	for _, n := range order {
		out = append(out, *by[n])
	}
	return out, nil
}

func (u *Server) fleetsPage(w http.ResponseWriter, r *http.Request, s *store.Session, sid string) {
	fs, err := u.fleets(r)
	if err != nil {
		http.Error(w, "status failed", 500)
		return
	}
	u.render(w, "fleets.html", 200, u.pageFor(s, "Fleets", "Fleets", r, fs))
}

func (u *Server) fleetPage(w http.ResponseWriter, r *http.Request, s *store.Session, sid string) {
	fs, err := u.fleets(r)
	if err != nil {
		http.Error(w, "status failed", 500)
		return
	}
	for _, f := range fs {
		if f.Name == r.PathValue("fleet") {
			u.render(w, "fleet.html", 200, u.pageFor(s, "Fleet "+f.Name, "Fleets", r, f))
			return
		}
	}
	http.NotFound(w, r)
}

func (u *Server) autonomyPage(w http.ResponseWriter, r *http.Request, s *store.Session, sid string) {
	fs, err := u.fleets(r)
	if err != nil {
		http.Error(w, "status failed", 500)
		return
	}
	u.render(w, "autonomy.html", 200, u.pageFor(s, "Autonomy", "Autonomy", r, fs))
}

// routineAction is run, pause, resume or autonomy on a routine, from a form.
func (u *Server) routineAction(w http.ResponseWriter, r *http.Request, s *store.Session, sid string) {
	id := r.PathValue("fleet") + "/" + r.PathValue("routine")
	back := safeBack(r.PostFormValue("back"), "/fleets/"+r.PathValue("fleet"))
	reason := r.PostFormValue("reason")
	var err error
	var ok string
	switch r.PathValue("action") {
	case "run":
		err, ok = u.API.RunNow(r.Context(), caller(s), "ui", id), "Started "+id+"."
	case "pause":
		err, ok = u.API.SetPaused(r.Context(), caller(s), "ui", id, true, ""), "Paused "+id+"."
	case "resume":
		err, ok = u.API.SetPaused(r.Context(), caller(s), "ui", id, false, reason), "Resumed "+id+"."
	case "autonomy":
		level := r.PostFormValue("autonomy")
		err, ok = u.API.SetAutonomy(r.Context(), caller(s), "ui", id, level, reason), id+" is now at "+level+"."
	default:
		http.NotFound(w, r)
		return
	}
	done(w, r, back, err, ok)
}

func (u *Server) fleetAction(w http.ResponseWriter, r *http.Request, s *store.Session, sid string) {
	f := r.PathValue("fleet")
	back := safeBack(r.PostFormValue("back"), "/fleets/"+f)
	var err error
	var ok string
	switch r.PathValue("action") {
	case "stop":
		err, ok = u.API.SetStopped(r.Context(), caller(s), "ui", f, true, ""), "STOPped fleet "+f+"."
	case "start":
		err, ok = u.API.SetStopped(r.Context(), caller(s), "ui", f, false, r.PostFormValue("reason")), "Started fleet "+f+"."
	default:
		http.NotFound(w, r)
		return
	}
	done(w, r, back, err, ok)
}

// freezeAction pulls the freeze, or lifts it with a reason; both from the Now page.
func (u *Server) freezeAction(frozen bool) func(http.ResponseWriter, *http.Request, *store.Session, string) {
	return func(w http.ResponseWriter, r *http.Request, s *store.Session, sid string) {
		ok := "Frozen: every routine runs at shadow until the freeze is lifted."
		if !frozen {
			ok = "The freeze is lifted: each routine runs at its own autonomy again."
		}
		done(w, r, "/", u.API.SetFrozen(r.Context(), caller(s), "ui", frozen, r.PostFormValue("reason")), ok)
	}
}

type historyData struct {
	Namespaces []string
	NS         string
	Decisions  []store.Decision
	Older      int64
}

func (u *Server) historyPage(w http.ResponseWriter, r *http.Request, s *store.Session, sid string) {
	if err := u.API.Check(r.Context(), caller(s), api.ActLedgerRead); err != nil {
		http.Error(w, "your role may not read the ledger", http.StatusForbidden)
		return
	}
	nss, err := u.API.Store.Namespaces(r.Context())
	if err != nil {
		http.Error(w, "history failed", 500)
		return
	}
	d := historyData{Namespaces: nss, NS: r.URL.Query().Get("ns")}
	if d.NS == "" && len(nss) > 0 {
		d.NS = nss[0]
		for _, n := range nss {
			if n == "triage/cycle" {
				d.NS = n
			}
		}
	}
	before, _ := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
	if d.NS != "" {
		if d.Decisions, err = u.API.Store.RecentDecisions(r.Context(), d.NS, before, 50); err != nil {
			http.Error(w, "history failed", 500)
			return
		}
		if len(d.Decisions) == 50 {
			d.Older = d.Decisions[49].Seq
		}
	}
	u.render(w, "history.html", 200, u.pageFor(s, "History", "History", r, d))
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
	u.render(w, "users.html", 200, u.pageFor(s, "Users", "Users", r, us))
}

// showToken renders a new token once, never cached, never in a URL.
func (u *Server) showToken(w http.ResponseWriter, r *http.Request, s *store.Session, name, tok string) {
	u.render(w, "token.html", 200, u.pageFor(s, "New token for "+name, "Users", r, map[string]string{"Name": name, "Token": tok}))
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
