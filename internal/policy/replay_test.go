package policy

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/chipi/flotilla/internal/registry"
)

// The replay of the routine-action ceiling change (AGENTS.md rule 3, the replay-proven-fix practice): with the option at its default,
// propose, every decision the registry's real actions can get is what it was before the change.
//
// testdata/default_ceiling_replay.jsonl is that "before": this corpus, run through policy.Decide at commit 6a2a43e (the last commit
// before the change, where State has one HostCeiling set to propose and no routine ceiling), one line per case. To regenerate it
// from git rather than re-implement the old code: check out that commit in a detached worktree, copy replay_test.go into
// internal/policy, replace the two ceiling assignments in defaultCeilings with `s.HostCeiling = "propose"`, and run
// `go test ./internal/policy -run TestDefaultCeilingReplay -args -replay-write`.
//
// What may differ: the "autonomy:" reason of a routine or repo action, which now names its own ceiling (or none) instead of the host's.
// Outcome, autonomy, what record would have done and the plan hash must not, for any row.

// defaultCeilings is the State a flotillad with no routine_actions block builds (internal/api planState).
func defaultCeilings(s *State) {
	s.HostExecutorCeiling = "propose"
	s.RoutineCeiling = "propose"
}

var replayWrite = flag.Bool("replay-write", false, "write the golden file instead of reading it")

type replayRow struct {
	Key      string   `json:"key"`
	Outcome  string   `json:"outcome"`
	Autonomy string   `json:"autonomy"`
	Would    string   `json:"would"`
	Hash     string   `json:"hash"`
	Reasons  []string `json:"reasons"`
}

func replayParams(a registry.Action) map[string]string {
	p := map[string]string{}
	for name, prm := range a.Params {
		switch prm.Kind {
		case "target_host":
			p[name] = "host-01"
		case "enum":
			p[name] = prm.Enum[0]
		default:
			p[name] = "x"
		}
	}
	return p
}

// replayCorpus is every real action x the levels each cap can take x the states that change a decision.
func replayCorpus(t *testing.T) map[string]replayRow {
	t.Helper()
	reg, err := registry.Load(filepath.Join("..", "..", "host", "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	levels := []string{"record", "propose", "auto"}
	out := map[string]replayRow{}
	for _, name := range reg.Actions() {
		a, _ := reg.Action(name)
		target := "host:host-01"
		if a.ByRoutine() {
			target = "x:thing/1"
		}
		for _, rt := range levels {
			for _, ea := range levels {
				for _, frozen := range []bool{false, true} {
					for _, tainted := range []bool{false, true} {
						for _, failed := range []int{0, 3} {
							for _, locked := range []bool{false, true} {
								p := Plan{CaseID: "case-1", RunID: "run-1", Action: name, Params: replayParams(a)}
								s := State{CaseKind: a.ForKinds[0], CaseTarget: target, Action: a, ActionFound: true, RunOnRecord: true,
									RoutineAutonomy: rt, ActionAutonomy: ea, Frozen: frozen, Tainted: tainted, FailedHour: failed}
								if locked {
									s.LockHeldBy = "case-0"
								}
								defaultCeilings(&s)
								d := Decide(p, s, Limits{})
								key := fmt.Sprintf("%s|routine=%s|earned=%s|frozen=%t|tainted=%t|failed=%d|locked=%t", name, rt, ea, frozen, tainted, failed, locked)
								out[key] = replayRow{Key: key, Outcome: d.Outcome, Autonomy: d.Autonomy, Would: d.Would, Hash: d.PlanHash, Reasons: d.Reasons}
							}
						}
					}
				}
			}
		}
	}
	return out
}

func TestDefaultCeilingReplay(t *testing.T) {
	now := replayCorpus(t)
	golden := filepath.Join("testdata", "default_ceiling_replay.jsonl")
	if *replayWrite {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		f, err := os.Create(golden)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		keys := make([]string, 0, len(now))
		for k := range now {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			b, _ := json.Marshal(now[k])
			fmt.Fprintln(f, string(b))
		}
		return
	}
	f, err := os.Open(golden)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	reg, err := registry.Load(filepath.Join("..", "..", "host", "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	rows, hostRows := 0, 0
	for sc.Scan() {
		var want replayRow
		if err := json.Unmarshal(sc.Bytes(), &want); err != nil {
			t.Fatal(err)
		}
		rows++
		got, ok := now[want.Key]
		if !ok {
			t.Errorf("%s: recorded, but the corpus no longer has it (an action changed: regenerate from git as the header says)", want.Key)
			continue
		}
		if got.Outcome != want.Outcome || got.Autonomy != want.Autonomy || got.Would != want.Would || got.Hash != want.Hash {
			t.Errorf("%s: now %s/%s/%q, was %s/%s/%q", want.Key, got.Outcome, got.Autonomy, got.Would, want.Outcome, want.Autonomy, want.Would)
		}
		a := want.Key[:strings.Index(want.Key, "|")]
		act, _ := reg.Action(a)
		if !act.ByRoutine() {
			hostRows++
			if strings.Join(got.Reasons, "\n") != strings.Join(want.Reasons, "\n") {
				t.Errorf("%s: a host action's reasons changed:\n now %q\n was %q", want.Key, got.Reasons, want.Reasons)
			}
			continue
		}
		if strip(got.Reasons) != strip(want.Reasons) {
			t.Errorf("%s: reasons other than the autonomy line changed:\n now %q\n was %q", want.Key, got.Reasons, want.Reasons)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if rows < 1000 || hostRows == 0 || hostRows == rows {
		t.Fatalf("the replay ran %d rows (%d of host actions): the golden file is not the corpus", rows, hostRows)
	}
}

func strip(rs []string) string {
	var out []string
	for _, r := range rs {
		if !strings.HasPrefix(r, "autonomy: ") {
			out = append(out, r)
		}
	}
	return strings.Join(out, "\n")
}
