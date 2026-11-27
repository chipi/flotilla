package spec

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// The param kinds of the performance watch (RFC-0017, ranked-watch@1). Each renders to one value of the command's
// environment, in a shape ops/ranked_watch.py reads back.
const (
	KindSourceName = "source_name" // the NAME of a source: its URL is host config (SOURCE_<NAME> in the worker's environment), never the spec
	KindJSONPath   = "json_path"   // steps.7d: dotted keys into the source's JSON
	KindField      = "field"       // a field of a row
	KindRiseRule   = "rise_rule"   // {ratio, at_least}: a rise counts when it is both
	KindSignals    = "signals"     // {source, ids, fresh}: the service-level source, the row ids to read (globs), the collector's freshness row
	KindUnit       = "unit"        // how a cost reads: seconds
)

func init() {
	for _, k := range []string{KindSourceName, KindJSONPath, KindField, KindRiseRule, KindSignals, KindUnit} {
		paramKinds[k] = true
	}
}

var (
	sourceNameRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
	jsonPathRe   = regexp.MustCompile(`^[A-Za-z0-9_]{1,40}(\.[A-Za-z0-9_]{1,40}){0,7}$`)
	signalIDRe   = regexp.MustCompile(`^[A-Za-z0-9._*-]{1,100}$`)
)

// renderWatchParam is renderParamValue for the kinds above; a kind it does not know is an error.
func renderWatchParam(p Param, v any) (string, error) {
	switch p.Kind {
	case KindSourceName:
		s, ok := v.(string)
		if !ok || !sourceNameRe.MatchString(s) {
			return "", fmt.Errorf("%v: a source name such as ci_stats (the URL is host config)", v)
		}
		return s, nil
	case KindJSONPath:
		s, ok := v.(string)
		if !ok || !jsonPathRe.MatchString(s) {
			return "", fmt.Errorf("%v: dotted keys such as steps.7d", v)
		}
		return s, nil
	case KindField:
		s, ok := v.(string)
		if !ok || !labelNameRe.MatchString(s) {
			return "", fmt.Errorf("%v: a field name", v)
		}
		return s, nil
	case KindUnit:
		if s, ok := v.(string); !ok || s != "seconds" {
			return "", fmt.Errorf("%v: seconds", v)
		}
		return "seconds", nil
	case KindRiseRule:
		return renderRise(v)
	case KindSignals:
		return renderSignals(v)
	}
	return "", fmt.Errorf("kind %q", p.Kind)
}

func number(v any) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case float64:
		return n, !math.IsNaN(n) && !math.IsInf(n, 0)
	}
	return 0, false
}

// renderRise is ratio:at_least ("0.5:600"): the share of the previous cost and the least rise, in the cost's own unit.
func renderRise(v any) (string, error) {
	m, ok := v.(map[string]any)
	if !ok || len(m) != 2 {
		return "", fmt.Errorf("%v: {ratio: 0.5, at_least: 600}", v)
	}
	ratio, rok := number(m["ratio"])
	least, lok := number(m["at_least"])
	if !rok || !lok || ratio <= 0 || ratio > 1000 || least < 0 || least != math.Trunc(least) || least > 1e9 {
		return "", fmt.Errorf("%v: ratio above 0 and at_least a whole number from 0, both given", v)
	}
	return strconv.FormatFloat(ratio, 'f', -1, 64) + ":" + strconv.FormatFloat(least, 'f', 0, 64), nil
}

// renderSignals is source:fresh:id,id (the ids and their colon omitted when there are none). An id may be a glob, and a glob would be expanded by the shell that runs the command,
// so the value is single-quoted when it has a star (no other character in it is special).
func renderSignals(v any) (string, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return "", fmt.Errorf("%v: {source: ..., ids: [...], fresh: ...}", v)
	}
	var keys []string
	for k := range m {
		keys = append(keys, k)
		if k != "source" && k != "ids" && k != "fresh" {
			sort.Strings(keys)
			return "", fmt.Errorf("signals has no %q: source, ids and fresh", k)
		}
	}
	src, _ := m["source"].(string)
	if !sourceNameRe.MatchString(src) {
		return "", fmt.Errorf("signals source %v: a source name such as slo_stats", m["source"])
	}
	fresh := ""
	if f, given := m["fresh"]; given {
		s, ok := f.(string)
		if !ok || !signalIDRe.MatchString(s) || strings.Contains(s, "*") {
			return "", fmt.Errorf("signals fresh %v: the collector's freshness row id (no glob)", f)
		}
		fresh = s
	}
	var ids []string
	if l, given := m["ids"]; given {
		list, ok := l.([]any)
		if !ok {
			return "", fmt.Errorf("signals ids %v: a list of row ids, globs allowed", l)
		}
		for _, e := range list {
			s, ok := e.(string)
			if !ok || !signalIDRe.MatchString(s) {
				return "", fmt.Errorf("signals ids: %v is not a row id (letters, digits . _ - and *)", e)
			}
			ids = append(ids, s)
		}
	}
	if fresh == "" && len(ids) == 0 {
		return "", fmt.Errorf("signals needs ids to read or a fresh row to check")
	}
	out := src + ":" + fresh
	if len(ids) > 0 { // never a trailing colon: the command is a YAML plain scalar, where ": " would read as a key
		out += ":" + strings.Join(ids, ",")
	}
	if strings.Contains(out, "*") {
		out = "'" + out + "'"
	}
	return out, nil
}

// briefSectionProblem is why a title cannot be a brief section ("" when it can): the same rule a routine's brief_section keeps.
func briefSectionProblem(s string) string {
	switch {
	case !briefSectionRe.MatchString(s):
		return fmt.Sprintf("brief_section: %q is not a short title (a capital, then letters and spaces, at most 32)", s)
	case s == "Needs you" || s == "Handled overnight" || s == "Watch" || s == "Decisions":
		return fmt.Sprintf("brief_section: %q is one of the brief's own sections", s)
	}
	return ""
}
