package api

// ADR-0014: one table of who may do what, read by Server.Check, which the API's handlers and the UI's
// both go through (actions.go).

// Action is something a caller asks to do.
type Action string

const (
	ActRead       Action = "read"        // status, runs, events
	ActLedgerRead Action = "ledger-read" // a person reading the ledger (History)
	ActLedger     Action = "ledger"      // a worker's own ledger (read and write)
	ActRun        Action = "run"         // start a run now
	ActPause      Action = "pause"       // pause a routine
	ActStop       Action = "stop"        // STOP a fleet
	ActResume     Action = "resume"      // resume a routine (loosening: needs a reason)
	ActStart      Action = "start"       // start a STOPped fleet (loosening: needs a reason)
	ActAutonomy   Action = "autonomy"    // set a routine's autonomy (raising it needs a reason)
	ActUsers      Action = "users"       // users and sessions
	ActFreeze     Action = "freeze"      // pull the installation's freeze (every routine at shadow)
	ActUnfreeze   Action = "unfreeze"    // lift it (loosening: needs a reason)
	ActHold       Action = "hold"        // hold a host or a target (RFC-0004 §5.1 step 4)
	ActUnhold     Action = "unhold"      // lift a hold early (loosening: needs a reason)
	ActApprove    Action = "approve"     // approve or reject a proposed plan (RFC-0004 §5)
	ActHosts      Action = "hosts"       // mint a host's enrolment token, revoke a host (ADR-0006 §3)
)

// Roles of people (ADR-0014); callers keep RoleAgent, RoleOperator (the callers file's) and RoleWorker.
const (
	RoleAdmin  = "admin"
	RoleViewer = "viewer"
)

// may is ADR-0014's table. A role missing from an action's set may not do it. The callers-file
// operator is resolved before this table is read (Server.EffectiveRole): it is an operator, with the
// bootstrap besides, only while no admin exists, and nothing after.
var may = map[Action]map[string]bool{
	ActRead:       {RoleAgent: true, RoleViewer: true, RoleOperator: true, RoleAdmin: true},
	ActLedgerRead: {RoleViewer: true, RoleOperator: true, RoleAdmin: true},
	ActLedger:     {RoleWorker: true},
	ActRun:        {RoleAgent: true, RoleOperator: true, RoleAdmin: true},
	ActPause:      {RoleAgent: true, RoleOperator: true, RoleAdmin: true},
	ActStop:       {RoleAgent: true, RoleOperator: true, RoleAdmin: true},
	ActResume:     {RoleOperator: true, RoleAdmin: true},
	ActStart:      {RoleOperator: true, RoleAdmin: true},
	ActAutonomy:   {RoleOperator: true, RoleAdmin: true},
	ActUsers:      {RoleAdmin: true},
	ActFreeze:     {RoleAgent: true, RoleOperator: true, RoleAdmin: true},
	ActUnfreeze:   {RoleOperator: true, RoleAdmin: true},
	ActHold:       {RoleAgent: true, RoleOperator: true, RoleAdmin: true},
	ActUnhold:     {RoleOperator: true, RoleAdmin: true},
	ActHosts:      {RoleOperator: true, RoleAdmin: true},
	ActApprove:    {RoleOperator: true, RoleAdmin: true},
}

// Authorize reports whether a caller holding role may do action.
func Authorize(role string, a Action) bool { return may[a][role] }

// loosening actions need a reason, from every client (ADR-0014 decision 2).
func loosening(a Action) bool {
	return a == ActResume || a == ActStart || a == ActUnfreeze || a == ActUnhold
}
