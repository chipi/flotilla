package api

import (
	"context"
	"testing"

	"github.com/chipi/flotilla/internal/spec"
)

// Staleness is for routines on time. A routine on cases runs when there is work, so it is never stale;
// one on time that never succeeded is.
func TestStaleOnlyForRoutinesOnTime(t *testing.T) {
	onTime := newServer(t, func(*spec.Routine) {})
	onTime.Sched.Routines[0].Fresh = onTime.Sched.Routines[0].Every * 3
	rs, _, _ := onTime.Status(context.Background())
	if !rs[0].Stale {
		t.Fatal("a routine on time that never succeeded is not stale")
	}
	onCases := newServer(t, func(j *spec.Routine) {
		j.Trigger.Case, j.Every, j.Fresh = &spec.CaseTrigger{Kind: "failed_backup"}, 0, 0
	})
	rs, _, _ = onCases.Status(context.Background())
	if rs[0].Stale {
		t.Fatal("a routine on cases shows stale")
	}
}
