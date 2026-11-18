package ui

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/chipi/flotilla/internal/store"
)

// The decision mix (the installation's research roadmap): what a fleet decided per UTC day, counted the way
// the fleet session's projects/triage-ops/decision_mix.py counts a dispositions.tsv, so the two agree
// for a day: live stages only (propose, live; shadow left out), the four main dispositions and
// "other", recurrences apart, a gate fired when its outcome is neither "passed" nor "n/a" (the
// "operational" label is not a gate), and the project as the GlitchTip short id's prefix or "grafana".

var mixLive = map[string]bool{"propose": true, "live": true}

var mixDispositions = []string{"file", "escalate", "dismiss", "cleanup", "other"}

func mixLabel(d string) string {
	switch d {
	case "file", "escalate", "dismiss", "cleanup":
		return d
	}
	return "other"
}

func mixRecurrence(d store.LedgerDecision) bool {
	return d.Kind == "recurrence" || d.Disposition == "recurrence"
}

var gateStart = regexp.MustCompile(`^\w+:`)

// mixGates splits "i:passed/c:n/a/d:downgraded (no corroboration)/e:n/a" into letter -> outcome; an
// outcome may hold "/", so a part that does not start a new "letter:" belongs to the one before.
func mixGates(s string) [][2]string {
	var out [][2]string
	for _, part := range strings.Split(s, "/") {
		if gateStart.MatchString(part) {
			k, v, _ := strings.Cut(part, ":")
			out = append(out, [2]string{k, v})
		} else if len(out) > 0 {
			out[len(out)-1][1] += "/" + part
		}
	}
	return out
}

func mixProject(fp string) string {
	src, id, _ := strings.Cut(fp, ":")
	switch src {
	case "grafana":
		return "grafana"
	case "glitchtip":
		if i := strings.LastIndex(id, "-"); i > 0 {
			return id[:i]
		}
		return "unknown"
	}
	return "other"
}

type mixSeg struct {
	Label string
	N     int
	Pct   float64
	X     float64 // where it starts on the day's bar, in the same units
}

type mixDay struct {
	Day        string
	Total, Rec int
	Segs       []mixSeg // in mixDispositions order, zero counts left out
	Gates      string   // "d 3 · e 1", or ""
	Changes    []string // a prompt or model change on this day
}

type mixProj struct {
	Name  string
	Total int
	Line  string
}

type decisionMix struct {
	Days     []mixDay // newest first
	Projects []mixProj
	Total    int
	From, To string
	Shadow   int // shadow-stage decisions in the window, not counted
	HasAny   bool
}

// buildMix counts ds over the days given (UTC dates, oldest first). Changes of prompt_sha or model
// are found over every live decision in ds in time order, before the window too, so the first day
// of the window shows a change only when it really happened that day.
func buildMix(ds []store.LedgerDecision, days []string) decisionMix {
	m := decisionMix{}
	if len(days) == 0 {
		return m
	}
	m.From, m.To = days[0], days[len(days)-1]
	idx := map[string]int{}
	byDay := make([]map[string]int, len(days))
	gates := make([]map[string]int, len(days))
	changes := make([][]string, len(days))
	rec := make([]int, len(days))
	for i, d := range days {
		idx[d], byDay[i], gates[i] = i, map[string]int{}, map[string]int{}
	}
	proj := map[string]map[string]int{}
	var lastPrompt, lastModel string
	for _, d := range ds {
		day := d.TS
		if len(day) > 10 {
			day = day[:10]
		}
		i, in := idx[day]
		if !mixLive[d.Stage] {
			if in && d.Stage == "shadow" && !mixRecurrence(d) {
				m.Shadow++
			}
			continue
		}
		if mixRecurrence(d) {
			if in {
				rec[i]++
			}
			continue
		}
		if d.PromptSHA != "" && lastPrompt != "" && d.PromptSHA != lastPrompt && in {
			changes[i] = append(changes[i], fmt.Sprintf("%s %s UTC: prompt %s → %s", day, hhmm(d.TS), short7(lastPrompt), short7(d.PromptSHA)))
		}
		if d.Model != "" && lastModel != "" && d.Model != lastModel && in {
			changes[i] = append(changes[i], fmt.Sprintf("%s %s UTC: model %s → %s", day, hhmm(d.TS), lastModel, d.Model))
		}
		if d.PromptSHA != "" {
			lastPrompt = d.PromptSHA
		}
		if d.Model != "" {
			lastModel = d.Model
		}
		if !in {
			continue
		}
		label := mixLabel(d.Disposition)
		byDay[i][label]++
		m.Total++
		for _, g := range mixGates(d.Gates) {
			if g[0] != "operational" && g[1] != "passed" && g[1] != "n/a" {
				gates[i][g[0]]++
			}
		}
		p := mixProject(d.Fingerprint)
		if proj[p] == nil {
			proj[p] = map[string]int{}
		}
		proj[p][label]++
	}
	max := 1
	for i := range days {
		t := 0
		for _, n := range byDay[i] {
			t += n
		}
		if t > max {
			max = t
		}
	}
	for i := len(days) - 1; i >= 0; i-- {
		day, x := mixDay{Day: days[i], Rec: rec[i], Changes: changes[i]}, 0.0
		for _, l := range mixDispositions {
			if n := byDay[i][l]; n > 0 {
				day.Total += n
				pct := 100 * float64(n) / float64(max)
				day.Segs = append(day.Segs, mixSeg{Label: l, N: n, Pct: pct, X: x})
				x += pct
			}
		}
		var gs []string
		for _, k := range sortedKeys(gates[i]) {
			gs = append(gs, fmt.Sprintf("%s %d", k, gates[i][k]))
		}
		day.Gates = strings.Join(gs, " · ")
		m.Days = append(m.Days, day)
	}
	for name, c := range proj {
		p := mixProj{Name: name}
		var parts []string
		for _, l := range mixDispositions {
			if c[l] > 0 {
				p.Total += c[l]
				parts = append(parts, fmt.Sprintf("%s %d", l, c[l]))
			}
		}
		p.Line = strings.Join(parts, " · ")
		m.Projects = append(m.Projects, p)
	}
	sort.Slice(m.Projects, func(i, j int) bool {
		if m.Projects[i].Total != m.Projects[j].Total {
			return m.Projects[i].Total > m.Projects[j].Total
		}
		return m.Projects[i].Name < m.Projects[j].Name
	})
	m.HasAny = m.Total > 0 || m.Shadow > 0
	return m
}

// decisionNamespaces are the ledger namespaces (routine ids) of a fleet that hold a decision.
func (u *Server) decisionNamespaces(ctx context.Context, f fleetSum) ([]string, error) {
	var out []string
	for _, x := range f.Routines {
		c, err := u.API.Store.LedgerCounts(ctx, x.ID)
		if err != nil {
			return nil, err
		}
		if c.Decisions > 0 {
			out = append(out, x.ID)
		}
	}
	return out, nil
}

// decisionMix reads the last n UTC days of the namespaces' decisions, with a week before them so a
// prompt or model change on the first day is told from one that happened earlier.
func (u *Server) decisionMix(ctx context.Context, nss []string, now time.Time, n int) (decisionMix, error) {
	ds := days(now, n)
	from := now.UTC().AddDate(0, 0, -n-6).Format("2006-01-02")
	rows, err := u.API.Store.DecisionsSince(ctx, nss, from)
	if err != nil {
		return decisionMix{}, err
	}
	return buildMix(rows, ds), nil
}

// decisionItems are the brief's Decisions lines: per fleet with decisions, the last full UTC day
// against its 14-day daily average, linked to the fleet's Decisions tab.
func (u *Server) decisionItems(ctx context.Context, fs []fleetSum, now time.Time) ([]store.BriefItem, error) {
	var out []store.BriefItem
	for _, f := range fs {
		nss, err := u.decisionNamespaces(ctx, f)
		if err != nil {
			return nil, err
		}
		if len(nss) == 0 {
			continue
		}
		m, err := u.decisionMix(ctx, nss, now.AddDate(0, 0, -1), 14)
		if err != nil {
			return nil, err
		}
		if len(m.Days) == 0 {
			continue
		}
		y := m.Days[0]
		var parts []string
		for _, sg := range y.Segs {
			parts = append(parts, fmt.Sprintf("%s %d", sg.Label, sg.N))
		}
		text := fmt.Sprintf("%s decided %d on %s", f.Name, y.Total, y.Day)
		if len(parts) > 0 {
			text += " (" + strings.Join(parts, ", ") + ")"
		}
		text += fmt.Sprintf(", against %.0f a day over %d days", float64(m.Total)/float64(len(m.Days)), len(m.Days))
		if y.Rec > 0 {
			text += fmt.Sprintf("; %s", plural(y.Rec, "recurrence", "recurrences"))
		}
		if len(y.Changes) > 0 {
			text += "; " + strings.Join(y.Changes, "; ")
		}
		out = append(out, store.BriefItem{Text: text + ".", Href: "/fleets/" + f.ID + "?tab=decisions", Kind: "decisions:" + f.ID})
	}
	return out, nil
}

func short7(s string) string {
	if len(s) > 7 {
		return s[:7]
	}
	return s
}

func hhmm(ts string) string {
	if len(ts) >= 16 {
		return ts[11:16]
	}
	return ts
}
