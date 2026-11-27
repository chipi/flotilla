package registry

import (
	"strings"
	"testing"
	"time"
)

// A record-only action (RFC-0016 §12): it names a fix and holds the threshold (pre) and the check window, and nothing
// carries it out. It is the smallest declaration of "a cleanup that does not run yet".
const recordOnly = `{"name": "disk.cleanup_known", "executor": "record", "for_kinds": ["disk_low"],
  "params": {"host": {"kind": "target_host"}}, "risk": "low", "reversible": "yes", "pre": ["free_ratio_below(0.15)"],
  "check": {"verify": "disk_free_above_threshold", "within": "60m"}, "limits": {"per_target_per_day": 1}, "timeout": "30m"}`

func TestRecordOnlyActions(t *testing.T) {
	r, err := Load(write(t, `{"component": "c", "actions": [`+recordOnly+`]}`))
	if err != nil {
		t.Fatal(err)
	}
	a, _ := r.Action("disk.cleanup_known")
	if !a.RecordOnly() || a.ByRoutine() || a.Repo() || a.MaxAutonomy() != "record" {
		t.Fatalf("record-only %v, by routine %v, repo %v, max %s", a.RecordOnly(), a.ByRoutine(), a.Repo(), a.MaxAutonomy())
	}
	for name, c := range map[string]struct{ from, to string }{
		"no precondition (it holds the threshold)": {`"pre": ["free_ratio_below(0.15)"]`, `"pre": []`},
		"no check window":                          {`"within": "60m"`, `"within": "0s"`},
		"no check name":                            {`"verify": "disk_free_above_threshold"`, `"verify": ""`},
		"a text param (that is a routine's)":       {`{"kind": "target_host"}`, `{"kind": "text", "max": 10}`},
	} {
		body := `{"component": "c", "actions": [` + strings.Replace(recordOnly, c.from, c.to, 1) + `]}`
		if body == `{"component": "c", "actions": [`+recordOnly+`]}` {
			t.Fatalf("%s: the test's edit did not apply", name)
		}
		if _, err := Load(write(t, body)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// The example declaration the disk flow reads: its threshold and its window are the registry's, one home (RFC-0016 §3).
func TestTheDiskActionIsRecordOnly(t *testing.T) {
	r, err := Load(repoFile(t, "host/registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	a, ok := r.Action("disk.cleanup_known")
	if !ok {
		t.Fatal("disk.cleanup_known is not declared")
	}
	if !a.RecordOnly() || !a.For("disk_low") || strings.Join(a.Pre, ",") != "free_ratio_below(0.15)" ||
		a.Check.Verify != "disk_free_above_threshold" || time.Duration(a.Check.Within) != time.Hour {
		t.Fatalf("disk.cleanup_known: %+v", a)
	}
}
