package registry

import "github.com/chipi/flotilla/internal/spec"

// FlowActions is the registry as a flow's checks read it (RFC-0016 section 5): whether an action exists, its
// preconditions, its check and the case kinds it serves.
func (r *Registry) FlowActions() spec.Actions {
	return func(name string) (spec.ActionInfo, bool) {
		a, ok := r.Action(name)
		if !ok {
			return spec.ActionInfo{}, false
		}
		pre := append(append([]string(nil), a.Pre...), a.PreShadow...)
		return spec.ActionInfo{Pre: pre, Verify: a.Check.Verify, ForKinds: a.ForKinds}, true
	}
}
