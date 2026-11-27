package spec

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// RFC-0017: ranked-watch@1, the performance watch, and its example flow.

func TestRankedWatchIsAMonitorWithNoModel(t *testing.T) {
	lib, err := LoadLibrary(repoBlocks)
	if err != nil {
		t.Fatal(err)
	}
	b := lib[BlockRef{"ranked-watch", 1}]
	if b == nil {
		t.Fatal("ranked-watch@1 is not in the library")
	}
	if b.Role != RoleMonitor || b.Model != "none" || b.MaxAutonomy != AutonomyRecord || b.BriefSection != "Performance" {
		t.Fatalf("%+v", b)
	}
	for _, c := range b.Code {
		if _, err := os.Stat(filepath.Join("..", "..", filepath.FromSlash(c))); err != nil {
			t.Errorf("names code %s, which the repo does not have: %v", c, err)
		}
	}
	for _, p := range []string{"source", "list", "key", "cost", "top"} {
		if q, _ := b.ParamByName(p); q.Optional {
			t.Errorf("%s is required", p)
		}
	}
	for _, p := range []string{"rise", "signals", "fixes", "unit"} {
		if q, ok := b.ParamByName(p); !ok || !q.Optional {
			t.Errorf("%s is optional", p)
		}
	}
}

func TestWatchParamKinds(t *testing.T) {
	signals := func(m map[string]any) any { return m }
	ids := func(s ...string) []any {
		var l []any
		for _, x := range s {
			l = append(l, x)
		}
		return l
	}
	cases := []struct {
		name, kind string
		v          any
		want       string // "" a refusal
	}{
		{"source", KindSourceName, "ci_stats", "ci_stats"},
		{"source is a name, not a URL", KindSourceName, "https://ci.example.com/ci.json", ""},
		{"source upper case", KindSourceName, "CiStats", ""},
		{"source not text", KindSourceName, 3, ""},
		{"path", KindJSONPath, "steps.7d", "steps.7d"},
		{"path one segment", KindJSONPath, "rows", "rows"},
		{"path with a slash", KindJSONPath, "steps/7d", ""},
		{"path with an empty segment", KindJSONPath, "steps..7d", ""},
		{"path with a glob", KindJSONPath, "steps.*", ""},
		{"field", KindField, "lost", "lost"},
		{"field with a dot", KindField, "x3.med", ""},
		{"rise", KindRiseRule, map[string]any{"ratio": 0.5, "at_least": 600}, "0.5:600"},
		{"rise ratio a whole number", KindRiseRule, map[string]any{"ratio": 1, "at_least": 300}, "1:300"},
		{"rise float ratio 1.0", KindRiseRule, map[string]any{"ratio": 1.0, "at_least": 300}, "1:300"},
		{"rise missing at_least", KindRiseRule, map[string]any{"ratio": 0.5}, ""},
		{"rise extra key", KindRiseRule, map[string]any{"ratio": 0.5, "at_least": 1, "x": 1}, ""},
		{"rise zero ratio", KindRiseRule, map[string]any{"ratio": 0, "at_least": 1}, ""},
		{"rise negative minimum", KindRiseRule, map[string]any{"ratio": 0.5, "at_least": -1}, ""},
		{"rise text", KindRiseRule, "50%", ""},
		{"signals with a glob are quoted", KindSignals, signals(map[string]any{"source": "slo_stats", "fresh": "ci-collector--fresh", "ids": ids("ci-runners--*")}),
			"'slo_stats:ci-collector--fresh:ci-runners--*'"},
		{"signals ids joined", KindSignals, signals(map[string]any{"source": "slo_stats", "ids": ids("a-1", "b-2")}), "slo_stats::a-1,b-2"},
		{"signals fresh only", KindSignals, signals(map[string]any{"source": "slo_stats", "fresh": "c-1"}), "slo_stats:c-1"},
		{"signals need a source", KindSignals, signals(map[string]any{"ids": ids("a")}), ""},
		{"signals need ids or fresh", KindSignals, signals(map[string]any{"source": "slo_stats"}), ""},
		{"signals source is a name", KindSignals, signals(map[string]any{"source": "https://x.example.com", "ids": ids("a")}), ""},
		{"signals unknown key", KindSignals, signals(map[string]any{"source": "s", "ids": ids("a"), "x": 1}), ""},
		{"signals id with a separator", KindSignals, signals(map[string]any{"source": "s", "ids": ids("a,b")}), ""},
		{"signals id with a shell character", KindSignals, signals(map[string]any{"source": "s", "ids": ids("a;rm")}), ""},
		{"signals fresh may not be a glob", KindSignals, signals(map[string]any{"source": "s", "fresh": "a-*"}), ""},
		{"signals ids a list", KindSignals, signals(map[string]any{"source": "s", "ids": "a"}), ""},
		{"unit seconds", KindUnit, "seconds", "seconds"},
		{"unit unknown", KindUnit, "parsecs", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := renderParam(Param{Name: "p", Kind: c.kind, Env: "P"}, c.v, nil)
			if c.want == "" {
				if err == nil {
					t.Fatalf("want a refusal, got %q", got)
				}
				return
			}
			if err != nil || got != c.want {
				t.Fatalf("got %q, %v; want %q", got, err, c.want)
			}
		})
	}
}

func TestBriefSectionIsCheckedOnTheBlock(t *testing.T) {
	good := strings.Replace(goodBlock, "max_autonomy: record", "max_autonomy: record\nbrief_section: Performance", 1)
	root := t.TempDir()
	put(t, root, "probe/v1/block.yaml", good)
	b, err := LoadBlock(filepath.Join(root, "probe", "v1"))
	if err != nil || b.BriefSection != "Performance" {
		t.Fatalf("%v %+v", err, b)
	}
	for name, c := range map[string]struct{ edit, want string }{
		"lower case":    {"brief_section: performance", "brief_section"},
		"reserved name": {"brief_section: Watch", "own sections"},
		"too long":      {"brief_section: " + strings.Repeat("A", 40), "brief_section"},
	} {
		t.Run(name, func(t *testing.T) {
			r := t.TempDir()
			put(t, r, "probe/v1/block.yaml", strings.Replace(good, "brief_section: Performance", c.edit, 1))
			if _, err := LoadBlock(filepath.Join(r, "probe", "v1")); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want %q, got %v", c.want, err)
			}
		})
	}
}

// TestPerfFlowIsByteEqualToItsFolders is the generator proof for the example CI runners flow: the committed routine
// folders are what the flow expands to, with the brief's section and the knobs of RFC-0017 section 4 on their commands.
func TestPerfFlowIsByteEqualToItsFolders(t *testing.T) {
	fleetDir := filepath.Join("..", "..", "fleets", "perf")
	fe, err := ExpandFleet(fleetDir, repoBlocks, actionsStub)
	if err != nil || fe == nil {
		t.Fatalf("%v %v", fe, err)
	}
	if len(fe.Exp.Steps) != 2 {
		t.Fatalf("%d steps", len(fe.Exp.Steps))
	}
	if d := fe.Exp.Drift(fleetDir); len(d) != 0 {
		t.Fatalf("the committed folders differ from the flow's expansion: %v", d)
	}
	week, err := os.ReadFile(filepath.Join(fleetDir, "ci-perf-watch", "routine.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"detects: [perf_regression]", "brief_section: Performance", "every: 168h", "autonomy: record", "worker: ops-read"} {
		if !strings.Contains(string(week), want) {
			t.Errorf("ci-perf-watch/routine.yaml has no %q:\n%s", want, week)
		}
	}
	if strings.Contains(string(week), "max_autonomy") {
		t.Error("a monitor never plans: no cap line unless the step writes one")
	}
	spec, err := os.ReadFile(filepath.Join(fleetDir, "ci-perf-watch", "v1", "spec.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"RW_SOURCE=ci_stats", "RW_LIST=steps.7d", "RW_KEY=id", "RW_COST=lost", "RW_TOP=15", "RW_RISE=0.5:600",
		"RW_SIGNALS='slo_stats:", ":ci-runners--*'", "RW_FIXES=knowledge/fixes.yaml", "python3 /opt/flotilla/ops/ranked_watch.py --fleet perf"} {
		if !strings.Contains(string(spec), want) {
			t.Errorf("ci-perf-watch's command has no %q:\n%s", want, spec)
		}
	}
	day, err := os.ReadFile(filepath.Join(fleetDir, "ci-perf-daily", "v1", "spec.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"RW_LIST=steps.yesterday", "RW_TOP=5", "RW_RISE=1:300"} {
		if !strings.Contains(string(day), want) {
			t.Errorf("ci-perf-daily's command has no %q:\n%s", want, day)
		}
	}
}

// TestPerfFleetLoads: the fleet is an ordinary fleet to the loader, and what it declares is what the flow wrote.
func TestPerfFleetLoads(t *testing.T) {
	rs, err := Load(filepath.Join("..", "..", "fleets"))
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]*Routine{}
	for _, r := range rs {
		ids[r.ID()] = r
	}
	for _, id := range []string{"perf/ci-perf-watch", "perf/ci-perf-daily"} {
		r := ids[id]
		if r == nil {
			t.Fatalf("%s is not loaded", id)
		}
		if r.Autonomy != AutonomyRecord || r.BriefSection != "Performance" || len(r.Detects) != 1 || r.Detects[0] != "perf_regression" {
			t.Errorf("%s: %+v", id, r)
		}
	}
}
