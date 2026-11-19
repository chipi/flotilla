package dispatch

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/chipi/flotilla/internal/input"
	"github.com/chipi/flotilla/internal/sched"
	"github.com/chipi/flotilla/internal/spec"
	"github.com/chipi/flotilla/internal/store"
)

// What a shadow run left behind goes to the shadow records and nowhere else (v2 step 2, design section 4, layer 1). This file is the
// redirect: the shadow's counterpart of Detected, run, check and execute, and it calls none of what they call to change the
// system (cases, plans, leases, spend, events, the brief, Slack asks, host executions). TestTheShadowPathCallsNoLiveWrite reads
// this file and the scheduler's shadow file and fails on any of those names.

// The kinds of shadow record ShadowDone writes. Pairs match a shadow record to the live side by (input, subject).
const (
	RecRunStarted   = "run.started"
	RecRunEnded     = "run.ended"
	RecReport       = "report"         // a monitor's report, whole
	RecReportBad    = "report.refused" // a report that cannot be read, with why
	RecDetectOpen   = "detect.open"    // a detection with no open case on the live keys: it would open one
	RecDetectAttach = "detect.attach"  // a detection whose key is an open case's
	RecDetectClear  = "detect.clear"   // an open case's key the report no longer has: the live path would clear it
	RecDetectBad    = "detect.refused" // a detection the routine may not make
	RecPlan         = "plan"           // a case routine's plan: what it decided for the case
	RecDecision     = "decision"       // any other outcome for a case, a check's or an execute's, a ledger decision
	RecWouldRun     = "would_run"      // an action the shadow would have taken, blocked: it is never enqueued or sent
	RecNote         = "note"           // a note it would have proposed
	BlockedByShadow = "shadow"         // why a would_run did not run
)

func rec(run store.Run, hash, subject, kind string, payload any) store.ShadowRecord {
	b, _ := json.Marshal(payload)
	return store.ShadowRecord{RunID: run.ID, Routine: run.Routine, Version: run.Version, VersionHash: hash, InputKey: run.InputKey,
		Subject: subject, Kind: kind, Payload: b}
}

// ShadowStarted marks the start of a shadow run with both start times: the live run's input and this run.
func ShadowStarted(ctx context.Context, st *store.Store, run store.Run, hash string, in store.Input) error {
	_, err := st.RecordShadow(ctx, rec(run, hash, "", RecRunStarted, map[string]any{
		"started_at": run.StartedAt, "live_started_at": in.StartedAt, "input_kind": in.Kind}))
	return err
}

// ShadowDone records what a finished shadow run decided, against the input it was given, and never changes the system: a
// monitor's report whole with what it would have opened, attached and cleared against the live case keys as of the input; a case
// routine's decision; a check's or an execute's outcome with the action it would have run. A failed run records its ending only.
func ShadowDone(ctx context.Context, st *store.Store, sj *spec.Routine, run store.Run, hash string, in store.Input, out *sched.CaseOutcome) error {
	var recs []store.ShadowRecord
	ended := map[string]any{"status": run.Status, "detail": run.Detail, "spend_usd": run.SpendUSD}
	if run.DurationS != nil {
		ended["duration_s"] = *run.DurationS
	}
	recs = append(recs, rec(run, hash, "", RecRunEnded, ended))
	if run.Status == store.StatusOK && out != nil {
		more, err := shadowDecisions(sj, run, hash, in, out)
		if err != nil {
			return err
		}
		recs = append(recs, more...)
	}
	for _, r := range recs {
		if _, err := st.RecordShadow(ctx, r); err != nil {
			return err
		}
	}
	return nil
}

func shadowDecisions(sj *spec.Routine, run store.Run, hash string, in store.Input, out *sched.CaseOutcome) ([]store.ShadowRecord, error) {
	env, err := input.FromStore(in)
	if err != nil {
		return nil, err
	}
	caseID := ""
	if len(env.Case) > 0 {
		var c struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(env.Case, &c)
		caseID = c.ID
	}
	var recs []store.ShadowRecord
	if len(sj.Detects) > 0 && in.Kind == store.InputTick {
		return monitorRecords(sj, run, hash, in, out), nil
	}
	if in.Kind == store.InputTick {
		return nil, nil // a tick that reports nothing in a plan file (triage): its decisions come through its ledger writes
	}
	if out.PlanError != "" || len(out.Plan) == 0 {
		why := out.PlanError
		if why == "" {
			why = "wrote no plan file"
		}
		return []store.ShadowRecord{rec(run, hash, caseID, RecDecision, map[string]any{"outcome": "none", "detail": why})}, nil
	}
	var dec Decision
	if err := json.Unmarshal(out.Plan, &dec); err != nil {
		return []store.ShadowRecord{rec(run, hash, caseID, RecDecision, map[string]any{"outcome": "none", "detail": "plan file: " + err.Error()})}, nil
	}
	subject := caseID
	if subject == "" {
		subject = dec.CaseID
	}
	switch in.Kind {
	case store.InputExecute:
		// the live executing run carried out the approved plan it was handed; the shadow's would-be action is that plan's
		var plan struct {
			Action string            `json:"action"`
			Params map[string]string `json:"params"`
		}
		_ = json.Unmarshal(env.Plan, &plan)
		recs = append(recs, rec(run, hash, subject, RecWouldRun, wouldRun(plan.Action, plan.Params)))
	case store.InputCase:
		if dec.Outcome == "plan" {
			recs = append(recs, rec(run, hash, subject, RecPlan, dec))
			recs = append(recs, rec(run, hash, subject, RecWouldRun, wouldRun(dec.Action, dec.Params)))
			return recs, nil
		}
	}
	return append(recs, rec(run, hash, subject, RecDecision, dec)), nil
}

func wouldRun(action string, params map[string]string) map[string]any {
	return map[string]any{"action": action, "params": params, "blocked": BlockedByShadow,
		"text": fmt.Sprintf("would run %s (blocked: %s)", action, BlockedByShadow)}
}

func monitorRecords(sj *spec.Routine, run store.Run, hash string, in store.Input, out *sched.CaseOutcome) []store.ShadowRecord {
	if why, _ := ReportRefusal(store.StatusOK, out); why != "" {
		return []store.ShadowRecord{rec(run, hash, "", RecReportBad, map[string]any{"why": why})}
	}
	rp, why := PlanReport(sj.Detects, out.Plan)
	recs := []store.ShadowRecord{rec(run, hash, "", RecReport, json.RawMessage(out.Plan))}
	if why != "" {
		return append(recs, rec(run, hash, "", RecReportBad, map[string]any{"why": why}))
	}
	live := map[string]bool{}
	for _, k := range in.LiveCaseKeys {
		live[k] = true
	}
	for _, x := range rp.Items {
		kind := RecDetectOpen
		if live[x.Key.String()] {
			kind = RecDetectAttach
		}
		recs = append(recs, rec(run, hash, x.Key.String(), kind, map[string]any{"kind": x.Kind, "target": x.Target, "title": x.Title, "detail": x.Detail}))
	}
	for _, x := range rp.Refused {
		recs = append(recs, rec(run, hash, "", RecDetectBad, map[string]any{"kind": x.Kind, "target": x.Target}))
	}
	for _, k := range in.LiveCaseKeys {
		if !rp.Present[k] {
			recs = append(recs, rec(run, hash, k, RecDetectClear, map[string]any{}))
		}
	}
	return recs
}
