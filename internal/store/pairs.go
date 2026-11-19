package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Pairs (v2 step 2, design section 4): for one input a live version and a shadow version each decided some things, and a pair is a subject
// both decided. A subject is the thing decided on: a triage signal (its ledger decision's subject), a case (its plan or its outcome), a
// detection's key (what a monitor opened, attached or cleared). Pairs are matched per subject; a subject one side decided and the other did
// not is unpaired, on its side. flotilla shows both and counts them; it never says which is better (that is eval-harness's).

// Subject families, as the pairs read them.
const (
	PairOutcomePlan = "plan"
)

// PairSide is one version's decision on a subject.
type PairSide struct {
	Version int       `json:"version"`
	Hash    string    `json:"hash,omitempty"`
	Outcome string    `json:"outcome"`          // what it decided, as a short key two sides are compared by: "file", "plan:backup.rerun", "attach", ...
	Text    string    `json:"text"`             // the same for people: "attach to case", "would run backup.rerun (blocked: shadow)"
	Detail  string    `json:"detail,omitempty"` // why, as the version said it
	Plan    *PairPlan `json:"plan,omitempty"`
	RunID   string    `json:"run_id,omitempty"`
	At      string    `json:"at,omitempty"`
	// Per station (the version's station that made the call; the harness role for a call from before stations): turns, tokens, the gateway's cost
	// and the rendered texts of the run that decided.
	Stations []PairStation `json:"stations,omitempty"`
}

// PairPlan is the plan a side made for a case: its action and parameters, and for a shadow whether it was blocked.
type PairPlan struct {
	Action  string            `json:"action"`
	Params  map[string]string `json:"params,omitempty"`
	Blocked string            `json:"blocked,omitempty"` // "shadow": it would have run, and did not
	Ran     bool              `json:"ran,omitempty"`     // the live side: it was sent to a host
}

// PairText is one rendered text a station read: its name, hash and length, and its body (masked) unless it was over the cap.
type PairText struct {
	Name    string  `json:"name"`
	SHA256  string  `json:"sha256"`
	Bytes   int64   `json:"bytes"`
	Text    *string `json:"text,omitempty"`
	OverCap bool    `json:"over_cap,omitempty"`
}

// PairStation is what one station of a run used; Role is the station's name, or the harness role for rows from before stations.
type PairStation struct {
	Texts     []PairText `json:"texts,omitempty"`
	Role      string     `json:"role"`
	Calls     int        `json:"calls"`
	TokensIn  int64      `json:"tokens_in"`
	TokensOut int64      `json:"tokens_out"`
	CostUSD   float64    `json:"cost_usd"`
}

// Pair is one subject of one input, with each side's decision; a side that did not decide it is nil.
type Pair struct {
	InputKey string    `json:"input_key"`
	Subject  string    `json:"subject"`
	At       string    `json:"at"` // the live run's start
	Live     *PairSide `json:"live,omitempty"`
	Shadow   *PairSide `json:"shadow,omitempty"`
	Same     bool      `json:"same"` // both decided it, and the same
}

// PairSet is a version's pairs against the live version they were made beside, newest first.
type PairSet struct {
	Routine     string `json:"routine"`
	Version     int    `json:"version"`
	LiveVersion int    `json:"live_version"`
	Since       string `json:"since,omitempty"`
	Until       string `json:"until,omitempty"`
	Paired      int    `json:"paired"`
	Differ      int    `json:"differ"`
	Unpaired    int    `json:"unpaired"`
	Pairs       []Pair `json:"pairs"`
}

// PairQuery picks the pairs: a routine's version, in a window of the live runs' starts.
type PairQuery struct {
	Routine      string
	Version      int
	Since, Until string // the live run's start, as stored (UTC)
}

// Pairs reads a version's pairs. The shadow side is its shadow records of the input; the live side is what the live run of the input did
// on the same subject: a ledger decision, a plan (and whether it was sent to a host), or a change to the case or incident the subject names.
func (s *Store) Pairs(ctx context.Context, q PairQuery) (PairSet, error) {
	out := PairSet{Routine: q.Routine, Version: q.Version, Since: q.Since, Until: q.Until, Pairs: []Pair{}}
	if lv, ok, err := s.LiveVersion(ctx, q.Routine); err != nil {
		return out, err
	} else if ok {
		out.LiveVersion = lv.Version
	}
	key := ShadowKey(q.Routine, q.Version)
	// the inputs this version ran on and finished (shadow_runs names the live routine), each with the shadow run that did
	qr := `SELECT i.input_key, i.started_at, i.version, r.run_id, r.version_hash FROM shadow_runs r JOIN run_inputs i ON i.input_key = r.input_key
		WHERE r.routine = ? AND r.version = ? AND r.status = ?`
	args := []any{q.Routine, q.Version, ShadowDone}
	if q.Since != "" {
		qr, args = qr+` AND i.started_at >= ?`, append(args, q.Since)
	}
	if q.Until != "" {
		qr, args = qr+` AND i.started_at < ?`, append(args, q.Until)
	}
	rows, err := s.db.QueryContext(ctx, qr+` ORDER BY i.started_at DESC`, args...)
	if err != nil {
		return out, err
	}
	type inputRow struct {
		key, at, shadowRun, hash string
		liveVersion              int
	}
	var inputs []inputRow
	for rows.Next() {
		var x inputRow
		if err := rows.Scan(&x.key, &x.at, &x.liveVersion, &x.shadowRun, &x.hash); err != nil {
			rows.Close()
			return out, err
		}
		inputs = append(inputs, x)
	}
	rows.Close()
	for _, in := range inputs {
		recs, err := s.ShadowRecords(ctx, ShadowRecordFilter{InputKey: in.key, Routine: key})
		if err != nil {
			return out, err
		}
		shadow := shadowSides(recs, q.Version, in.hash, in.shadowRun)
		live, err := s.liveSides(ctx, in.key, q.Routine, in.liveVersion, shadow)
		if err != nil {
			return out, err
		}
		subjects := map[string]bool{}
		for k := range shadow {
			subjects[k] = true
		}
		for k := range live {
			subjects[k] = true
		}
		names := make([]string, 0, len(subjects))
		for k := range subjects {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, sub := range names {
			p := Pair{InputKey: in.key, Subject: sub, At: in.at, Live: live[sub], Shadow: shadow[sub]}
			switch {
			case p.Live != nil && p.Shadow != nil:
				p.Same = p.Live.Outcome == p.Shadow.Outcome
				out.Paired++
				if !p.Same {
					out.Differ++
				}
			default:
				out.Unpaired++
			}
			out.Pairs = append(out.Pairs, p)
		}
	}
	for i := range out.Pairs {
		p := &out.Pairs[i]
		if p.Live != nil {
			p.Live.Stations, _ = s.stations(ctx, p.InputKey)
		}
		if p.Shadow != nil {
			p.Shadow.Stations, _ = s.stations(ctx, p.Shadow.RunID)
		}
	}
	return out, nil
}

// PairsFor is how many pairs a version has: subjects the shadow version and the live version both decided on one input.
func (s *Store) PairsFor(ctx context.Context, routine string, version int) (int, error) {
	ps, err := s.Pairs(ctx, PairQuery{Routine: routine, Version: version})
	return ps.Paired, err
}

// shadowSides folds a shadow run's records of one input into what it decided per subject.
func shadowSides(recs []ShadowRecord, version int, hash, runID string) map[string]*PairSide {
	out := map[string]*PairSide{}
	side := func(r ShadowRecord) *PairSide {
		if out[r.Subject] == nil {
			out[r.Subject] = &PairSide{Version: version, Hash: hash, RunID: runID, At: r.CreatedAt}
		}
		return out[r.Subject]
	}
	for _, r := range recs {
		if r.Subject == "" {
			continue
		}
		var p map[string]any
		_ = json.Unmarshal(r.Payload, &p)
		str := func(k string) string { v, _ := p[k].(string); return v }
		switch r.Kind {
		case "decision":
			sd := side(r)
			o := str("disposition")
			if o == "" {
				o = str("outcome")
			}
			sd.Outcome, sd.Text, sd.Detail = o, o, cut(firstOf(str("reason"), str("detail")), 160)
		case "plan":
			sd := side(r)
			sd.Outcome = PairOutcomePlan + ":" + str("action")
			sd.Text, sd.Detail = "plan "+str("action"), cut(str("detail"), 160)
			pl := &PairPlan{Action: str("action")}
			if params, ok := p["params"].(map[string]any); ok {
				pl.Params = map[string]string{}
				for k, v := range params {
					pl.Params[k] = fmt.Sprint(v)
				}
			}
			sd.Plan = pl
		case "would_run":
			sd := side(r)
			if sd.Plan == nil {
				sd.Plan = &PairPlan{Action: str("action")}
			}
			sd.Plan.Blocked = str("blocked")
			sd.Text = str("text")
			if sd.Outcome == "" {
				sd.Outcome = PairOutcomePlan + ":" + str("action")
			}
		case "detect.open":
			sd := side(r)
			sd.Outcome, sd.Text, sd.Detail = "open", "open case", cut(str("detail"), 160)
		case "detect.attach":
			sd := side(r)
			sd.Outcome, sd.Text, sd.Detail = "attach", "attach to case", cut(str("detail"), 160)
		case "detect.clear":
			sd := side(r)
			sd.Outcome, sd.Text = "clear", "clear"
		}
	}
	return out
}

func firstOf(xs ...string) string {
	for _, x := range xs {
		if x != "" {
			return x
		}
	}
	return ""
}

func cut(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

// liveSides is what the live run of an input decided, for the subjects the shadow decided and for the ones only it did: its ledger decisions,
// its plans (sent to a host or not) and the changes it made to cases and incidents.
func (s *Store) liveSides(ctx context.Context, inputKey, routine string, version int, shadow map[string]*PairSide) (map[string]*PairSide, error) {
	out := map[string]*PairSide{}
	hash := ""
	if vs, err := s.Versions(ctx, routine); err == nil {
		for _, v := range vs {
			if v.Version == version {
				hash = v.Hash
			}
		}
	}
	mk := func(at string) *PairSide { return &PairSide{Version: version, Hash: hash, RunID: inputKey, At: at} }
	// ledger decisions of the run, in a live namespace
	rows, err := s.db.QueryContext(ctx, `SELECT `+entrySubject+`, e.row, e.written_at FROM ledger_entries e WHERE e.run_id = ? AND e.kind = 'decision' AND instr(e.namespace, '@v') = 0 ORDER BY e.seq`, inputKey)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var sub, raw, at string
		if err := rows.Scan(&sub, &raw, &at); err != nil {
			rows.Close()
			return nil, err
		}
		var row map[string]string
		_ = json.Unmarshal([]byte(raw), &row)
		sd := mk(at)
		sd.Outcome, sd.Text, sd.Detail = row["disposition"], row["disposition"], cut(row["reason"], 160)
		out[sub] = sd
	}
	rows.Close()
	// plans of the run, and whether each was sent to a host
	rows, err = s.db.QueryContext(ctx, `SELECT p.case_id, p.action, p.params, p.outcome, p.decided_at,
		(SELECT COUNT(*) FROM host_executions h WHERE h.plan_id = p.id) FROM plans p WHERE p.run_id = ? ORDER BY p.id`, inputKey)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var caseID, action, params, outcome, at string
		var sent int
		if err := rows.Scan(&caseID, &action, &params, &outcome, &at, &sent); err != nil {
			rows.Close()
			return nil, err
		}
		sd := mk(at)
		sd.Outcome = PairOutcomePlan + ":" + action
		sd.Plan = &PairPlan{Action: action, Ran: sent > 0}
		_ = json.Unmarshal([]byte(params), &sd.Plan.Params)
		if sent > 0 {
			sd.Text = "ran " + action
		} else {
			sd.Text = "plan " + action + " (" + outcome + ")"
		}
		out[caseID] = sd
	}
	rows.Close()
	// what the run did to a case, or to the case a detection key names: "run <id>: ..." on its timeline, and an incident cleared in its window
	rows, err = s.db.QueryContext(ctx, `SELECT c.id, c.canonical_key, e.kind, e.to_status, e.detail, e.at FROM case_events e JOIN cases c ON c.id = e.case_id
		WHERE e.detail LIKE ? ESCAPE '\' ORDER BY e.seq`, "run "+likeEscape(inputKey)+":%")
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id, ckey, kind, to, detail, at string
		if err := rows.Scan(&id, &ckey, &kind, &to, &detail, &at); err != nil {
			rows.Close()
			return nil, err
		}
		subject, outcome, text := id, kind, kind
		switch kind {
		case "case.detected":
			subject, outcome, text = ckey, "open", "open case"
		case "signal.attached", signalAbsorbed:
			subject, outcome, text = ckey, "attach", "attach to case"
		default:
			if to != "" {
				outcome, text = "moved:"+to, "moved the case to "+to
			}
		}
		if out[subject] != nil && out[subject].Plan != nil {
			continue // the plan says it better
		}
		sd := mk(at)
		sd.Outcome, sd.Text, sd.Detail = outcome, text, cut(strings.TrimSpace(strings.TrimPrefix(detail, "run "+inputKey+":")), 160)
		out[subject] = sd
	}
	rows.Close()
	// an incident the monitor stopped reporting: cleared by the live run, with no run id on the event, so by its subject and the run's window
	for sub := range shadow {
		if !strings.HasPrefix(sub, "target=") || out[sub] != nil {
			continue
		}
		var at string
		// shadow-ok: the live run of the input, by its id, for the window its incident was cleared in
		err := s.db.QueryRowContext(ctx, `SELECT e.at FROM case_events e JOIN cases c ON c.id = e.case_id JOIN run_inputs i ON i.input_key = ?
			WHERE c.canonical_key = ? AND e.kind = ? AND e.actor = i.routine AND e.at >= i.started_at
			AND e.at <= COALESCE((SELECT ended_at FROM runs WHERE id = i.input_key), e.at) ORDER BY e.seq LIMIT 1`, inputKey, sub, incidentCleared).Scan(&at)
		if err == nil {
			sd := mk(at)
			sd.Outcome, sd.Text = "clear", "clear"
			out[sub] = sd
		}
	}
	return out, nil
}

// stations is what each role of a run used (the gateway's cost), for one run id. Steps are not calls.
func (s *Store) stations(ctx context.Context, runID string) ([]PairStation, error) {
	// shadow-ok: the calls of one run by id, whichever kind it is
	rows, err := s.db.QueryContext(ctx, `SELECT COALESCE(NULLIF(station, ''), role), COUNT(*), COALESCE(SUM(tokens_in), 0), COALESCE(SUM(tokens_out), 0), COALESCE(SUM(cost_usd), 0)
		FROM run_calls WHERE run_id = ? AND kind <> 'step' GROUP BY 1 ORDER BY 1`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PairStation
	for rows.Next() {
		var p PairStation
		if err := rows.Scan(&p.Role, &p.Calls, &p.TokensIn, &p.TokensOut, &p.CostUSD); err != nil {
			return nil, err
		}
		if p.Role == "" {
			p.Role = "(no role)"
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, s.stationTexts(ctx, runID, out)
}

// stationTexts fills each station's rendered texts: each distinct (name, hash) once, in the order the station first read it, with the body
// kept for its hash (nil when it was over the cap).
func (s *Store) stationTexts(ctx context.Context, runID string, sts []PairStation) error {
	// shadow-ok: the calls of one run by id, whichever kind it is
	rows, err := s.db.QueryContext(ctx, `SELECT COALESCE(NULLIF(station, ''), role), texts FROM run_calls WHERE run_id = ? AND kind <> 'step' AND texts <> '' ORDER BY id`, runID)
	if err != nil {
		return err
	}
	type seenKey struct{ label, name, sha string }
	seen := map[seenKey]bool{}
	byLabel := map[string][]PairText{}
	for rows.Next() {
		var label, raw string
		if err := rows.Scan(&label, &raw); err != nil {
			rows.Close()
			return err
		}
		var refs []PairText
		if json.Unmarshal([]byte(raw), &refs) != nil {
			continue
		}
		for _, r := range refs {
			k := seenKey{label, r.Name, r.SHA256}
			if !seen[k] {
				seen[k] = true
				byLabel[label] = append(byLabel[label], r)
			}
		}
	}
	rows.Close()
	for i := range sts {
		label := sts[i].Role
		if label == "(no role)" {
			label = ""
		}
		for _, t := range byLabel[label] {
			var body sql.NullString
			var over bool
			// reached only through a run's calls above, which are read by run id: the shadow rule is theirs
			err := s.db.QueryRowContext(ctx, `SELECT text, over_cap FROM call_texts WHERE sha256 = ?`, t.SHA256).Scan(&body, &over)
			if err != nil && err != sql.ErrNoRows {
				return err
			}
			if body.Valid {
				b := body.String
				t.Text = &b
			}
			t.OverCap = over || (err == nil && !body.Valid)
			sts[i].Texts = append(sts[i].Texts, t)
		}
	}
	return nil
}
