package ui

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/chipi/flotilla/internal/api"
	"github.com/chipi/flotilla/internal/spec"
	"github.com/chipi/flotilla/internal/store"
)

// Earned trust (the operator, 2026-11-23: "make it super easy" to see that an action earned its next level and raise it).
// One reading of each action's record serves Autonomy (ready rows first, the reason written for you), Now and the brief
// (a line when an action is ready), so the three never disagree.

type trust struct {
	api.ActionStatus
	Next   string // the level a promotion gives, "" at its highest
	Streak store.Streak
	Ready  bool   // the record is enough for Next
	Cap    string // what holds Next back outside the action itself, "" when nothing
	Also   string // a routine that has to be promoted with it for Next to take effect, ""
	Reason string // the why a promotion is recorded with, written from the record
}

// actionTrust reads every action's record, its caps and the routines that plan it.
func (u *Server) actionTrust(ctx context.Context, routines []api.RoutineStatus) ([]trust, error) {
	as, err := u.API.Actions(ctx)
	if err != nil {
		return nil, err
	}
	answers, err := u.API.Store.AnswersByAction(ctx)
	if err != nil {
		return nil, err
	}
	planners, err := u.API.Store.ActionPlanners(ctx)
	if err != nil {
		return nil, err
	}
	byID := map[string]api.RoutineStatus{}
	for _, r := range routines {
		byID[r.ID] = r
	}
	var out []trust
	for _, a := range as {
		t := trust{ActionStatus: a}
		if spec.Rank(a.Autonomy) < spec.Rank(a.Max) {
			t.Next = levelUp[a.Autonomy]
		}
		if t.Streak, err = u.API.Store.PromotionStreak(ctx, a.Name); err != nil {
			return nil, err
		}
		if t.Next == "" {
			out = append(out, t)
			continue
		}
		t.Ready = t.Streak.Ready()
		t.Cap, t.Also = u.trustCaps(a, t.Next, planners[a.Name], byID)
		t.Reason = trustReason(a, t.Streak, answers[a.Name])
		out = append(out, t)
	}
	return out, nil
}

// trustCaps is what would keep a promotion to next from taking effect: policy acts at the lowest of the routine's level,
// the action's and the executor's cap, so an action promoted past them changes nothing.
func (u *Server) trustCaps(a api.ActionStatus, next string, planners []string, routines map[string]api.RoutineStatus) (string, string) {
	if ra, ok := registryAction(u, a.Name); ok && spec.Rank(next) > spec.Rank(api.HostExecutorCeiling) {
		if ra.ByRoutine() && spec.Rank(next) > spec.Rank(u.API.RoutineActionCeiling()) {
			return "routine actions are capped at " + u.API.RoutineActionCeiling() + " (flotillad.yaml, routine_actions.max_autonomy)", ""
		}
		if !ra.ByRoutine() {
			return "a host agent runs at most " + api.HostExecutorCeiling + " (the host executor's cap)", ""
		}
	}
	var below []string
	for _, id := range planners {
		r, ok := routines[id]
		if !ok {
			continue
		}
		if r.MaxAutonomy != "" && spec.Rank(r.MaxAutonomy) < spec.Rank(next) {
			return fmt.Sprintf("%s is capped at %s by its spec (max_autonomy, in the fleets repo)", id, r.MaxAutonomy), ""
		}
		if spec.Rank(r.AutonomySet) < spec.Rank(next) {
			below = append(below, id)
		}
	}
	if len(below) > 0 {
		return "", strings.Join(below, ", ")
	}
	return "", ""
}

// trustReason is the record in one sentence, so a promotion's recorded why is the evidence, not whatever is typed in a hurry.
func trustReason(a api.ActionStatus, st store.Streak, ans [2]int) string {
	var s string
	if a.Autonomy == spec.AutonomyRecord {
		s = fmt.Sprintf("%d recorded plans in a row judged right", st.Clean)
	} else {
		s = fmt.Sprintf("%d plans in a row approved as they stood and verified", st.Clean)
	}
	if ans[0]+ans[1] > 0 {
		s += fmt.Sprintf("; approved %d, rejected %d in all", ans[0], ans[1])
	}
	if a.Record.Failed > 0 {
		s += fmt.Sprintf("; %s in all", plural(a.Record.Failed, "failure", "failures"))
	}
	return s
}

// earnedLine is the one line Now and the brief show for an action that is ready, with what else it takes when a cap or a
// routine stands in the way.
func earnedLine(t trust) string {
	s := fmt.Sprintf("%s has earned %s: %s.", t.Name, t.Next, t.Reason)
	switch {
	case t.Cap != "":
		s += " Promoting it changes nothing until that cap is raised: " + t.Cap + "."
	case t.Also != "":
		s += " " + t.Also + " runs below " + t.Next + ", so promote it as well."
	}
	return s
}

// earned is the ready actions, by name.
func earned(ts []trust) []trust {
	var out []trust
	for _, t := range ts {
		if t.Ready {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// judgeRow is a recorded plan that waits for a person's Right or Wrong, on Now.
type judgeRow struct{ Case, Title, Action string }

// toJudge is up to limit recorded plans nobody has judged, newest first, and how many wait in all.
func (u *Server) toJudge(ctx context.Context, rows []caseRow, limit int) ([]judgeRow, int, error) {
	ps, n, err := u.API.Store.UnjudgedRecorded(ctx, limit)
	if err != nil {
		return nil, 0, err
	}
	titles := map[string]string{}
	for _, c := range rows {
		titles[c.ID] = c.Title
	}
	var out []judgeRow
	for _, p := range ps {
		t := titles[p.CaseID]
		if t == "" {
			t = p.CaseID
		}
		out = append(out, judgeRow{Case: p.CaseID, Title: t, Action: p.Action})
	}
	return out, n, nil
}

// trustItems is the brief's Trust section: each action that is ready, and how many recorded plans wait for a verdict.
func (u *Server) trustItems(ctx context.Context, routines []api.RoutineStatus) ([]store.BriefItem, error) {
	ts, err := u.actionTrust(ctx, routines)
	if err != nil {
		return nil, err
	}
	var out []store.BriefItem
	for _, t := range earned(ts) {
		out = append(out, store.BriefItem{Text: earnedLine(t), Href: "/autonomy#action-" + t.Name, Kind: "trust:ready"})
	}
	_, n, err := u.API.Store.UnjudgedRecorded(ctx, 0)
	if err != nil {
		return nil, err
	}
	if n > 0 {
		out = append(out, store.BriefItem{Text: plural(n, "recorded plan waits", "recorded plans wait") +
			" for your Right or Wrong on Now: each one judged counts toward its action's next level.", Href: "/", Kind: "trust:judge"})
	}
	return out, nil
}
