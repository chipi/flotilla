package spec

import (
	"regexp"
	"strconv"
	"time"
)

// StationRead is one MCP server a harness-flotilla station may read from (RFC-0014 section 4.8): the tools it may call, each by exact name, and
// the bounds. A tool's name to the model is "<server>.<tool>"; the server's address and token are host config, never the spec's.
type StationRead struct {
	Server         string   `yaml:"server" json:"server"`
	Tools          []string `yaml:"tools" json:"tools"`
	MaxCalls       int      `yaml:"max_calls" json:"max_calls"`               // per run, all stations; 0 = the default
	MaxResultChars int      `yaml:"max_result_chars" json:"max_result_chars"` // per call; 0 = the default
	Timeout        string   `yaml:"timeout" json:"-"`                         // per call; "" = the default
}

const (
	readDefaultMaxCalls       = 10
	readMaxCalls              = 100
	readDefaultMaxResultChars = 65536
	readMaxResultChars        = 262144
	readDefaultTimeout        = 15 * time.Second
	readMinTimeout            = time.Second
	readMaxTimeout            = 60 * time.Second
)

var (
	readServerRe = regexp.MustCompile(`^[a-z0-9_-]{1,40}$`)
	readToolRe   = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)
)

// readJSON is a read as the runtime gets it: the defaults filled, the timeout in seconds.
type readJSON struct {
	Server         string   `json:"server"`
	Tools          []string `json:"tools"`
	MaxCalls       int      `json:"max_calls"`
	MaxResultChars int      `json:"max_result_chars"`
	TimeoutS       float64  `json:"timeout_s"`
}

// timeout is the read's per-call timeout; ok is false for a value outside 1s..60s or not a duration.
func (r StationRead) timeout() (d time.Duration, ok bool) {
	if r.Timeout == "" {
		return readDefaultTimeout, true
	}
	d, err := time.ParseDuration(r.Timeout)
	return d, err == nil && d >= readMinTimeout && d <= readMaxTimeout
}

func (r StationRead) resolved() readJSON {
	out := readJSON{Server: r.Server, Tools: r.Tools, MaxCalls: r.MaxCalls, MaxResultChars: r.MaxResultChars}
	if out.Tools == nil {
		out.Tools = []string{}
	}
	if out.MaxCalls == 0 {
		out.MaxCalls = readDefaultMaxCalls
	}
	if out.MaxResultChars == 0 {
		out.MaxResultChars = readDefaultMaxResultChars
	}
	d, _ := r.timeout()
	out.TimeoutS = d.Seconds()
	return out
}

// validateReads checks a station's reads: (at is "stations.<name>").
func validateReads(bad func(string, ...any), at, harness, tools string, reads []StationRead) {
	if len(reads) == 0 {
		return
	}
	if harness != "flotilla" {
		bad("%s.reads: only harness flotilla reads from MCP servers (a coding harness has its own tools)", at)
		return
	}
	// the flotilla harness serves all and none (fleets/harnesses.json): its tools are tool_names and reads, so a reading station is tools: all
	if tools != "all" {
		bad("%s.reads: a station with reads is tools: all, the flotilla harness's value for the tools it is given (got %q)", at, tools)
	}
	seen := map[string]bool{}
	for i, r := range reads {
		rat := at + ".reads[" + strconv.Itoa(i) + "]"
		if !readServerRe.MatchString(r.Server) {
			bad("%s.server: %q is lower-case letters, digits, - and _, at most 40", rat, r.Server)
		} else if seen[r.Server] {
			bad("%s.server: %q is declared twice in this station", rat, r.Server)
		}
		seen[r.Server] = true
		if len(r.Tools) == 0 {
			bad("%s.tools: at least one tool, each by its exact name", rat)
		}
		tseen := map[string]bool{}
		for _, t := range r.Tools {
			if !readToolRe.MatchString(t) {
				bad("%s.tools: %q is a tool's exact name (letters, digits, . _ -, at most 64; no wildcard)", rat, t)
			} else if tseen[t] {
				bad("%s.tools: %q is listed twice", rat, t)
			}
			tseen[t] = true
		}
		if r.MaxCalls != 0 && (r.MaxCalls < 1 || r.MaxCalls > readMaxCalls) {
			bad("%s.max_calls: %d is 1..%d (default %d)", rat, r.MaxCalls, readMaxCalls, readDefaultMaxCalls)
		}
		if r.MaxResultChars != 0 && (r.MaxResultChars < 1 || r.MaxResultChars > readMaxResultChars) {
			bad("%s.max_result_chars: %d is 1..%d (default %d)", rat, r.MaxResultChars, readMaxResultChars, readDefaultMaxResultChars)
		}
		if _, ok := r.timeout(); !ok {
			bad("%s.timeout: %q is a duration from 1s to 60s (default 15s)", rat, r.Timeout)
		}
	}
}
