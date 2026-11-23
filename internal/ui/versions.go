package ui

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/chipi/flotilla/internal/api"
	"github.com/chipi/flotilla/internal/spec"
	"github.com/chipi/flotilla/internal/store"
)

// The routine page's versions (docs/wip/v2-design.md section 1): each version of the routine with its state,
// hash, who moved it and why, and the operator's forms to promote a shadow, roll back to the previous version
// and retire a shadow. Every form goes through the API's own functions (api/versions.go), so the rules are one.

type rVersion struct {
	N                       int
	State, Chip, Label      string
	Hash, FullHash          string
	Deployed, Live, By, Why string
	DeployedAt              string // as stored, for the sort
	Pairs                   string
	HasPairs                bool // it has compared pairs: its Pairs cell links to them
	Detail                  []string
	CanPromote, CanRetire   bool
	Current                 bool // the live one: the rollback form is its
	Note                    string
}

type versionsData struct {
	Acceptance  string
	NeedsReason bool // pairs are declared and shadows record none yet: a promote cannot go without a reason
	Rows        []rVersion
	Autonomy    string // what the routine is set to now: pre-fills promote and roll back
	CanRollBack bool
	Previous    int
	Live        int
	T           tableQ
	Heads       []thCell
}

var versionSorts = map[string]bool{"version": true, "when": true}

// table sorts and pages the versions, the newest first; a promote or retire form on each row makes 10 a page. Its keys are prefixed "v".
func (d *versionsData) table(r *http.Request) {
	t := readTableAs(r, "v", "version", true, func(c string) bool { return versionSorts[c] })
	t.Anchor, t.Per = "versions", 10
	rows := append([]rVersion(nil), d.Rows...)
	less := func(a, b rVersion) bool {
		if t.Sort == "when" && a.DeployedAt != b.DeployedAt {
			return a.DeployedAt < b.DeployedAt
		}
		return a.N < b.N
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if t.Desc {
			return less(rows[j], rows[i])
		}
		return less(rows[i], rows[j])
	})
	d.Rows = window(rows, &t)
	d.T = t
	d.Heads = []thCell{t.Th("version", "Version"), t.Plain("State"), t.Plain("Hash"), t.Th("when", "Deployed"), t.Plain("Live since"), t.Plain("By and why"), t.Plain("Pairs and results")}
}

// pairsText is a version's compared pairs, linked to its list when it has any.
func pairsText(n int) string {
	if n == 0 {
		return "none yet"
	}
	return fmt.Sprintf("%d compared pairs", n)
}

var versionChip = map[string]string{store.VersionLive: "chip--ok", store.VersionShadow: "chip--neutral", store.VersionPrevious: "chip--wait", store.VersionRetired: "chip--neutral"}

func (u *Server) versionsFor(ctx context.Context, id string, st *api.RoutineStatus) (*versionsData, error) {
	v, err := u.API.RoutineVersions(ctx, id)
	if err != nil {
		return nil, err
	}
	d := &versionsData{Acceptance: strings.Join(v.Acceptance, ", "), Autonomy: st.AutonomySet, Live: v.Live}
	for _, k := range v.Acceptance {
		if k == spec.AcceptPairs {
			d.NeedsReason = true
		}
	}
	for _, x := range v.Versions {
		r := rVersion{N: x.Version, State: x.State, Chip: versionChip[x.State], FullHash: x.Hash, Hash: x.Hash, Pairs: pairsText(x.Pairs), HasPairs: x.Pairs > 0,
			Deployed: u.clock(x.DeployedAt), DeployedAt: x.DeployedAt, By: strings.TrimPrefix(x.StateBy, "user:"), Why: x.StateWhy}
		if len(r.Hash) > 7 {
			r.Hash = r.Hash[:7]
		}
		if x.LiveAt != "" {
			r.Live = u.clock(x.LiveAt)
		}
		switch x.State {
		case store.VersionShadow:
			r.Label = fmt.Sprintf("shadow: runs beside v%d when shadows are on", v.Live)
			r.CanPromote, r.CanRetire = x.Loaded && x.Error == "", true
		case store.VersionPrevious:
			d.Previous, d.CanRollBack = x.Version, true
		case store.VersionLive:
			r.Current = true
		case store.VersionRetired:
			if x.RetiredKind == store.RetiredRolledBack {
				r.Detail = append(r.Detail, fmt.Sprintf("rolled back at %s by %s: %s", u.clock(x.StateAt), r.By, x.StateWhy))
				r.Why = ""
			}
		}
		if p := x.Promotion; p != nil {
			line := "promoted with: " + strings.Join(p.Acceptance, ", ")
			for _, g := range p.Gates {
				line += " · gate " + g.Name + " " + g.Result
			}
			for _, e := range p.Evals {
				line += " · eval " + e.RunID + " " + e.Verdict
			}
			if len(p.Missing) > 0 {
				line += " · without: " + strings.Join(p.Missing, ", ")
			}
			if len(p.Failed) > 0 {
				line += " · failed: " + strings.Join(p.Failed, ", ")
			}
			r.Detail = append(r.Detail, line)
			r.Pairs = fmt.Sprintf("%d compared pairs at the promote", p.Pairs)
		}
		if !x.Loaded && x.State != store.VersionRetired {
			r.Note = "its folder is not in the fleets folder"
		} else if x.Error != "" {
			r.Note = "does not load: " + x.Error
		}
		d.Rows = append(d.Rows, r)
	}
	return d, nil
}

// splitLines is the non-empty trimmed lines of a text box.
func splitLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

// versionAction promotes or retires one version, from the routine page's forms.
func (u *Server) versionAction(w http.ResponseWriter, r *http.Request, s *store.Session, sid string) {
	id := r.PathValue("fleet") + "/" + r.PathValue("routine")
	back := safeBack(r.PostFormValue("back"), "/fleets/"+id+"#versions")
	n, err := strconv.Atoi(r.PathValue("n"))
	if err != nil || n < 1 {
		http.NotFound(w, r)
		return
	}
	reason := r.PostFormValue("reason")
	switch r.PathValue("action") {
	case "promote":
		req := api.PromoteRequest{Autonomy: r.PostFormValue("autonomy"), Reason: reason, WithoutPairs: r.PostFormValue("without_pairs") != ""}
		for _, l := range splitLines(r.PostFormValue("gates")) {
			g, perr := api.ParseGate(l)
			if perr != nil {
				done(w, r, back, &api.Problem{Code: 400, Msg: perr.Error()}, "")
				return
			}
			req.Gates = append(req.Gates, g)
		}
		for _, l := range splitLines(r.PostFormValue("evals")) {
			e, perr := api.ParseEval(l)
			if perr != nil {
				done(w, r, back, &api.Problem{Code: 400, Msg: perr.Error()}, "")
				return
			}
			req.Evals = append(req.Evals, e)
		}
		err = u.API.PromoteVersion(r.Context(), caller(s), "ui", id, n, req)
		done(w, r, back, err, fmt.Sprintf("v%d of %s is live, at %s. The version before it is the previous one: roll back to return to it.", n, id, req.Autonomy))
	case "retire":
		err = u.API.RetireVersion(r.Context(), caller(s), "ui", id, n, reason)
		done(w, r, back, err, fmt.Sprintf("Retired v%d of %s.", n, id))
	default:
		http.NotFound(w, r)
	}
}

// rollbackAction is the routine page's roll back form.
func (u *Server) rollbackAction(w http.ResponseWriter, r *http.Request, s *store.Session, id, back string) {
	res, err := u.API.RollbackVersion(r.Context(), caller(s), "ui", id, api.RollbackRequest{Autonomy: r.PostFormValue("autonomy"), Reason: r.PostFormValue("reason")})
	ok := fmt.Sprintf("Rolled back %s: v%d is live again. %s", id, res.Live, plural(len(res.Cases), "plan awaiting approval was withdrawn", "plans awaiting approval were withdrawn"))
	if len(res.Cancelled) > 0 {
		ok += " The run in flight was cancelled."
	}
	done(w, r, back, err, ok)
}
