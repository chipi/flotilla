package spec

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
)

type stationJSON struct {
	Harness   string                 `json:"harness"`
	Model     string                 `json:"model"`
	Tools     string                 `json:"tools"`
	Limits    StationLimits          `json:"limits"`
	Texts     map[string]StationText `json:"texts"`
	ToolNames []string               `json:"tool_names"`
	Gates     []string               `json:"gates"`
	Hooks     *HarnessHooks          `json:"hooks"`
	Skills    []string               `json:"skills"`
	Reads     []readJSON             `json:"reads,omitempty"` // left out without reads, so a version without them keeps its document
}

// StationsJSON is what a run gets in FLOTILLA_STATIONS, or "" for a version without stations: per station its harness, model alias, tools,
// limits, texts (name -> path relative to the fleet's folder, and sha256), tool names, gates, hooks and skills, and the version's limits.
// The model route comes from FLOTILLA_MODELS (the aliases the stations name).
func (j *Routine) StationsJSON() string {
	if len(j.Stations) == 0 {
		return ""
	}
	doc := struct {
		SchemaVersion int                    `json:"schema_version"`
		VersionLimits map[string]any         `json:"version_limits"`
		Stations      map[string]stationJSON `json:"stations"`
	}{1, j.VersionLimits, map[string]stationJSON{}}
	if doc.VersionLimits == nil {
		doc.VersionLimits = map[string]any{}
	}
	for _, n := range j.StationNames() {
		s := j.Stations[n]
		sj := stationJSON{Harness: s.Harness, Model: s.Model, Tools: s.Tools, Limits: s.Limits, Texts: j.StationText[n], Hooks: s.Hooks,
			ToolNames: []string{}, Gates: []string{}, Skills: s.Skills}
		if s.ToolNames != nil {
			sj.ToolNames = *s.ToolNames
		}
		if s.Gates != nil {
			sj.Gates = *s.Gates
		}
		for _, r := range s.Reads {
			sj.Reads = append(sj.Reads, r.resolved())
		}
		if sj.Skills == nil {
			sj.Skills = []string{}
		}
		if sj.Texts == nil {
			sj.Texts = map[string]StationText{}
		}
		doc.Stations[n] = sj
	}
	b, _ := json.Marshal(doc)
	return string(b)
}

// LoadVersionDir loads one version folder (fleets/<fleet>/<routine>/v<n>/) with its routine.yaml beside it, as the routine bound to that
// version: what `flotillad -stations` prints a run of.
func LoadVersionDir(vdir string) (*Routine, error) {
	abs, err := filepath.Abs(vdir)
	if err != nil {
		return nil, err
	}
	m := versionDirRe.FindStringSubmatch(filepath.Base(abs))
	if m == nil {
		return nil, fmt.Errorf("%s is not a version folder (v1, v2, ...)", vdir)
	}
	n, _ := strconv.Atoi(m[1])
	j, err := loadFolder(filepath.Join(filepath.Dir(abs), RoutineFile))
	if err != nil {
		return nil, err
	}
	return j.WithVersion(n)
}
