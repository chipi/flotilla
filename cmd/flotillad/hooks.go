package main

import (
	"fmt"
	"sort"

	"github.com/chipi/flotilla/internal/hook"
	"github.com/chipi/flotilla/internal/spec"
)

func hooksByName(hs []*hook.Hook) map[string]*hook.Hook {
	out := make(map[string]*hook.Hook, len(hs))
	for _, h := range hs {
		out[h.Name] = h
	}
	return out
}

// hookLines is -check's account of the hooks: each one, the kind it opens, and a warning when no routine acts
// on that kind, since its cases would wait handed over until their timer escalates them. The smoke hook closes
// its own case and needs none.
func hookLines(cfg *Config, routines []*spec.Routine) []string {
	acting := map[string][]string{}
	for _, j := range routines {
		if j.Trigger.Case != nil && j.Enabled {
			acting[j.Trigger.Case.Kind] = append(acting[j.Trigger.Case.Kind], j.ID())
		}
	}
	var out []string
	for _, h := range cfg.Hooks {
		switch rs := acting[h.Kind]; {
		case h.CloseOnArrival:
			out = append(out, fmt.Sprintf("hook %s: opens a %s case and closes it on arrival (POST /hooks/%s, signed)", h.Name, h.Kind, h.Name))
		case len(rs) == 0:
			out = append(out, fmt.Sprintf("WARNING hook %s: opens %s cases, and no enabled routine acts on that kind: they wait until their timer escalates them", h.Name, h.Kind))
		default:
			sort.Strings(rs)
			out = append(out, fmt.Sprintf("hook %s: opens a %s case for %v (POST /hooks/%s, signed)", h.Name, h.Kind, rs, h.Name))
		}
		if a := h.Asker; a != nil {
			if cfg.Directory == nil {
				out = append(out, fmt.Sprintf("WARNING hook %s: its asker (the %s id in %s) is looked up in the directory, and none is configured: every asker is unknown", h.Name, a.Tool, a.Field))
			} else {
				out = append(out, fmt.Sprintf("hook %s: its asker is the %s id in %s, looked up in the directory when the case opens", h.Name, a.Tool, a.Field))
			}
		}
	}
	return out
}
