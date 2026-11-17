package ui

import (
	"context"
	"time"

	"github.com/chipi/flotilla/internal/spec"
	"github.com/chipi/flotilla/internal/store"
)

// routineModel adds what a routine's own model calls say to its page: the Instruments card, the small numbers
// on the model blocks and, when the model block is open, its panel. A routine that made no call in the window
// keeps the page it had.
func (u *Server) routineModel(ctx context.Context, d *routineData, j *spec.Routine, id string, c mixCounts, now time.Time, window int) error {
	since := now.AddDate(0, 0, -window).UTC().Format(store.TimeFormat)
	all, err := u.API.Store.RoutineCallsSince(ctx, id, since)
	if err != nil {
		return err
	}
	var calls []store.ModelCall
	for _, c := range all {
		if isAgentCall(c) {
			calls = append(calls, c)
		}
	}
	if len(calls) == 0 {
		return nil
	}
	today := now.UTC().Format("2006-01-02")
	days := dayStatsOf(calls, c.byDay)
	mc := modelCtx{Days: days, Focus: focusDay(days, today), Today: today, Window: window, Calls: calls, Probes: c.probes, Decs: c.decs,
		Spec: j, Aliases: u.aliases(), Loc: u.loc()}

	for li := range d.Lanes {
		for bi := range d.Lanes[li].Blocks {
			b := &d.Lanes[li].Blocks[bi]
			switch b.ID {
			case "harness.protocol", "models":
				b.Extra = mc.costNums()
			case "harness.limits":
				b.Extra = mc.limitsNums()
			case "harness.tools":
				b.Extra = mc.toolsNums()
			}
		}
	}
	if d.Sel != nil && (d.Sel.ID == "harness.protocol" || d.Sel.ID == "models") {
		d.Panel = mc.panel()
	}

	in := instInput{Days: days, Today: today, Window: window, DayBudget: j.Budget.DayUSD}
	if h := j.Harness; h != nil {
		in.MaxSteps, in.FinalCall = h.Limits.MaxSteps, h.Quirks.FinalDecisionCall
	}
	sha, model := latestPrompt(c.decs)
	switch {
	case sha == "":
		in.EvalGap = "no decision in the ledger names its prompt, so no eval result can be matched"
	default:
		latest, err := u.API.Store.LatestEval(ctx, id, sha, model)
		if err != nil {
			return err
		}
		older, err := u.API.Store.Evals(ctx, id, 200)
		if err != nil {
			return err
		}
		for _, e := range latest {
			v := evalView{Metric: e.Metric, Value: e.Value, N: e.N, Repeats: e.Repeats, SHA: e.PromptSHA, Source: e.Source}
			for _, o := range older {
				if o.Metric == e.Metric && o.Model == e.Model && o.PromptSHA != sha {
					pv := o.Value
					v.Prev = &pv
					break
				}
			}
			in.Evals = append(in.Evals, v)
		}
		if len(latest) == 0 {
			in.EvalGap = "no eval result for prompt " + shortText(sha, 8) + " on " + model
		}
	}
	if inst, ok := buildInstruments(in); ok {
		d.Inst = &inst
	}
	return nil
}

// latestPrompt is the newest decision's prompt sha and model.
func latestPrompt(decs []store.LedgerDecision) (sha, model string) {
	for i := len(decs) - 1; i >= 0; i-- {
		if decs[i].PromptSHA != "" {
			return decs[i].PromptSHA, decs[i].Model
		}
	}
	return "", ""
}

// routinePaths sums the order of the routine's recent runs into paths. It has nothing to say about a routine
// whose runs went through a single step each (triage's one model call is not a path).
func (u *Server) routinePaths(ctx context.Context, id string) (*pathSummary, error) {
	runs, err := u.API.Store.Runs(ctx, id, 100)
	if err != nil {
		return nil, err
	}
	ids := make([]string, len(runs))
	for i, r := range runs {
		ids[i] = r.ID
	}
	calls, err := u.API.Store.RunCalls(ctx, ids)
	if err != nil {
		return nil, err
	}
	flows := map[string]*runFlow{}
	multi := false
	for _, x := range ids {
		if f := buildFlow(calls[x]); f != nil {
			flows[x] = f
			multi = multi || len(f.Eps) > 1 || len(f.Steps) > 0
		}
	}
	if !multi {
		return nil, nil
	}
	ps := buildPaths(ids, flows)
	if ps.Runs == 0 {
		return nil, nil
	}
	return &ps, nil
}
