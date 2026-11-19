package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"github.com/chipi/flotilla/internal/models"
	"github.com/chipi/flotilla/internal/prompts"
	"github.com/chipi/flotilla/internal/sched"
	"github.com/chipi/flotilla/internal/spec"
)

// printStations is `flotillad -stations <fleets/f/r/vN/> -models <models config>`: what a run of that version gets (its command, directory,
// timeout and the whole environment, FLOTILLA_STATIONS and FLOTILLA_MODELS among them), with the checks flotillad makes at load, and no
// config, no store and no network. The environment is sched.RunContract, the function a live or shadow run is given its own from.
func printStations(w io.Writer, vdir, modelsPath string) error {
	j, err := spec.LoadVersionDir(vdir)
	if err != nil {
		return err
	}
	var mc *models.Config
	if modelsPath != "" {
		raw, err := os.ReadFile(modelsPath)
		if err != nil {
			return err
		}
		mc = &models.Config{}
		if err := yaml.Unmarshal(raw, mc); err != nil {
			return fmt.Errorf("%s: %w", modelsPath, err)
		}
		if err := mc.Validate(); err != nil {
			return err
		}
	}
	var errs []error
	if len(j.Stations) > 0 {
		errs = append(errs, checkStationModels(j, mc))
	}
	// the version's texts live under the fleet's folder: <fleets>/<fleet>/<routine>/v<n>/, three folders up from the version
	if err := prompts.Check(specsDirOf(vdir), []*spec.Routine{j}); err != nil {
		errs = append(errs, fmt.Errorf("prompts: %w", err))
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}
	env, err := sched.RunContract(j, mc, sched.EnvParams{RunID: "offline", RoutineKey: j.ID(), Stage: spec.FleetdStage(spec.AutonomyRecord), BudgetLeft: j.Budget.DayUSD})
	if err != nil {
		return err
	}
	env["FLOTILLA_SHADOW"] = "1"
	out := struct {
		Routine string            `json:"routine"`
		Version int               `json:"version"`
		Command string            `json:"command"`
		Workdir string            `json:"workdir"`
		Timeout string            `json:"timeout"`
		Env     map[string]string `json:"env"`
	}{j.ID(), j.Version, j.Run.Command, j.Run.Workdir, j.Run.Timeout, env}
	b, _ := json.MarshalIndent(out, "", "  ")
	_, err = fmt.Fprintln(w, string(b))
	return err
}

func specsDirOf(vdir string) string {
	abs, err := filepath.Abs(vdir)
	if err != nil {
		return vdir
	}
	return filepath.Dir(filepath.Dir(filepath.Dir(abs)))
}
