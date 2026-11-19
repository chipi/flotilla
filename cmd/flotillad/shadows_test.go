package main

import (
	"strings"
	"testing"

	"github.com/chipi/flotilla/internal/api"
	"github.com/chipi/flotilla/internal/spec"
)

// S1: a shadow worker's token is never a caller of flotillad's workers' listener. flotillad needs it to call the worker, and nothing else.
func TestAShadowWorkersTokenIsNotACaller(t *testing.T) {
	workers := map[string]WorkerConfig{"triage": {URL: "http://a"}, "triage-shadow": {URL: "http://b", Shadow: true}, "ops": {URL: "http://c"}}
	tokens := map[string]string{"triage": "t-live", "triage-shadow": "t-shadow", "ops": "t-ops"}
	got := callerTokens(workers, tokens)
	if len(got) != 2 || got["triage"] != "t-live" || got["ops"] != "t-ops" || got["triage-shadow"] != "" {
		t.Fatalf("%v", got)
	}
	// and the callers the API makes from them never resolve the shadow worker's token
	for _, c := range api.WorkerCallers(got) {
		if c.TokenSHA256 == api.HashToken("t-shadow") {
			t.Fatalf("a shadow worker's token is a caller: %+v", c)
		}
	}
	if len(callerTokens(workers, nil)) != 0 {
		t.Fatal("no tokens, no callers")
	}
}

// S4: two different workers on one token are refused, by name; one worker listed under two names is one worker.
func TestWorkersThatShareATokenAreRefusedByName(t *testing.T) {
	workers := map[string]WorkerConfig{
		"triage":           {URL: "http://triage:8081"},
		"triage-candidate": {URL: "http://candidate:8081"},
		"bugfix":           {URL: "http://bugfix:8081"},
		"bugfix-fix":       {URL: "http://bugfix:8081"}, // the same worker under a second name
		"ops":              {URL: "http://ops:8081"},
	}
	own := map[string]string{"triage": "a", "triage-candidate": "b", "bugfix": "c", "bugfix-fix": "c", "ops": "d"}
	if err := sharedTokenError(workers, own); err != nil {
		t.Fatalf("distinct tokens, and one worker under two names: %v", err)
	}
	shared := map[string]string{"triage": "a", "triage-candidate": "a", "bugfix": "c", "bugfix-fix": "c", "ops": "d"}
	err := sharedTokenError(workers, shared)
	if err == nil || !strings.Contains(err.Error(), "workers triage and triage-candidate share a token file") || strings.Contains(err.Error(), "bugfix") {
		t.Fatalf("%v", err)
	}
	three := map[string]string{"triage": "a", "triage-candidate": "a", "ops": "a", "bugfix": "c", "bugfix-fix": "c"}
	if err := sharedTokenError(workers, three); err == nil || !strings.Contains(err.Error(), "ops and triage and triage-candidate share a token file") {
		t.Fatalf("%v", err)
	}
	two := map[string]string{"triage": "a", "triage-candidate": "a", "ops": "d", "bugfix": "x", "bugfix-fix": "x"}
	workers["bugfix-fix"] = WorkerConfig{URL: "http://other:8081"}
	if err := sharedTokenError(workers, two); err == nil || !strings.Contains(err.Error(), "workers bugfix and bugfix-fix; workers triage and triage-candidate share a token file") {
		t.Fatalf("every sharing pair is named: %v", err)
	}
}

func TestShadowsConfig(t *testing.T) {
	c, err := load(t, "shadows: {enabled: true, queue_max: 5, after_live: true}\nworkers:\n  triage-shadow: {url: 'http://s:8081', token_file: /t, shadow: true}\n")
	if err != nil {
		t.Fatal(err)
	}
	if !c.Shadows.Enabled || c.Shadows.QueueMax != 5 || !c.Shadows.AfterLive || !c.Workers["triage-shadow"].Shadow {
		t.Fatalf("%+v", c)
	}
	if c, err := load(t, ""); err != nil || c.Shadows.Enabled {
		t.Fatalf("shadows are off by default: %+v %v", c, err)
	}
	if _, err := load(t, "shadows: {enable: true}\n"); err == nil {
		t.Fatal("an unknown key under shadows is refused")
	}
}

func versioned(id string, worker string, versions ...int) *spec.Routine {
	j := &spec.Routine{Fleet: strings.Split(id, "/")[0], Routine: strings.Split(id, "/")[1], Worker: worker, Enabled: true}
	for _, n := range versions {
		j.Versions = append(j.Versions, &spec.Version{Number: n})
	}
	j.Version = versions[0]
	return j
}

func TestCheckShadows(t *testing.T) {
	cfg := &Config{Workers: map[string]WorkerConfig{"triage": {}, "triage-shadow": {Shadow: true}, "ops": {}}}
	cfg.Shadows.Enabled = true
	two := versioned("triage/cycle", "triage", 1, 2)
	one := versioned("ops/backup", "ops", 1)
	if errs := checkShadows(cfg, []*spec.Routine{two, one}); len(errs) != 0 {
		t.Fatalf("%v", errs)
	}
	// the shadow worker is not declared shadow: true
	cfg.Workers["triage-shadow"] = WorkerConfig{}
	if errs := checkShadows(cfg, []*spec.Routine{two}); len(errs) != 1 || !strings.Contains(errs[0].Error(), "triage/cycle has 2 versions, and its shadow worker triage-shadow is not a worker declared shadow: true") {
		t.Fatalf("%v", errs)
	}
	// a named shadow worker that does not exist
	two.ShadowWorker = "nowhere"
	if errs := checkShadows(cfg, []*spec.Routine{two}); len(errs) != 1 || !strings.Contains(errs[0].Error(), "nowhere") {
		t.Fatalf("%v", errs)
	}
	// a live routine on a shadow worker
	cfg.Workers["ops"] = WorkerConfig{Shadow: true}
	if errs := checkShadows(cfg, []*spec.Routine{one}); len(errs) != 1 || !strings.Contains(errs[0].Error(), "a shadow worker runs shadow versions only") {
		t.Fatalf("%v", errs)
	}
	// a remote shadow worker is not supported yet, whatever shadows say
	cfg2 := &Config{Workers: map[string]WorkerConfig{"w": {Shadow: true, Remote: "w-x-y"}}}
	if errs := checkShadows(cfg2, nil); len(errs) != 1 || !strings.Contains(errs[0].Error(), "remote") {
		t.Fatalf("%v", errs)
	}
	// a routine whose live worker is remote cannot be shadowed yet: its snapshot and its commit are read through the link, which has neither
	cfg3 := &Config{Workers: map[string]WorkerConfig{"bugfix": {Remote: "w-bugfix-m"}, "bugfix-shadow": {Shadow: true}}}
	cfg3.Shadows.Enabled = true
	if errs := checkShadows(cfg3, []*spec.Routine{versioned("bugfix/fix", "bugfix", 1, 2)}); len(errs) != 1 ||
		!strings.Contains(errs[0].Error(), "bugfix/fix has 2 versions and runs on the remote worker bugfix: a remote worker's routine cannot be shadowed yet") {
		t.Fatalf("%v", errs)
	}
	cfg3.Shadows.Enabled = false
	if errs := checkShadows(cfg3, []*spec.Routine{versioned("bugfix/fix", "bugfix", 1, 2)}); len(errs) != 0 {
		t.Fatalf("with shadows off a remote routine's versions are only listed: %v", errs)
	}
	// shadows off: a routine with versions needs nothing
	off := &Config{Workers: map[string]WorkerConfig{"triage": {}}}
	if errs := checkShadows(off, []*spec.Routine{two}); len(errs) != 0 {
		t.Fatalf("%v", errs)
	}
}

func TestShadowCheckLines(t *testing.T) {
	cfg := &Config{Workers: map[string]WorkerConfig{"triage-shadow": {Shadow: true}}}
	if got := shadowCheckLines(cfg, nil); len(got) != 1 || !strings.HasPrefix(got[0], "shadows: off") {
		t.Fatalf("%v", got)
	}
	cfg.Shadows.Enabled = true
	two := versioned("triage/cycle", "triage", 1, 2)
	two.Version = 1
	got := shadowCheckLines(cfg, []*spec.Routine{two, versioned("ops/b", "ops", 1)})
	if len(got) != 2 || !strings.Contains(got[0], "queue_max 20") || !strings.Contains(got[1], "triage/cycle v2 on worker triage-shadow") || !strings.Contains(got[1], "mode shadow") {
		t.Fatalf("%v", got)
	}
}
