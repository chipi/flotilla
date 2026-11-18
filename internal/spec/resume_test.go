package spec

import (
	"strings"
	"testing"
)

// ADR-0019: resume is declared on a routine that acts on a case, with the statuses it covers and a bound.
func TestResume(t *testing.T) {
	onCase := func(resume string) string {
		return strings.NewReplacer("fleet: triage", "fleet: ops", "routine: cycle", "routine: fix",
			"trigger: {every: 10m}", "trigger: {case: {kind: code_bug}}", "freshness: 35m\n", resume).Replace(good)
	}
	d := t.TempDir()
	write(t, d, "ops/fix.yaml", onCase("resume: {on: [interrupted, error], max: 1}\n"))
	routines, err := Load(d)
	if err != nil {
		t.Fatal(err)
	}
	r := routines[0].Resume
	if r == nil || r.Max != 1 || !r.ResumesOn("interrupted") || !r.ResumesOn("error") || r.ResumesOn("timeout") || r.ResumesOn("ok") {
		t.Fatalf("resume %+v", r)
	}
	if (*Resume)(nil).ResumesOn("error") {
		t.Fatal("no resume declared resumes nothing")
	}
	for name, c := range map[string]struct{ spec, want string }{
		"timeout is not a resume status": {onCase("resume: {on: [timeout], max: 1}\n"), "not interrupted or error"},
		"no statuses":                    {onCase("resume: {on: [], max: 1}\n"), "at least one"},
		"a status twice":                 {onCase("resume: {on: [error, error], max: 1}\n"), "listed twice"},
		"no max":                         {onCase("resume: {on: [error]}\n"), "resume.max 0"},
		"too many":                       {onCase("resume: {on: [error], max: 9}\n"), "resume.max 9"},
		"a timed routine":                {good + "resume: {on: [error], max: 1}\n", "acts on a case"},
		"an unknown field":               {onCase("resume: {on: [error], max: 1, always: true}\n"), "always"},
	} {
		t.Run(name, func(t *testing.T) {
			d := t.TempDir()
			write(t, d, "ops/fix.yaml", c.spec)
			if _, err := Load(d); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want %q, got %v", c.want, err)
			}
		})
	}
}
