package ui

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"

	"github.com/chipi/flotilla/internal/store"
)

// A model call's tools column is JSON in one of two shapes, and both occur in one database: the older
// ["bash","read"] and the newer [{"name":"bash","arg":"grep -n x f.ts","ok":true}] (arg and ok may be absent).
// parseTools reads either, and a mix, and never fails: what it cannot read is skipped.

type toolCall struct {
	Name, Arg string
	OK        *bool // nil when the call did not record its result
}

func (t toolCall) Failed() bool { return t.OK != nil && !*t.OK }

// writesFile is a tool that changes a file; the swimlane draws it amber.
func (t toolCall) WritesFile() bool {
	switch strings.ToLower(t.Name) {
	case "edit", "write", "multiedit", "patch", "apply_patch":
		return true
	}
	return false
}

func parseTools(raw string) []toolCall {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var items []json.RawMessage
	if json.Unmarshal([]byte(raw), &items) != nil {
		return nil
	}
	var out []toolCall
	for _, it := range items {
		var name string
		if json.Unmarshal(it, &name) == nil {
			if name != "" {
				out = append(out, toolCall{Name: name})
			}
			continue
		}
		var obj map[string]json.RawMessage
		if json.Unmarshal(it, &obj) != nil {
			continue
		}
		var tc toolCall
		_ = json.Unmarshal(obj["name"], &tc.Name)
		if tc.Name == "" {
			continue
		}
		if a, ok := obj["arg"]; ok && json.Unmarshal(a, &tc.Arg) != nil {
			tc.Arg = strings.TrimSpace(string(a))
		}
		var ok bool
		if o, has := obj["ok"]; has && json.Unmarshal(o, &ok) == nil {
			tc.OK = &ok
		}
		out = append(out, tc)
	}
	return out
}

// toolCounts is "bash 54, read 8": each tool name with its count, the most used first.
func toolCounts(tcs []toolCall) string {
	n := map[string]int{}
	for _, t := range tcs {
		n[t.Name]++
	}
	names := make([]string, 0, len(n))
	for k := range n {
		names = append(names, k)
	}
	sort.Slice(names, func(a, b int) bool {
		if n[names[a]] != n[names[b]] {
			return n[names[a]] > n[names[b]]
		}
		return names[a] < names[b]
	})
	parts := make([]string, len(names))
	for i, k := range names {
		parts[i] = k + " " + strconv.Itoa(n[k])
	}
	return strings.Join(parts, ", ")
}

// isStep is a harness milestone row, not a model call: kind "step" (schema 25), or a row from before it with
// neither an alias nor a model.
func isStep(c store.ModelCall) bool { return c.IsStep() || (c.Alias == "" && c.Model == "") }

// isAgentCall is a real call a routine made to do its work: no step row, no model-contract check.
func isAgentCall(c store.ModelCall) bool { return !isStep(c) && c.Check == "" }

// realCalls counts a run's model calls: harness step rows are milestones, not calls.
func realCalls(cs []store.ModelCall) int {
	n := 0
	for _, c := range cs {
		if !isStep(c) {
			n++
		}
	}
	return n
}
