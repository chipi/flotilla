// Package spec loads and validates job specs: fleets/<fleet>/<job>.yaml.
//
// A spec says what a job is: when it runs, which worker runs it, what it runs, how long it may take,
// what it may spend, and its default stage. It holds nothing about one installation (hosts, paths,
// secrets); those are the worker's and flotillad's config.
package spec

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Stages, least to most trusted. What each allows is the job's business in phase 2: the stage is
// passed to the job as FLEETD_STAGE, as fleetd did, and the triage fleet enforces it.
const (
	StageShadow  = "shadow"
	StagePropose = "propose"
	StageLive    = "live"
)

var stages = map[string]bool{StageShadow: true, StagePropose: true, StageLive: true}

var nameRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,39}$`)

type Job struct {
	Fleet       string  `yaml:"fleet"`
	Job         string  `yaml:"job"`
	Description string  `yaml:"description"`
	Enabled     bool    `yaml:"enabled"`
	Worker      string  `yaml:"worker"`
	Trigger     Trigger `yaml:"trigger"`
	Run         Run     `yaml:"run"`
	Stage       string  `yaml:"stage"`
	Budget      Budget  `yaml:"budget"`
	Freshness   string  `yaml:"freshness"`

	// filled by Load
	Path  string        `yaml:"-"`
	Hash  string        `yaml:"-"`
	Every time.Duration `yaml:"-"`
	Limit time.Duration `yaml:"-"`
	Fresh time.Duration `yaml:"-"`
}

type Trigger struct {
	Every string `yaml:"every"`
}

type Run struct {
	Command   string `yaml:"command"`
	Workdir   string `yaml:"workdir"`
	Timeout   string `yaml:"timeout"`
	SpendFile string `yaml:"spend_file"`
}

type Budget struct {
	DayUSD float64 `yaml:"day_usd"`
}

// ID is fleet/job, the key everything else uses.
func (j *Job) ID() string { return j.Fleet + "/" + j.Job }

// Load reads every fleets/<fleet>/<job>.yaml under dir. Any invalid spec fails the whole load, so a
// bad spec stops a deploy instead of silently dropping a job.
func Load(dir string) ([]*Job, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*", "*.yaml"))
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	var jobs []*Job
	seen := map[string]string{}
	var errs []error
	for _, p := range paths {
		j, err := LoadFile(p)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if want := filepath.Base(filepath.Dir(p)); j.Fleet != want {
			errs = append(errs, fmt.Errorf("%s: fleet %q, but the file is under %s/", p, j.Fleet, want))
			continue
		}
		if want := strings.TrimSuffix(filepath.Base(p), ".yaml"); j.Job != want {
			errs = append(errs, fmt.Errorf("%s: job %q, but the file is %s.yaml", p, j.Job, want))
			continue
		}
		if prev, dup := seen[j.ID()]; dup {
			errs = append(errs, fmt.Errorf("%s: %s already defined in %s", p, j.ID(), prev))
			continue
		}
		seen[j.ID()] = p
		jobs = append(jobs, j)
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	if len(jobs) == 0 {
		return nil, fmt.Errorf("no job specs under %s", dir)
	}
	return jobs, nil
}

// LoadFile parses and validates one spec. Unknown fields are errors: a typo must not become a
// silently ignored setting.
func LoadFile(path string) (*Job, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	var j Job
	if err := dec.Decode(&j); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := j.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	sum := sha256.Sum256(raw)
	j.Path, j.Hash = path, hex.EncodeToString(sum[:])[:12]
	return &j, nil
}

func (j *Job) validate() error {
	var errs []error
	bad := func(f string, a ...any) { errs = append(errs, fmt.Errorf(f, a...)) }
	if !nameRe.MatchString(j.Fleet) {
		bad("fleet %q: lower-case letters, digits and dashes", j.Fleet)
	}
	if !nameRe.MatchString(j.Job) {
		bad("job %q: lower-case letters, digits and dashes", j.Job)
	}
	if strings.TrimSpace(j.Description) == "" {
		bad("description is required")
	}
	if !nameRe.MatchString(j.Worker) {
		bad("worker %q: lower-case letters, digits and dashes", j.Worker)
	}
	if strings.TrimSpace(j.Run.Command) == "" {
		bad("run.command is required")
	}
	if !stages[j.Stage] {
		bad("stage %q: one of shadow, propose, live", j.Stage)
	}
	if j.Budget.DayUSD < 0 {
		bad("budget.day_usd %.2f: must not be negative", j.Budget.DayUSD)
	}
	dur := func(field, s string, min time.Duration) time.Duration {
		d, err := time.ParseDuration(s)
		if err != nil {
			bad("%s %q: a duration such as 10m", field, s)
			return 0
		}
		if d < min {
			bad("%s %s: at least %s", field, d, min)
		}
		return d
	}
	j.Every = dur("trigger.every", j.Trigger.Every, time.Minute)
	j.Limit = dur("run.timeout", j.Run.Timeout, 10*time.Second)
	j.Fresh = dur("freshness", j.Freshness, time.Minute)
	if j.Every > 0 && j.Limit > j.Every*3 {
		bad("run.timeout %s is more than three intervals (%s)", j.Limit, j.Every)
	}
	if j.Every > 0 && j.Fresh > 0 && j.Fresh < j.Every {
		bad("freshness %s is shorter than the interval %s: the job would always look stale", j.Fresh, j.Every)
	}
	return errors.Join(errs...)
}
