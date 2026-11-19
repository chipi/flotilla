package ui

import (
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/chipi/flotilla/internal/store"
)

// Knowledge (mockup v3's Recall page): Memory, the notes people propose from cases and activate here,
// and Sources, what each fleet's agents read from git (its playbooks and skills, ADR-0012). How often
// a run used a note is G7: the harness does not report what it read yet.

type noteRow struct {
	store.Note
	When, Decided string
}

type sourceRow struct {
	Fleet, Source, Kind, Version string
}

type knowledgeData struct {
	Tab      string
	Tabs     []pillLink
	Proposed []noteRow
	Active   []noteRow
	Dropped  int
	Sources  []sourceRow
}

func (u *Server) knowledgePage(w http.ResponseWriter, r *http.Request, s *store.Session, sid string) {
	tab := r.URL.Query().Get("tab")
	if tab != "sources" {
		tab = "memory"
	}
	d := knowledgeData{Tab: tab}
	for _, p := range [][2]string{{"memory", "Memory"}, {"sources", "Sources"}} {
		href := "/knowledge"
		if p[0] != "memory" {
			href += "?tab=" + p[0]
		}
		d.Tabs = append(d.Tabs, pillLink{Label: p[1], Href: href, On: tab == p[0]})
	}
	if tab == "memory" {
		notes, err := u.API.Store.Notes(r.Context(), "")
		if err != nil {
			http.Error(w, "notes failed", 500)
			return
		}
		for _, n := range notes {
			row := noteRow{Note: n, When: u.clock(n.CreatedAt), Decided: u.clock(n.DecidedAt)}
			switch n.Status {
			case store.NoteProposed:
				d.Proposed = append(d.Proposed, row)
			case store.NoteActive:
				d.Active = append(d.Active, row)
			default:
				d.Dropped++
			}
		}
	} else {
		d.Sources = u.sources()
	}
	u.render(w, "knowledge.html", 200, u.pageFor(s, "Knowledge", "Knowledge", r, d))
}

// sources are the playbooks and skills under each fleet's folder, as the image carries them.
func (u *Server) sources() []sourceRow {
	if u.SpecsDir == "" {
		return nil
	}
	var out []sourceRow
	fleets, _ := os.ReadDir(u.SpecsDir)
	for _, f := range fleets {
		if !f.IsDir() {
			continue
		}
		for _, kind := range []string{"playbooks", "skills"} {
			entries, _ := os.ReadDir(filepath.Join(u.SpecsDir, f.Name(), kind))
			for _, e := range entries {
				out = append(out, sourceRow{Fleet: title(f.Name()), Source: filepath.Join("fleets", f.Name(), kind, e.Name()),
					Kind: strings.TrimSuffix(kind, "s"), Version: u.API.Version})
			}
		}
	}
	// the texts a routine's versions carry in their own folders (fleets/<fleet>/<routine>/v<n>/*.md): the bugfix rules, triage's system text
	for _, f := range fleets {
		if !f.IsDir() {
			continue
		}
		routines, _ := os.ReadDir(filepath.Join(u.SpecsDir, f.Name()))
		for _, r := range routines {
			vs, _ := os.ReadDir(filepath.Join(u.SpecsDir, f.Name(), r.Name()))
			for _, v := range vs {
				if !r.IsDir() || !v.IsDir() || !versionDirName.MatchString(v.Name()) {
					continue
				}
				files, _ := os.ReadDir(filepath.Join(u.SpecsDir, f.Name(), r.Name(), v.Name()))
				for _, e := range files {
					if ext := filepath.Ext(e.Name()); !e.IsDir() && (ext == ".md" || ext == ".txt") {
						out = append(out, sourceRow{Fleet: title(f.Name()), Source: filepath.ToSlash(filepath.Join("fleets", f.Name(), r.Name(), v.Name(), e.Name())),
							Kind: "text of " + r.Name() + " " + v.Name(), Version: u.API.Version})
					}
				}
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Source < out[j].Source })
	return out
}

var versionDirName = regexp.MustCompile(`^v[1-9][0-9]*$`)

// noteAction activates or drops a note.
func (u *Server) noteAction(w http.ResponseWriter, r *http.Request, s *store.Session, sid string) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	to := map[string]string{"activate": store.NoteActive, "drop": store.NoteDropped}[r.PathValue("action")]
	if to == "" {
		http.NotFound(w, r)
		return
	}
	done(w, r, "/knowledge", u.API.DecideNote(r.Context(), caller(s), "ui", id, to), "Note N-"+r.PathValue("id")+" is "+to+".")
}
