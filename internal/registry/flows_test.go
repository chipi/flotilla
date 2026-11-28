package registry

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/chipi/flotilla/internal/spec"
)

// TestRepoFleetsFlowsAgreeWithTheRegistry is the repo's own gate for flows (RFC-0016): the fleets' flows expand against the
// library and the real registry, and their committed routine folders equal the expansion. The same check `make lint`
// runs through `flotilla flow expand -check`, and flotillad -check at a deploy.
func TestRepoFleetsFlowsAgreeWithTheRegistry(t *testing.T) {
	reg, err := Load(filepath.Join("..", "..", "host", "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	lines, err := spec.CheckFlows(filepath.Join("..", "..", "fleets"), filepath.Join("..", "..", "blocks"), reg.FlowActions())
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) == 0 || !strings.Contains(lines[0], "flows: jobs: 4 flows, 15 routines") {
		t.Fatalf("the example fleet jobs is a flow: %v", lines)
	}
}

func TestFlowActionsReadTheRegistry(t *testing.T) {
	reg, err := Load(filepath.Join("..", "..", "host", "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	a, ok := reg.FlowActions()("backup.rerun")
	if !ok || a.Verify != "backup_newer_than_execution" || len(a.Pre) == 0 || len(a.ForKinds) != 1 || a.ForKinds[0] != "failed_backup" {
		t.Fatalf("%+v %v", a, ok)
	}
	if _, ok := reg.FlowActions()("backup.nothing"); ok {
		t.Fatal("an action the registry lacks is not found")
	}
}
