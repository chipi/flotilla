package main

import (
	"os"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The compose file's promises about the shadow worker (docs/wip/v2-design.md section 4), checked on the file itself: a shadow cannot starve
// a live run, reads no live secret, writes no live state, and reaches the network through a proxy of its own.

type composeService struct {
	Image       string            `yaml:"image"`
	Profiles    []string          `yaml:"profiles"`
	MemLimit    string            `yaml:"mem_limit"`
	CPUs        float64           `yaml:"cpus"`
	CPUShares   int               `yaml:"cpu_shares"`
	Environment map[string]string `yaml:"environment"`
	Volumes     []string          `yaml:"volumes"`
	Networks    []string          `yaml:"networks"`
	DependsOn   map[string]struct {
		Condition string `yaml:"condition"`
		Required  *bool  `yaml:"required"`
	} `yaml:"depends_on"`
}

type composeVolume struct {
	Name     string `yaml:"name"`
	External bool   `yaml:"external"`
}

func loadComposeFile(t *testing.T) (map[string]composeService, map[string]composeVolume) {
	t.Helper()
	raw, err := os.ReadFile("../../deploy/compose/docker-compose.yml")
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Services map[string]composeService `yaml:"services"`
		Volumes  map[string]composeVolume  `yaml:"volumes"`
	}
	if err := yaml.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	return f.Services, f.Volumes
}

func loadCompose(t *testing.T) map[string]composeService {
	s, _ := loadComposeFile(t)
	return s
}

// A secrets volume is filled by deploy/server/secrets.sh before compose runs and is never created by compose: an unmarked one would be made
// empty by compose, owned by root, and a worker would start without its token (worker-triage-secrets lost the mark once, between two commits).
func TestEverySecretsVolumeIsExternal(t *testing.T) {
	_, vols := loadComposeFile(t)
	n := 0
	for name, v := range vols {
		if strings.HasSuffix(name, "-secrets") {
			n++
			if !v.External {
				t.Errorf("volume %s is not external: true", name)
			}
		}
	}
	if n < 8 {
		t.Fatalf("only %d secrets volumes found", n)
	}
}

func memBytes(t *testing.T, s string) int {
	t.Helper()
	mult := map[byte]int{'m': 1 << 20, 'g': 1 << 30}[s[len(s)-1]]
	n, err := strconv.Atoi(s[:len(s)-1])
	if err != nil || mult == 0 {
		t.Fatalf("mem_limit %q", s)
	}
	return n * mult
}

func volumeNames(s composeService) (names []string, mode map[string]string) {
	mode = map[string]string{}
	for _, v := range s.Volumes {
		parts := strings.Split(v, ":")
		if len(parts) < 2 {
			continue
		}
		names = append(names, parts[0])
		if len(parts) > 2 {
			mode[parts[0]] = parts[len(parts)-1]
		}
	}
	return
}

func TestTheShadowWorkerCannotStarveOrReachTheLiveWorker(t *testing.T) {
	svc := loadCompose(t)
	live, shadow, egress, egressShadow := svc["worker-triage"], svc["worker-triage-shadow"], svc["egress"], svc["egress-shadow"]
	if shadow.Image == "" || egressShadow.Image == "" {
		t.Fatal("the compose file has no worker-triage-shadow or egress-shadow")
	}
	if len(shadow.Profiles) != 1 || shadow.Profiles[0] != "shadow" || len(egressShadow.Profiles) != 1 || egressShadow.Profiles[0] != "shadow" {
		t.Errorf("the shadow worker and its proxy are off unless the shadow profile is on: %v %v", shadow.Profiles, egressShadow.Profiles)
	}
	if memBytes(t, shadow.MemLimit) >= memBytes(t, live.MemLimit) || shadow.CPUs >= live.CPUs {
		t.Errorf("the shadow worker's limits (%s, %v cpus) are not below the live worker's (%s, %v)", shadow.MemLimit, shadow.CPUs, live.MemLimit, live.CPUs)
	}
	if shadow.CPUShares == 0 || shadow.CPUShares >= 1024 {
		t.Errorf("cpu_shares %d: below the default 1024, so a busy host serves the live worker first", shadow.CPUShares)
	}
	if shadow.Environment["FLOTILLA_WORKER_MODE"] != "shadow" {
		t.Error("the shadow worker does not run in shadow mode")
	}
	for _, k := range []string{"FLOTILLA_ENV_FILES", "FLOTILLA_WRITE_ENV_FILES"} {
		if _, ok := shadow.Environment[k]; ok {
			t.Errorf("the shadow worker names %s: it may read credentials that read, never one that writes", k)
		}
	}
	if shadow.Environment["FLOTILLA_READ_ENV_FILES"] == "" {
		t.Error("the shadow worker names no read environment")
	}
	if !strings.Contains(shadow.Environment["ROUTINE_HTTPS_PROXY"], "egress-shadow") || !strings.Contains(shadow.Environment["ROUTINE_HTTP_PROXY"], "egress-shadow") {
		t.Errorf("the shadow worker reaches the network through its own proxy: %v", shadow.Environment)
	}
	liveVols, _ := volumeNames(live)
	shadowVols, mode := volumeNames(shadow)
	for _, v := range shadowVols {
		for _, l := range liveVols {
			if v == l && v != "triage-snapshots" {
				t.Errorf("the shadow worker mounts %s, which the live worker mounts too", v)
			}
		}
	}
	for _, v := range shadowVols {
		if strings.HasSuffix(v, "-secrets") && v != "worker-triage-shadow-secrets" {
			t.Errorf("the shadow worker mounts the secrets volume %s", v)
		}
	}
	if mode["triage-snapshots"] != "ro" {
		t.Errorf("the shadow worker mounts the live snapshots %q: read-only", mode["triage-snapshots"])
	}
	if m := map[string]bool{}; true {
		liveV, liveMode := volumeNames(live)
		for _, v := range liveV {
			m[v] = true
		}
		if !m["triage-snapshots"] || liveMode["triage-snapshots"] == "ro" {
			t.Error("the live worker writes the snapshots volume")
		}
	}
	if len(shadow.Networks) != 1 || shadow.Networks[0] != "shadow" {
		t.Errorf("the shadow worker is on its own internal network only, not the live one: %v", shadow.Networks)
	}
	for _, n := range egressShadow.Networks {
		if n == "flotilla" {
			t.Errorf("the shadow proxy is on the live network: %v", egressShadow.Networks)
		}
	}
	for name, s := range svc {
		on := false
		for _, n := range s.Networks {
			on = on || n == "shadow"
		}
		if on && name != "worker-triage-shadow" && name != "egress-shadow" && name != "flotillad" {
			t.Errorf("%s is on the shadow network: only flotillad (to call the worker and serve its ledger), the shadow worker and its proxy are", name)
		}
	}
	if !strings.Contains(strings.Join(svc["flotillad"].Networks, " "), "shadow") {
		t.Error("flotillad is not on the shadow network, so it cannot call the shadow worker")
	}
	if shadow.Environment["FLOTILLA_WRITE_KEYS"] == "" {
		t.Error("the shadow worker names no write keys: the worker's refusal rests on a built-in list alone")
	}
	if memBytes(t, egressShadow.MemLimit) > memBytes(t, egress.MemLimit) {
		t.Errorf("the shadow proxy is larger than the live one")
	}
	acl := ""
	for _, v := range egressShadow.Volumes {
		if strings.Contains(v, "acl.yaml") {
			acl = v
		}
	}
	if !strings.Contains(acl, "egress-shadow-acl.yaml") {
		t.Errorf("the shadow proxy has its own allowlist: %q", acl)
	}
	if !strings.Contains(svc["flotillad"].Environment["NO_PROXY"], "worker-triage-shadow") {
		t.Error("flotillad reaches the shadow worker directly, not through its proxy")
	}
	// a shadow's decisions reach the pairs only through flotillad's ledger: with the default (a TSV in its own volume) it decided and
	// paired nothing (2026-11-21, the first shadow run of the installation's triage/cycle v2: 10 decided, 0 pairs)
	if l, s := live.Environment["ROUTINE_SF_LEDGER_BACKEND"], shadow.Environment["ROUTINE_SF_LEDGER_BACKEND"]; l == "" || s != l {
		t.Errorf("the shadow worker's ledger backend %q is not the live worker's %q", s, l)
	}
	// the shadow proxy reaches every outside service the live one reads, but not Langfuse: pushing a trace is a write, and a shadow writes
	// nothing (the e2e's no-effect section asserts the same on the running stack)
	onShadow := map[string]bool{}
	for _, n := range egressShadow.Networks {
		onShadow[n] = true
	}
	for _, n := range egress.Networks {
		if n != "flotilla" && n != "langfuse" && !onShadow[n] {
			t.Errorf("the shadow proxy is not on %q, which the live proxy is on", n)
		}
	}
	if onShadow["langfuse"] {
		t.Error("the shadow proxy is on langfuse: a shadow's trace push would be a write")
	}
}

// Only the workers whose routines call flotillad's workers' listener have its address, and none has a worker-token fallback for it: under
// run_tokens: required (the default) a routine reaches the listener with its run's own token alone. The bugfix, bugfix-act and marketing
// workers run no routine that reads it (pi's tools, steered by an untrusted issue, would inherit the address), and a remote worker's compose
// file (deploy/remote) sets none either; the run token its routine is given sits in a file nothing there reads.
func TestOnlyTheListenersCallersHaveItsAddressAndNoneHasAWorkerTokenFallback(t *testing.T) {
	services := loadCompose(t)
	want := map[string]bool{"worker-triage": true, "worker-triage-candidate": true, "worker-triage-shadow": true, "worker-ops-read": true, "worker-ops-act-host": true}
	for name, s := range services {
		_, has := s.Environment["ROUTINE_FLOTILLA_API_URL"]
		if has != want[name] {
			t.Errorf("%s: ROUTINE_FLOTILLA_API_URL set=%v, want %v", name, has, want[name])
		}
		if _, ok := s.Environment["ROUTINE_FLOTILLA_API_TOKEN_FILE"]; ok {
			t.Errorf("%s: ROUTINE_FLOTILLA_API_TOKEN_FILE is the worker-token fallback, which run_tokens: required refuses", name)
		}
	}
	raw, err := os.ReadFile("../../deploy/remote/docker-compose.yml")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "FLOTILLA_API") {
		t.Error("deploy/remote/docker-compose.yml sets a FLOTILLA_API_* variable: a remote worker's routines do not call flotillad's workers' listener")
	}
}

// flotillad's start must not race a worker that is still the previous tag's: it waits for every worker the compose file defines, the ones of an
// optional profile with required: false (a host without the profile is left alone). The candidate was missing, so a start could meet an old
// candidate that run_tokens: required refuses.
func TestFlotilladWaitsForEveryWorkerItMayName(t *testing.T) {
	services := loadCompose(t)
	deps := services["flotillad"].DependsOn
	for name, s := range services {
		if !strings.HasPrefix(name, "worker-") || strings.HasSuffix(name, "-shadow") {
			continue
		}
		d, ok := deps[name]
		if !ok {
			t.Errorf("flotillad does not depend_on %s: its start could meet that worker still old", name)
			continue
		}
		if d.Condition != "service_healthy" {
			t.Errorf("flotillad depends on %s with condition %q, want service_healthy", name, d.Condition)
		}
		if len(s.Profiles) > 0 && (d.Required == nil || *d.Required) {
			t.Errorf("%s is in profile %v, so flotillad's dependency on it needs required: false", name, s.Profiles)
		}
	}
}
