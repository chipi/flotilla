package ui

import (
	"context"
	"fmt"
	"github.com/chipi/flotilla/internal/registry"
	"sort"
	"strings"
	"time"

	"github.com/chipi/flotilla/internal/cases"
	"github.com/chipi/flotilla/internal/store"
)

// decision is mockup v3's Decision box: one plan that waits for the operator, with everything the
// answer binds to. It is built from the case, its newest plan, the run that planned it and the
// registry's declaration of the action.
type decision struct {
	ID, Href, KindLabel, Fleet, Waiting string
	Title                               string
	Action, On, Command                 string
	Risk, Reversible, Channel           string
	ProposedBy, Run                     string
	Why                                 []string
	Evidence                            []string
	After                               string
	Hash, Short                         string
	CLI                                 bool
	CLIWhy, CLICommand                  string
	SelfPage                            bool
	ActionName                          string
	Change                              string // a repo change's diff, shown as written (ADR-0016)
}

// decisionFor is the box for a case awaiting approval; ok is false when it has no plan.
func (u *Server) decisionFor(ctx context.Context, c caseRow, selfPage bool) (decision, bool, error) {
	plans, err := u.API.Store.Plans(ctx, c.ID)
	if err != nil || len(plans) == 0 {
		return decision{}, false, err
	}
	p := plans[len(plans)-1]
	d := decision{ID: c.ID, Href: "/cases/" + c.ID, KindLabel: strings.ReplaceAll(c.Kind, "_", " "), Fleet: c.FleetName,
		Waiting: c.Age, SelfPage: selfPage, Hash: p.PlanHash, Short: shortHash(p.PlanHash), ActionName: p.Action}
	keys := make([]string, 0, len(p.Params))
	for k := range p.Params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	args := make([]string, len(keys))
	for i, k := range keys {
		args[i] = k + "=" + p.Params[k]
	}
	d.Action = p.Action + "(" + strings.Join(args, ", ") + ")"
	d.On = c.Target
	d.Command = u.hostCommand(ctx, hostOf(c.Target), p.Action)
	executor := ""
	if a, ok := registryAction(u, p.Action); ok && a.Repo() {
		executor = u.executorOf(p.Action)
		d.Action, d.Change = p.Action+" on "+p.Params["file"], p.Params["change"]
		d.Command = "nothing runs by itself: once you approve, apply the change and record its commit on the case"
		if p.Params["diff"] != "" {
			d.Action = fmt.Sprintf("%s on %s at %s", p.Action, p.Params["repo"], p.Params["base"])
			d.Change = strings.TrimSpace(p.Params["title"] + "\n\n" + p.Params["diff"])
		}
		if executor != "" {
			d.Command = "once you approve, " + executor + " opens it as a draft PR, once; you merge it"
		}
	}
	d.Title = c.Title
	d.Risk, d.Reversible, d.Channel = "unknown", "not declared", "here or the CLI"
	if u.API.Registry != nil {
		if a, ok := u.API.Registry.Action(p.Action); ok {
			d.Risk = a.Risk
			switch a.Reversible {
			case "yes":
				d.Reversible = "reversible"
			case "regenerable":
				d.Reversible = "regenerable: it can be made again"
			case "no":
				d.Reversible = "no: it cannot be undone"
			}
			d.After = "flotillad signs the plan and the host agent on " + hostOf(c.Target) + " runs it once."
			if a.Check.Verify != "" {
				d.After += fmt.Sprintf(" A check (%s) looks for the result within %s.", a.Check.Verify, words(time.Duration(a.Check.Within)))
			}
			if a.Risk == "high" || a.Reversible == "no" {
				d.CLI, d.Channel = true, "the CLI only"
				d.CLIWhy = "High risk or irreversible: only the CLI can approve it."
			}
		}
	}
	if d.After == "" {
		d.After = "flotillad signs the plan and the host agent on " + hostOf(c.Target) + " runs it once."
	}
	if a, ok := registryAction(u, p.Action); ok && a.Executor == registry.ExecRoutine {
		// ADR-0017: a routine carries it out against the system it reaches; no host is involved
		by := u.executorOf(p.Action)
		if by == "" {
			by = "no routine is enabled to carry it out, so"
			d.Command = "nothing runs: no enabled routine carries this action out"
			d.After = "Approving records your yes; " + by + " nothing changes until one is."
		} else {
			d.Command = "once you approve, " + by + " carries it out, once"
			d.After = "Approving starts " + by + " once: it re-checks what the plan assumed, then makes exactly this change."
		}
		if a.Check.Verify != "" {
			d.After += fmt.Sprintf(" A check (%s) looks for the result within %s.", a.Check.Verify, words(time.Duration(a.Check.Within)))
		}
	}
	if d.Change != "" && executor != "" {
		d.After = "Approving starts " + executor + " once: it opens a draft PR with exactly this diff. Merged, the case is verified; closed without merging, it is escalated. You merge, always: the fleet never does."
	} else if d.Change != "" {
		d.After = "Approving records your yes. You apply the change, it goes through review, CI and deploy, and you record the commit here; until step 4 no PR is opened for you."
	}
	if ok, _ := u.API.SlackApprovable(p.Action); !d.CLI && ok && u.API.Slack != nil {
		d.Channel = "here, the CLI or Slack"
	}
	d.CLICommand = "flotilla case approve " + c.ID + " --plan " + p.PlanHash
	d.Run = p.RunID
	if r, err := u.API.Store.Run(ctx, p.RunID); err == nil && r != nil {
		d.ProposedBy = r.Routine
	} else {
		d.ProposedBy = "an unknown run"
	}
	d.Why = p.Reasons
	evs, err := u.API.Store.CaseEvents(ctx, c.ID)
	if err != nil {
		return d, false, err
	}
	for _, e := range evs {
		if (e.By == cases.ByAct || e.By == cases.ByDiagnose || e.Seq == evs[0].Seq) && e.Detail != "" {
			if _, rest := splitRun(e.Detail); rest != "" {
				d.Evidence = append(d.Evidence, rest)
			}
		}
	}
	return d, true, nil
}

// hostOf is the host part of a target ("host:backup-01" -> "backup-01", "model-host/pyannote" -> "model-host").
func hostOf(target string) string {
	t := strings.TrimPrefix(target, "host:")
	if h, _, ok := strings.Cut(t, "/"); ok {
		return h
	}
	return t
}

// planLevels reads policy's "autonomy: routine X, action Y, host ceiling Z" reason into the levels the
// Case page's "Why policy asked" lists.
func planLevels(p store.PlanRecord) (routine, action, host string, ok bool) {
	for _, r := range p.Reasons {
		rest, found := strings.CutPrefix(r, "autonomy: ")
		if !found {
			continue
		}
		for _, part := range strings.Split(rest, ", ") {
			f := strings.Fields(part)
			if len(f) < 2 {
				continue
			}
			switch f[0] {
			case "routine":
				routine = f[len(f)-1]
			case "action":
				action = f[len(f)-1]
			case "host":
				host = f[len(f)-1]
			}
		}
		return routine, action, host, true
	}
	return "", "", "", false
}

// words writes a duration as people say it: "2 hours", "30 minutes", "1 hour 30 minutes".
func words(d time.Duration) string {
	h, m := int(d.Hours()), int(d.Minutes())%60
	var parts []string
	if h > 0 {
		parts = append(parts, plural(h, "hour", "hours"))
	}
	if m > 0 || h == 0 {
		parts = append(parts, plural(m, "minute", "minutes"))
	}
	return strings.Join(parts, " ")
}

func registryAction(u *Server, name string) (registry.Action, bool) {
	if u.API.Registry == nil {
		return registry.Action{}, false
	}
	return u.API.Registry.Action(name)
}

// executorOf is the routine that carries out an approved repo action (RFC-0010 §3), "" when a person
// applies it.
func (u *Server) executorOf(action string) string {
	if u.API.Sched == nil {
		return ""
	}
	for _, j := range u.API.Sched.Routines {
		if j.Enabled && j.OnExecute() == action {
			return j.ID()
		}
	}
	return ""
}
