package spec

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
)

// FleetExpansion is one fleet's flows as the generator expands them now.
type FleetExpansion struct {
	Fleet string
	Dir   string
	Flows int // flows, not counting the learning blocks
	Exp   *Expansion
}

// ExpandFleet expands the flows of the fleet in dir (fleets/<fleet>); nil when it has none.
func ExpandFleet(dir, blocksDir string, actions Actions) (*FleetExpansion, error) {
	ff, err := LoadFlows(dir)
	if err != nil || ff == nil {
		return nil, err
	}
	exp, err := ff.Expand(blocksDir, actions)
	if err != nil {
		return nil, fmt.Errorf("fleet %s: %w", ff.Fleet, err)
	}
	return &FleetExpansion{Fleet: ff.Fleet, Dir: dir, Flows: len(ff.Flows), Exp: exp}, nil
}

// CheckFlows is flotillad -check's flow gate: every fleet under specsDir that has flows is expanded, its wiring checked
// against the blocks and the registry, and its committed folders compared with the expansion. It returns one line
// per fleet that passed, and an error naming every refusal; a drifted file is named with its first differing line.
func CheckFlows(specsDir, blocksDir string, actions Actions) ([]string, error) {
	dirs, err := filepath.Glob(filepath.Join(specsDir, "*", FlowsDir))
	if err != nil {
		return nil, err
	}
	sort.Strings(dirs)
	var lines []string
	var errs []error
	for _, d := range dirs {
		fleetDir := filepath.Dir(d)
		fe, err := ExpandFleet(fleetDir, blocksDir, actions)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if fe == nil {
			continue
		}
		if drift := fe.Exp.Drift(fleetDir); len(drift) > 0 {
			for _, l := range drift {
				errs = append(errs, fmt.Errorf("fleet %s: the committed folders differ from the flows' expansion: %s (run flotilla flow expand %s)", fe.Fleet, l, fleetDir))
			}
			continue
		}
		lines = append(lines, fmt.Sprintf("flows: %s: %d flows, %d routines, committed folders equal the generator's output", fe.Fleet, fe.Flows, len(fe.Exp.Steps)))
	}
	return lines, errors.Join(errs...)
}
