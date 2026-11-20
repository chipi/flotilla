package versions

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/chipi/flotilla/internal/spec"
)

// shippedPlatform is a fleets folder holding the repo's own platform fleet, as the loader wants one.
func shippedPlatform(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	if err := os.CopyFS(filepath.Join(d, "platform"), os.DirFS(filepath.Join("..", "..", "fleets", "platform"))); err != nil {
		t.Fatal(err)
	}
	return d
}

// platform/heartbeat moved from a flat spec to the folder layout with its v1 identical (step 5 added a v2 beside it): testdata/heartbeat_flat.yaml
// is the flat file as it was, and every field of the routine, version-owned or not, is the same through both.
func TestHeartbeatV1IsIdenticalInTheFolderLayout(t *testing.T) {
	flatDir := t.TempDir()
	raw, err := os.ReadFile(filepath.Join("testdata", "heartbeat_flat.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	write(t, flatDir, "platform/heartbeat.yaml", string(raw))
	flat, err := spec.Load(flatDir)
	if err != nil || len(flat) != 1 {
		t.Fatalf("flat: %v %v", flat, err)
	}
	folder, err := spec.Load(shippedPlatform(t))
	if err != nil {
		t.Fatal(err)
	}
	var hb *spec.Routine
	for _, j := range folder {
		if j.ID() == "platform/heartbeat" {
			hb = j
		}
	}
	if hb == nil || hb.Versions[0].Legacy || len(hb.Versions) != 2 || hb.Version != 1 || hb.Versions[1].Number != 2 {
		t.Fatalf("the shipped heartbeat is not a folder routine with v1 and the v2 step 5 added: %+v", hb)
	}
	a, b := *flat[0], *hb
	for _, c := range []*spec.Routine{&a, &b} { // what the loader fills about where and how it was read, not what the spec says
		c.Path, c.Hash, c.VersionHash, c.Versions, c.Notes = "", "", "", nil, nil
	}
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("the folder heartbeat is not the flat one:\nflat   %+v\nfolder %+v", a, b)
	}
}

// A store that recorded the flat heartbeat's v1 (every store up to step 5) accepts the folder layout: the shipped v1 is provably the same
// (each recorded key and file equal), so the start re-records it and the event says so. This is the one routine shipped both ways: real data
// for the proof, not a fixture.
func TestStoreThatRecordedTheFlatHeartbeatReRecordsTheFolderLayout(t *testing.T) {
	ctx := context.Background()
	st := open(t)
	raw, err := os.ReadFile(filepath.Join("testdata", "heartbeat_flat.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	flatDir := t.TempDir()
	write(t, flatDir, "platform/heartbeat.yaml", string(raw))
	flat, err := spec.LoadVersions(flatDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := Reconcile(ctx, st, flat); err != nil {
		t.Fatal(err)
	}
	folder, err := spec.LoadVersions(shippedPlatform(t))
	if err != nil {
		t.Fatal(err)
	}
	var hb []*spec.Routine
	for _, j := range folder {
		if j.ID() == "platform/heartbeat" {
			hb = append(hb, j)
		}
	}
	stored, _ := st.AllVersions(ctx)
	plan, err := Check(hb, stored)
	if err != nil || len(plan.Updates) != 1 || !strings.HasPrefix(plan.Updates[0].Line(), "platform/heartbeat v1 moved to the folder layout, unchanged (run") {
		t.Fatalf("the shipped folder was not proven the flat v1: %v %+v", err, plan.Updates)
	}
	if _, _, err := Reconcile(ctx, st, hb); err != nil {
		t.Fatal(err)
	}
	live, ok, _ := st.LiveVersion(ctx, "platform/heartbeat")
	if !ok || live.Hash != hb[0].Versions[0].Hash || live.Hash == flat[0].Versions[0].Hash {
		t.Fatalf("v1 was not re-recorded from the folder: %+v", live)
	}
}
