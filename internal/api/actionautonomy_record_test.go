package api

import (
	"context"
	"testing"

	"github.com/chipi/flotilla/internal/spec"
)

// A record-only action (RFC-0016 §12) can never be promoted: nothing carries it out, so propose and auto are refused.
func TestRecordOnlyActionIsNeverPromoted(t *testing.T) {
	ctx := context.Background()
	s := newServer(t, func(*spec.Routine) {})
	op := Caller{Name: "operator", Role: RoleOperator}
	for _, lvl := range []string{"propose", "auto"} {
		err := s.SetActionAutonomy(ctx, op, "api", "disk.cleanup_known", lvl, "the record shows nothing")
		if p := AsProblem(err); p == nil || p.Code != 400 {
			t.Errorf("promoting a record-only action to %s: %v", lvl, err)
		}
	}
	as, _ := s.Actions(ctx)
	for _, a := range as {
		if a.Name == "disk.cleanup_known" && (a.Max != "record" || a.Autonomy != "record") {
			t.Errorf("disk.cleanup_known: %+v", a)
		}
	}
}
