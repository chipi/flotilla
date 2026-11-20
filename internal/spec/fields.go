package spec

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// The routine's own fields (what routine.yaml holds, and a flat spec's top-level keys other than the version's): they are in no version's hash and a
// normal fleets deploy changes them. The store keeps a snapshot of them as a record (internal/store routine_snapshots), never as a freeze: flotillad writes
// a routine.update event for each field that moved, and -check says so before the deploy.

// RoutineFields is the routine's own fields as a map from the field's name in routine.yaml to its canonical JSON (the YAML value decoded to plain
// values and encoded with sorted keys), so a Go type or tag never changes a recorded snapshot. Every field is present, an unset one as null or [].
func (j *Routine) RoutineFields() map[string]string {
	rf := routineFile{Fleet: j.Fleet, Routine: j.Routine, Description: j.Description, Enabled: j.Enabled, Worker: j.Worker, Trigger: j.Trigger,
		Detects: j.Detects, Autonomy: j.Autonomy, MaxAutonomy: j.MaxAutonomy, Budget: j.Budget, Freshness: j.Freshness, Resume: j.Resume,
		BriefSection: j.BriefSection, Chain: j.Chain, Acceptance: j.Acceptance, ShadowWorker: j.ShadowWorker, State: j.State, Repo: j.Repo}
	out := map[string]string{}
	rv, rt := reflect.ValueOf(rf), reflect.TypeOf(rf)
	for i := 0; i < rt.NumField(); i++ {
		name := rt.Field(i).Tag.Get("yaml")
		if name == "" || name == "fleet" || name == "routine" { // the routine's identity is the snapshot's key
			continue
		}
		b, err := yaml.Marshal(rv.Field(i).Interface())
		if err != nil {
			out[name] = "null"
			continue
		}
		var v any
		if yaml.Unmarshal(b, &v) != nil {
			out[name] = "null"
			continue
		}
		jb, err := json.Marshal(v)
		if err != nil {
			out[name] = "null"
			continue
		}
		out[name] = string(jb)
	}
	return out
}

// FieldsHash is the hash of a RoutineFields map: sha256 over its sorted (name, value) pairs.
func FieldsHash(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	for _, k := range keys {
		fmt.Fprintf(h, "%s\x00%s\n", k, m[k])
	}
	return hex.EncodeToString(h.Sum(nil))
}

// DiffFields lists, sorted by field, each field whose value differs between the recorded and the loaded routine fields: "budget {"day_usd":1} ->
// {"day_usd":2}". A field only one side has is shown against "(none)". A long value is cut for the line; the snapshot keeps it whole.
func DiffFields(old, now map[string]string) []string {
	names := map[string]bool{}
	for k := range old {
		names[k] = true
	}
	for k := range now {
		names[k] = true
	}
	var keys []string
	for k := range names {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []string
	for _, k := range keys {
		o, oOK := old[k]
		n, nOK := now[k]
		if oOK && nOK && o == n {
			continue
		}
		out = append(out, k+" "+showField(o, oOK)+" -> "+showField(n, nOK))
	}
	return out
}

func showField(v string, ok bool) string {
	if !ok {
		return "(none)"
	}
	var s string
	if strings.HasPrefix(v, `"`) && json.Unmarshal([]byte(v), &s) == nil {
		v = s
	}
	v = strings.Join(strings.Fields(v), " ")
	if len(v) > 120 {
		v = v[:120] + "..."
	}
	return v
}
