package spec

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/chipi/flotilla/fleets"
)

// Station is one model step of a version (v2-design.md section 2). Its name is the author's; flotilla gives it no meaning.
type Station struct {
	Harness string            `yaml:"harness" json:"harness"`
	Model   string            `yaml:"model" json:"model"`
	Prompts map[string]string `yaml:"prompts" json:"-"` // text name -> a file inside v<n>/
	Tools   string            `yaml:"tools" json:"tools"`
	Limits  StationLimits     `yaml:"limits" json:"limits"`
	// harness flotilla only
	ToolNames *[]string     `yaml:"tool_names" json:"-"`
	Gates     *[]string     `yaml:"gates" json:"-"`
	Hooks     *HarnessHooks `yaml:"hooks" json:"-"`
	Skills    []string      `yaml:"skills" json:"-"`
	Reads     []StationRead `yaml:"reads" json:"-"`
}

// StationLimits are per episode.
type StationLimits struct {
	MaxTurns        int    `yaml:"max_turns" json:"max_turns"`
	Timeout         string `yaml:"timeout" json:"timeout"`
	MaxOutputTokens int    `yaml:"max_output_tokens" json:"max_output_tokens"`
}

// StationText is a station's text as loaded: its path relative to the fleet's folder (where internal/prompts resolves it) and its hash.
type StationText struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// HarnessCaps is one harness's row of fleets/harnesses.json.
type HarnessCaps struct {
	ToolsServed        []string `json:"tools_served"`
	CanResume          bool     `json:"can_resume"`
	ResumeAfterKill    bool     `json:"resume_after_kill"`
	EnforcesReadonly   bool     `json:"enforces_readonly"`
	SupportsHooks      bool     `json:"supports_hooks"`
	ReportsCost        bool     `json:"reports_cost"`
	ReportsUsage       bool     `json:"reports_usage"`
	SupportsTurnCap    bool     `json:"supports_turn_cap"`
	NativeSystemPrompt bool     `json:"native_system_prompt"`
	PromptViaStdin     bool     `json:"prompt_via_stdin"`
}

var (
	harnessOnce sync.Once
	harnessTab  map[string]HarnessCaps
	harnessErr  error
)

// Harnesses is the capability table shipped with flotillad (fleets/harnesses.json, embedded).
func Harnesses() (map[string]HarnessCaps, error) {
	harnessOnce.Do(func() {
		var doc struct {
			Version   int                    `json:"version"`
			Harnesses map[string]HarnessCaps `json:"harnesses"`
		}
		if err := json.Unmarshal(fleets.HarnessesJSON, &doc); err != nil {
			harnessErr = fmt.Errorf("fleets/harnesses.json: %w", err)
			return
		}
		if doc.Version != 1 || len(doc.Harnesses) == 0 {
			harnessErr = fmt.Errorf("fleets/harnesses.json: version %d with %d harnesses", doc.Version, len(doc.Harnesses))
			return
		}
		harnessTab = doc.Harnesses
	})
	return harnessTab, harnessErr
}

func (c HarnessCaps) serves(tools string) bool {
	for _, t := range c.ToolsServed {
		if t == tools {
			return true
		}
	}
	return false
}

// StationNames lists the version's stations, sorted.
func (j *Routine) StationNames() []string {
	out := make([]string, 0, len(j.Stations))
	for n := range j.Stations {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// StationTextRoles lists "<station>.<text>" for every text of every station, sorted: the roles internal/prompts checks them under.
func (j *Routine) StationTextRoles() []string {
	var out []string
	for n, m := range j.StationText {
		for t := range m {
			out = append(out, n+"."+t)
		}
	}
	sort.Strings(out)
	return out
}

// StationTextPath is the path, relative to the fleet's folder, of the text named by a role from StationTextRoles.
func (j *Routine) StationTextPath(role string) string {
	n, t, _ := strings.Cut(role, ".")
	return j.StationText[n][t].Path
}

// validateStations checks a version's stations and fills what is derived from them. rel is the version's folder as the fleet's folder sees it
// ("<routine>/v<n>/"); vdir is where it is on disk.
func (j *Routine) validateStations(bad func(string, ...any), vf *versionFile, vdir, rel string) {
	if vf.Harness != nil || len(vf.Prompts) > 0 || len(vf.Models) > 0 {
		bad("a version with stations declares no harness:, prompts: or models: of its own (the stations carry them; models: is derived)")
	}
	tab, err := Harnesses()
	if err != nil {
		bad("%v", err)
		return
	}
	models := map[string]bool{}
	j.StationText = map[string]map[string]StationText{}
	for _, name := range sortedNames(vf.Stations) {
		s := vf.Stations[name]
		at := "stations." + name
		if s == nil {
			bad("%s: empty", at)
			continue
		}
		if !caseFieldRe.MatchString(name) {
			bad("%s: a station name is lower-case letters, digits and _, starting with a letter, at most 40", at)
		}
		caps, known := tab[s.Harness]
		if !known {
			bad("%s.harness: %q is not a harness this flotillad knows (fleets/harnesses.json: %s)", at, s.Harness, strings.Join(sortedNames(tab), ", "))
		}
		if s.Model == "" {
			bad("%s.model: an alias of the installation's models config is required", at)
		}
		models[s.Model] = true
		switch s.Tools {
		case "all", "read", "none":
			if known && !caps.serves(s.Tools) {
				bad("%s.tools: harness %s does not serve tools: %s (it serves %s)", at, s.Harness, s.Tools, strings.Join(caps.ToolsServed, ", "))
			}
		default:
			bad("%s.tools: %q is all, read or none", at, s.Tools)
		}
		if s.Limits.MaxTurns < 0 || s.Limits.MaxOutputTokens < 0 {
			bad("%s.limits: max_turns and max_output_tokens are not negative", at)
		}
		if s.Limits.Timeout != "" {
			if d, err := time.ParseDuration(s.Limits.Timeout); err != nil || d <= 0 {
				bad("%s.limits.timeout: %q is not a positive duration", at, s.Limits.Timeout)
			}
		}
		if s.Harness == "flotilla" {
			if s.ToolNames == nil || s.Gates == nil {
				bad("%s: harness flotilla needs tool_names and gates (each may be [])", at)
			}
			if s.Tools == "none" && s.ToolNames != nil && len(*s.ToolNames) > 0 {
				bad("%s: tools: none with tool_names", at)
			}
			if s.ToolNames != nil {
				for _, t := range *s.ToolNames {
					if strings.Contains(t, ".") {
						bad("%s.tool_names: %q has a dot; a dotted name is a read tool, declared in reads:", at, t)
					}
				}
			}
		} else if s.ToolNames != nil || s.Gates != nil || s.Hooks != nil || len(s.Skills) > 0 {
			bad("%s: tool_names, gates, hooks and skills belong to harness flotilla only", at)
		}
		validateReads(bad, at, s.Harness, s.Tools, s.Reads)
		if s.Hooks != nil && known && !caps.SupportsHooks {
			bad("%s.hooks: harness %s has no hooks", at, s.Harness)
		}
		if known && j.Resume != nil && s.Tools == "all" && !caps.ResumeAfterKill {
			bad("%s: the routine resumes (resume:) and this station writes on harness %s, which cannot continue a killed session", at, s.Harness)
		}
		j.StationText[name] = map[string]StationText{}
		for _, t := range sortedNames(s.Prompts) {
			p := s.Prompts[t]
			if !caseFieldRe.MatchString(t) {
				bad("%s.prompts.%s: a text name is lower-case letters, digits and _", at, t)
			}
			if !validPromptPath(p) {
				bad("%s.prompts.%s: %q is not a clean .md or .txt path inside the version's folder", at, t, p)
				continue
			}
			ref := StationText{Path: rel + p}
			if b, err := os.ReadFile(filepath.Join(vdir, filepath.FromSlash(p))); err == nil {
				ref.SHA256 = shaHex(b)
			}
			j.StationText[name][t] = ref
		}
	}
	j.Models = sortedNames(models)
	for k := range vf.Limits {
		if !caseFieldRe.MatchString(k) {
			bad("limits.%s: a limit name is lower-case letters, digits and _", k)
		}
	}
}

func sortedNames[T any](m map[string]T) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
