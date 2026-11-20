package spec

import (
	"fmt"
	"os"
	"path"
	"reflect"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// MoveProof is what ProveMoved found when a folder v1 is set against the manifest a flat v1 was recorded with:
// Compared names each key and file that was equal, Differs each one that was not. A move is provably unchanged when Differs is empty and
// something was compared.
type MoveProof struct {
	Compared []string
	Differs  []string
}

// Unchanged says the folder version loads to what the recorded flat v1 did.
func (p MoveProof) Unchanged() bool { return len(p.Differs) == 0 && len(p.Compared) > 0 }

// ProveMoved sets folder version v of routine j against the manifest recorded for the flat v1 it replaces, key by key:
//   - each version-owned key (run, explain, steps, skills, harness, models): the sha256 of its YAML in v<n>/spec.yaml, which is how legacyFields
//     recorded it, against the recorded one;
//   - each prompt: a prompt's path changes with the move, so each role's file is matched to the recorded prompt-file entry with the same bytes, and
//     the recorded "prompts" sha must then be what the flat spec's role-to-path map hashes to (so a role that points at another file is caught);
//   - each skill file: the recorded skill-file entry against the folder's.
//
// A version with stations has no flat counterpart. The routine's own fields (routine.yaml) are not in the manifest and are not part of the freeze.
func (j *Routine) ProveMoved(v *Version, recorded map[string]string) MoveProof {
	var p MoveProof
	diff := func(f string, a ...any) { p.Differs = append(p.Differs, fmt.Sprintf(f, a...)) }
	if len(v.Stations) > 0 || len(v.VersionLimits) > 0 {
		diff("stations (a flat spec has none)")
		return p
	}
	raw, err := os.ReadFile(v.Path)
	if err != nil {
		diff("%s (%v)", VersionFile, err)
		return p
	}
	found := legacyFields(raw)
	for _, k := range legacyOwned {
		if k == "prompts" {
			continue
		}
		r, rok := recorded[k]
		f, fok := found[k]
		switch {
		case !rok && !fok:
		case !rok:
			diff("%s (added)", k)
		case !fok:
			diff("%s (missing)", k)
		case r != f:
			diff("%s (changed)", k)
		default:
			p.Compared = append(p.Compared, k)
		}
	}

	recPrompts := map[string]string{}
	for k, s := range recorded {
		if f, ok := strings.CutPrefix(k, "prompt-file:"); ok {
			recPrompts[f] = s
		}
	}
	old := map[string]any{}
	matched := map[string]bool{}
	resolved := true
	for _, role := range sortedKeys(v.Prompts) {
		newp := strings.TrimPrefix(v.Prompts[role], j.Routine+"/")
		sha := v.PromptSHA[role]
		var same, named []string
		for f, s := range recPrompts {
			if sha != "" && s == sha {
				same = append(same, f)
			}
			if path.Base(f) == path.Base(newp) {
				named = append(named, f)
			}
		}
		if len(same) > 1 {
			var narrowed []string
			for _, f := range same {
				if path.Base(f) == path.Base(newp) {
					narrowed = append(narrowed, f)
				}
			}
			same = narrowed
		}
		sort.Strings(same)
		sort.Strings(named)
		switch {
		case len(same) == 1:
			old[role] = same[0]
			matched[same[0]] = true
			p.Compared = append(p.Compared, "prompt-file:"+same[0]+" -> "+newp)
		case len(same) > 1:
			resolved = false
			diff("prompt-file:%s (ambiguous: several recorded files have its bytes)", newp)
		case len(named) > 0:
			resolved = false
			diff("prompt-file:%s -> %s (changed)", named[0], newp)
		default:
			resolved = false
			diff("prompt-file:%s (changed: no recorded prompt file has these bytes)", newp)
		}
	}
	if resolved {
		var unmatched []string
		for f := range recPrompts {
			if !matched[f] {
				unmatched = append(unmatched, f)
			}
		}
		sort.Strings(unmatched)
		for _, f := range unmatched {
			diff("prompt-file:%s (missing)", f)
		}
	}
	recRoles, rok := recorded["prompts"]
	switch {
	case !resolved:
	case len(v.Prompts) == 0 && !rok:
	case len(v.Prompts) == 0:
		diff("prompts (missing)")
	case !rok:
		diff("prompts (added)")
	default:
		b, err := yaml.Marshal(old)
		if err != nil || shaHex(b) != recRoles {
			diff("prompts (changed: a role points at another file)")
		} else {
			p.Compared = append(p.Compared, "prompts")
		}
	}

	skills := map[string]bool{}
	for k := range recorded {
		if n, ok := strings.CutPrefix(k, "skill-file:"); ok {
			skills[n] = true
		}
	}
	for k := range v.Manifest {
		if n, ok := strings.CutPrefix(k, "skill-file:"); ok {
			skills[n] = true
		}
	}
	names := make([]string, 0, len(skills))
	for n := range skills {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		k := "skill-file:" + n
		r, rok := recorded[k]
		f, fok := v.Manifest[k]
		switch {
		case !rok:
			diff("%s (added)", k)
		case !fok:
			diff("%s (missing)", k)
		case r != f:
			diff("%s (changed)", k)
		default:
			p.Compared = append(p.Compared, k)
		}
	}
	if len(p.Differs) == 0 && len(p.Compared) == 0 {
		diff("nothing recorded to compare")
	}
	return p
}

// MovedView is a routine bound to version n with what a move may legitimately change taken out: where it was loaded from, its recorded
// hashes, the version list and the notes about the old autonomy word. A prompt's path is written relative to the fleet's folder, so it
// names the file's new place; its role and its content hash must be the same. Two views of the same routine in the two layouts
// are equal when the move changed nothing the routine loads to.
func MovedView(j *Routine, version int) (Routine, error) {
	vj, err := j.WithVersion(version)
	if err != nil {
		return Routine{}, err
	}
	c := *vj
	c.Path, c.Hash, c.VersionHash, c.Versions, c.Version, c.Notes = "", "", "", nil, 0, nil
	if c.Prompts != nil {
		p := map[string]string{}
		for role, f := range c.Prompts {
			p[role] = path.Base(f)
		}
		c.Prompts = p
	}
	return c, nil
}

// DifferingFields names each field of two routines that are not deeply equal, with both values.
func DifferingFields(a, b Routine) []string {
	var out []string
	va, vb := reflect.ValueOf(a), reflect.ValueOf(b)
	for i := 0; i < va.NumField(); i++ {
		if !reflect.DeepEqual(va.Field(i).Interface(), vb.Field(i).Interface()) {
			out = append(out, fmt.Sprintf("%s is %s, flat %s", va.Type().Field(i).Name, short(va.Field(i).Interface()), short(vb.Field(i).Interface())))
		}
	}
	return out
}

func short(v any) string {
	s := fmt.Sprintf("%v", v)
	if len(s) > 160 {
		s = s[:160] + "..."
	}
	return s
}
