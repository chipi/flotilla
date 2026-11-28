package spec

import (
	"os"
	"path/filepath"
	"testing"
)

var libraryNames = []string{"metric-monitor", "sop-act", "sop-investigate", "metric-verify", "sop-draft", "case-review", "process-change", "ranked-watch"}

// TestTheLibraryIsTheSevenBlocks pins RFC-0016 section 7: each block loads, its roles are the table's, and the code
// it names is in the repo (the workers carry it at the same paths, so no routine's command moves).
func TestTheLibraryIsTheSevenBlocks(t *testing.T) {
	lib, err := LoadLibrary(repoBlocks)
	if err != nil {
		t.Fatal(err)
	}
	roles := map[string]string{"metric-monitor": RoleMonitor, "sop-act": RoleAct, "sop-investigate": RoleStage, "metric-verify": RoleVerify,
		"sop-draft": RoleLearn, "case-review": RoleLearn, "process-change": RoleAct, "ranked-watch": RoleMonitor}
	// the eight v1 blocks, the four that disk step 1 versions (RFC-0016 §12: metric-monitor, sop-act, sop-investigate, metric-verify @2),
	// and the service watch's four: service-monitor@1, service-verify@1, sop-act@3, sop-investigate@3
	if len(lib) != len(libraryNames)+4+4 {
		t.Fatalf("%d block versions, want %d", len(lib), len(libraryNames)+4+4)
	}
	for _, n := range libraryNames {
		b := lib[BlockRef{n, 1}]
		if b == nil {
			t.Fatalf("%s@1 is not in the library", n)
		}
		if b.Role != roles[n] {
			t.Errorf("%s: role %s, want %s", n, b.Role, roles[n])
		}
		for _, c := range b.Code {
			if _, err := os.Stat(filepath.Join("..", "..", filepath.FromSlash(c))); err != nil {
				t.Errorf("%s names code %s, which the repo does not have: %v", n, c, err)
			}
		}
		if b.Model == "station" && len(b.Texts) == 0 {
			t.Errorf("%s has a station and no prompts", n)
		}
	}
}

// TestLibraryCommandsAreTheLiveOnes: a block's command is the script path the live routines run (/opt/flotilla/ops/<file>),
// so a routine's command never moves when it is expressed as a flow.
func TestLibraryCommandsAreTheLiveOnes(t *testing.T) {
	lib, err := LoadLibrary(repoBlocks)
	if err != nil {
		t.Fatal(err)
	}
	for n, script := range map[string]string{"metric-monitor": "backup_monitor.py", "sop-act": "sop_act.py", "sop-investigate": "sop_investigate.py",
		"metric-verify": "verify_backup.py", "sop-draft": "sop_draft.py", "case-review": "case_review.py", "process-change": "process_change.py",
		"ranked-watch": "ranked_watch.py"} {
		b := lib[BlockRef{n, 1}]
		if b == nil || b.Command[:len("python3 /opt/flotilla/ops/")+len(script)] != "python3 /opt/flotilla/ops/"+script {
			t.Errorf("%s: command %q", n, b.Command)
		}
	}
}

// TestJobsFlowIsByteEqualToItsFolders is the backup proof (RFC-0016 section 8) on the example fleet: the generator's
// output for fleets/jobs/flows/backup is byte-equal to the committed routine folders, so expressing the chain as a flow
// moves no version's hash (the shipped manifests golden would say so, and still passes).
func TestJobsFlowIsByteEqualToItsFolders(t *testing.T) {
	fleetDir := filepath.Join("..", "..", "fleets", "jobs")
	fe, err := ExpandFleet(fleetDir, repoBlocks, actionsStub)
	if err != nil || fe == nil {
		t.Fatalf("%v %v", fe, err)
	}
	if len(fe.Exp.Steps) != 11 { // backup: 3, disk: 4, service: 4
		t.Fatalf("%d steps", len(fe.Exp.Steps))
	}
	if d := fe.Exp.Drift(fleetDir); len(d) != 0 {
		t.Fatalf("the committed folders differ from the flow's expansion: %v", d)
	}
	for p, want := range fe.Exp.Files {
		got, err := os.ReadFile(filepath.Join(fleetDir, filepath.FromSlash(p)))
		if err != nil || string(got) != string(want) {
			t.Errorf("%s is not byte-equal: %v", p, err)
		}
	}
}
