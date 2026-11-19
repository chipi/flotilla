package main

import (
	"errors"
	"fmt"

	"github.com/chipi/flotilla/internal/models"
	"github.com/chipi/flotilla/internal/spec"
)

// checkStationModels is the part of a version's station check that needs the installation's models config: each station's alias exists,
// and a station that gives the model tools (tools: all or read) is not on an alias whose capability is none (a station's `tools` and an
// alias's `tools` are different things with one name: what the harness offers, and what the model can call).
func checkStationModels(j *spec.Routine, cfg *models.Config) error {
	var errs []error
	for _, name := range j.StationNames() {
		s := j.Stations[name]
		if cfg == nil || cfg.AliasTools(s.Model) == "" && !hasAlias(cfg, s.Model) {
			errs = append(errs, fmt.Errorf("%s: station %s: model %q is not an alias of the models config", j.ID(), name, s.Model))
			continue
		}
		if s.Tools != "none" && cfg.AliasTools(s.Model) == "none" {
			errs = append(errs, fmt.Errorf("%s: station %s gives tools (%s) to alias %s, whose tools capability is none", j.ID(), name, s.Tools, s.Model))
		}
	}
	return errors.Join(errs...)
}

func hasAlias(cfg *models.Config, alias string) bool {
	_, ok := cfg.Aliases[alias]
	return ok
}
