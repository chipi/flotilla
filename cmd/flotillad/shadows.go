package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/chipi/flotilla/internal/api"
	"github.com/chipi/flotilla/internal/spec"
)

// checkShadows is what flotillad refuses to start with when shadows are on: a routine with more than one version whose shadow worker
// is not configured as one, a shadow worker that a routine runs on, or a shadow worker with no remote link yet (remote shadows are
// not supported). With shadows off nothing is checked: a version in the shadow state is listed and promotable, as before.
func checkShadows(cfg *Config, routines []*spec.Routine) []error {
	var errs []error
	for name, w := range cfg.Workers {
		if w.Shadow && w.Remote != "" {
			errs = append(errs, fmt.Errorf("worker %s: a remote worker cannot be a shadow worker yet", name))
		}
	}
	if !cfg.Shadows.Enabled {
		return errs
	}
	if cfg.Shadows.QueueMax < 0 {
		errs = append(errs, fmt.Errorf("shadows.queue_max %d: 0 (the default, 20) or more", cfg.Shadows.QueueMax))
	}
	for _, j := range routines {
		if w, ok := cfg.Workers[j.Worker]; ok && w.Shadow && j.Enabled {
			errs = append(errs, fmt.Errorf("%s runs on worker %s, which is declared shadow: true: a shadow worker runs shadow versions only", j.ID(), j.Worker))
		}
		if len(j.Versions) < 2 || !j.Enabled {
			continue
		}
		if w := cfg.Workers[j.Worker]; w.Remote != "" {
			errs = append(errs, fmt.Errorf("%s has %d versions and runs on the remote worker %s: a remote worker's routine cannot be shadowed yet (its state snapshot and repository head are read through the link, which has neither)", j.ID(), len(j.Versions), j.Worker))
			continue
		}
		sw := j.ShadowWorkerName()
		if w, ok := cfg.Workers[sw]; !ok || !w.Shadow {
			errs = append(errs, fmt.Errorf("%s has %d versions, and its shadow worker %s is not a worker declared shadow: true in the config", j.ID(), len(j.Versions), sw))
		}
	}
	sort.Slice(errs, func(a, b int) bool { return errs[a].Error() < errs[b].Error() })
	return errs
}

// shadowCheckLines is what -check says about shadows.
func shadowCheckLines(cfg *Config, routines []*spec.Routine) []string {
	if !cfg.Shadows.Enabled {
		return []string{"shadows: off (shadows.enabled is not set: a shadow version is listed and promotable, and does not run)"}
	}
	qm := cfg.Shadows.QueueMax
	if qm == 0 {
		qm = 20
	}
	out := []string{fmt.Sprintf("shadows: on (queue_max %d, after_live %v)", qm, cfg.Shadows.AfterLive)}
	for _, j := range routines {
		if len(j.Versions) < 2 || !j.Enabled {
			continue
		}
		var vs []string
		for _, v := range j.Versions {
			if v.Number != j.Version {
				vs = append(vs, fmt.Sprintf("v%d", v.Number))
			}
		}
		out = append(out, fmt.Sprintf("shadows: %s %s on worker %s, which must answer /healthz with mode shadow; the shadow worker has the read environment only", j.ID(), strings.Join(vs, ","), j.ShadowWorkerName()))
	}
	return out
}

// callerTokens are the tokens that may call flotillad's workers' listener: every worker's but a shadow worker's. flotillad needs a
// shadow worker's token to call it, and nothing else: a shadow routine that reads the token file it is given must not be able to use
// it on the ledger or the notes (the listener would take it for a worker's).
func callerTokens(workers map[string]WorkerConfig, tokens map[string]string) map[string]string {
	out := map[string]string{}
	for name, tok := range tokens {
		if !workers[name].Shadow {
			out[name] = tok
		}
	}
	return out
}

// listenerCallers is the callers file's plus, unless run tokens are required, a worker's own token (never a shadow worker's) for the workers'
// listener (RFC-0002 §9 step 5). Under required nothing is added: a worker's own token opens nothing there, and a run's token is no caller.
func listenerCallers(cfg *Config, callers []api.Caller, workerTokens map[string]string) []api.Caller {
	if cfg.RunTokens == RunTokensRequired {
		return callers
	}
	return append(callers, api.WorkerCallers(callerTokens(cfg.Workers, workerTokens))...)
}

// sharedTokenError refuses two different workers that hold one token (the same file, or the same bytes in two): a token two workers
// hold cannot say which is calling, and one a shadow worker shared with a live worker would give a candidate the live worker's
// access. One worker listed under two names (the same url) is one worker and may share its token.
func sharedTokenError(workers map[string]WorkerConfig, tokens map[string]string) error {
	byToken := map[string][]string{}
	for name, tok := range tokens {
		byToken[api.HashToken(tok)] = append(byToken[api.HashToken(tok)], name)
	}
	var shared []string
	for _, names := range byToken {
		sort.Strings(names)
		urls := map[string]bool{}
		for _, n := range names {
			urls[workers[n].URL] = true
		}
		if len(urls) > 1 {
			shared = append(shared, strings.Join(names, " and "))
		}
	}
	if len(shared) == 0 {
		return nil
	}
	sort.Strings(shared)
	return fmt.Errorf("workers %s share a token file: give each worker its own token", strings.Join(shared, "; workers "))
}
