package registry

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func repoFile(t *testing.T, rel string) string {
	_, here, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(here), "..", "..", rel)
}

// The declaration flotillad's image carries: RFC-0004 §3.4's entry for v1's one action.
func TestTheHostAgentsDeclaration(t *testing.T) {
	r, err := Load(repoFile(t, "host/registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	a, ok := r.Action("backup.rerun_observability")
	if !ok {
		t.Fatal("v1's action is not declared")
	}
	if !a.For("failed_backup") || a.For("memory_growth") || a.Risk != "low" || a.Reversible != "regenerable" ||
		time.Duration(a.Check.Within) != time.Hour || a.Limits.PerTargetPerDay != 2 || a.Params["host"].Kind != "target_host" ||
		strings.Join(a.Pre, ",") != "backup_age_above(26h),not_running" || strings.Join(a.PreShadow, ",") != "backup_age_above(20h),not_running" {
		t.Fatalf("backup.rerun_observability: %+v", a)
	}
}

func write(t *testing.T, body string) string {
	p := filepath.Join(t.TempDir(), "registry.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const good = `{"name": "x.y", "for_kinds": ["k"], "params": {"host": {"kind": "target_host"}}, "risk": "low",
  "reversible": "yes", "pre": ["a(1h)"], "check": {"verify": "v", "within": "5m"}, "limits": {"per_target_per_day": 1}, "timeout": "1m"}`

func TestBadDeclarationsAreRefused(t *testing.T) {
	if _, err := Load(write(t, `{"component": "c", "actions": [`+good+`]}`)); err != nil {
		t.Fatalf("the good one: %v", err)
	}
	for name, c := range map[string]struct{ from, to string }{
		"a name that is one word":   {`"name": "x.y"`, `"name": "rerun"`},
		"no case kinds":             {`"for_kinds": ["k"]`, `"for_kinds": []`},
		"a param of no kind":        {`{"kind": "target_host"}`, `{"kind": "string"}`},
		"an enum without values":    {`{"kind": "target_host"}`, `{"kind": "enum"}`},
		"risk out of its set":       {`"risk": "low"`, `"risk": "tiny"`},
		"reversible out of its set": {`"reversible": "yes"`, `"reversible": "mostly"`},
		"no preconditions":          {`"pre": ["a(1h)"]`, `"pre": []`},
		"a precondition with space": {`"pre": ["a(1h)"]`, `"pre": ["rm -rf"]`},
		"no check window":           {`"within": "5m"`, `"within": "0s"`},
		"no daily limit":            {`"per_target_per_day": 1`, `"per_target_per_day": 0`},
		"no timeout":                {`"timeout": "1m"`, `"timeout": "0s"`},
		"a duration as a number":    {`"timeout": "1m"`, `"timeout": 60`},
		"an unknown field":          {`"risk": "low"`, `"risk": "low", "approve": "always"`},
	} {
		body := `{"component": "c", "actions": [` + strings.Replace(good, c.from, c.to, 1) + `]}`
		if strings.Count(body, c.to) == 0 {
			t.Fatalf("%s: the test's edit did not apply", name)
		}
		if _, err := Load(write(t, body)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := Load(write(t, `{"actions": [`+good+`]}`)); err == nil {
		t.Error("no component: accepted")
	}
	a := write(t, `{"component": "c", "actions": [`+good+`]}`)
	b := filepath.Join(t.TempDir(), "other.json")
	_ = os.WriteFile(b, []byte(`{"component": "d", "actions": [`+good+`]}`), 0o600)
	if _, err := Load(a, b); err == nil || !strings.Contains(err.Error(), "already declared") {
		t.Errorf("one action in two files: %v", err)
	}
}
