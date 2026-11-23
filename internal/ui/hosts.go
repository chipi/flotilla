package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/chipi/flotilla/internal/api"
	"github.com/chipi/flotilla/internal/hostca"
	"github.com/chipi/flotilla/internal/hostwire"
	"github.com/chipi/flotilla/internal/store"
)

type hostRow struct {
	Allowed  []allowedRow
	Load     hostLoad
	Instance string
	store.Host
	Status, Lamp, Cap, Agent, Cert, Exec24, Contact string
	Execs                                           int
}

func (u *Server) hostRows(r *http.Request) ([]hostRow, error) {
	hosts, err := u.API.Store.Hosts(r.Context())
	if err != nil {
		return nil, err
	}
	execs, err := u.API.Store.HostExecutionsSince(r.Context(), u.clockNow().Add(-24*time.Hour).UTC().Format(time.RFC3339))
	if err != nil {
		return nil, err
	}
	n := map[string]int{}
	for _, x := range execs {
		n[x.Host]++
	}
	remote := map[string]api.RemoteWorker{}
	if u.API.Remote != nil {
		for _, w := range u.API.Remote() {
			remote[w.Identity] = w
		}
	}
	out := make([]hostRow, 0, len(hosts))
	for _, h := range hosts {
		row := hostRow{Host: h, Cap: api.HostExecutorCeiling, Agent: orDash(h.Version), Execs: n[h.Name], Exec24: fmt.Sprint(n[h.Name])}
		switch {
		case h.RevokedAt != nil:
			row.Status, row.Lamp, row.Cap, row.Cert = "revoked", "lamp lamp--idle", "–", "revoked "+u.clock(*h.RevokedAt)
		case h.EnrolledAt == nil:
			row.Status, row.Lamp, row.Cert = "not enrolled", "lamp lamp--idle", "–"
		case hostca.WorkerIdentityRe.MatchString(h.Name):
			// a remote worker's link: its state is the link's now (the store's row keeps only its last good contact)
			row.Status, row.Lamp = "worker link, not connected", "lamp lamp--stop"
			if w, ok := remote[h.Name]; ok && w.Refused != "" {
				row.Status, row.Agent = "worker link refused", w.Refused // the status stays short; the versions are the agent's column
			} else if ok && w.Connected {
				row.Status, row.Lamp, row.Agent = "worker link "+w.Worker+", connected", "lamp", w.Version
			} else if !ok {
				row.Status, row.Lamp = "worker link, not configured", "lamp lamp--idle" // enrolled once, no worker names it now
			}
		case h.Connected:
			row.Status, row.Lamp = "connected", "lamp"
		default:
			row.Status, row.Lamp = "not connected", "lamp lamp--stop"
		}
		if row.Cert == "" {
			if t, ok := parseTime(h.CertNotAfter); ok {
				row.Cert = "expires in " + age(time.Until(t))
				if time.Until(t) < 0 {
					row.Cert = "expired " + u.clock(h.CertNotAfter)
				}
			} else {
				row.Cert = "–"
			}
		}
		row.Allowed = allowedOf(h.Allowlist)
		if h.Ceiling != "" {
			row.Cap = h.Ceiling
		}
		row.Instance = u.HostMetrics[h.Name]
		if h.RevokedAt == nil {
			row.Load = u.loadNow(r.Context(), row.Instance)
		}
		row.Contact = "never"
		if h.LastContact != "" {
			row.Contact = u.agoText(h.LastContact)
		}
		out = append(out, row)
	}
	return out, nil
}

type hostsData struct {
	HasLoad bool // a host reports its load: without one the column is all dashes, so it is left out
	T       tableQ
	Tabs    []pillLink
	Heads   []thCell
	Rows    []hostRow
	All     int
	// RoutineCeiling is shown in the footnote: a host's cap does not apply to routine actions, which have their own.
	RoutineCeiling string
}

var hostSorts = map[string]bool{"host": true, "status": true, "exec": true, "agent": true, "cert": true}

func (u *Server) hostsPage(w http.ResponseWriter, r *http.Request, s *store.Session, sid string) {
	rows, err := u.hostRows(r)
	if err != nil {
		http.Error(w, "hosts failed", 500)
		return
	}
	t := readTable(r, "host", false, func(c string) bool { return hostSorts[c] })
	st, q := t.Get("status"), strings.ToLower(t.Get("q"))
	d := hostsData{T: t, All: len(rows), RoutineCeiling: u.API.RoutineActionCeiling()}
	count := map[string]int{}
	var out []hostRow
	for _, h := range rows {
		count[h.Status]++
		if (st == "" || h.Status == st) && (q == "" || strings.Contains(strings.ToLower(h.Name+" "+h.Version), q)) {
			out = append(out, h)
		}
	}
	for _, p := range [][2]string{{"", "All"}, {"connected", "Connected"}, {"not connected", "Not connected"}, {"revoked", "Revoked"}} {
		n := count[p[0]]
		if p[0] == "" {
			n = len(rows)
		}
		d.Tabs = append(d.Tabs, pillLink{Label: p[1], Href: t.With("status", p[0]), N: n, HasN: true, On: st == p[0]})
	}
	key := func(h hostRow) string {
		switch t.Sort {
		case "status":
			return h.Status
		case "exec":
			return fmt.Sprintf("%08d", h.Execs)
		case "agent":
			return h.Version
		case "cert":
			return h.CertNotAfter
		}
		return h.Name
	}
	sort.SliceStable(out, func(i, j int) bool {
		if t.Desc {
			return key(out[i]) > key(out[j])
		}
		return key(out[i]) < key(out[j])
	})
	d.Rows = window(out, &t)
	d.T = t
	for _, h := range rows {
		d.HasLoad = d.HasLoad || h.Load.OK
	}
	d.Heads = []thCell{t.Th("host", "Host"), t.Th("status", "Status"), t.Plain("Host autonomy cap")}
	if d.HasLoad {
		d.Heads = append(d.Heads, t.Plain("CPU · mem · disk"))
	}
	d.Heads = append(d.Heads,
		t.Th("exec", "Executions 24h"), t.Th("agent", "Host agent"), t.Th("cert", "Certificate"))
	u.render(w, "hosts.html", 200, u.pageFor(s, "Hosts", "Hosts", r, d))
}

type hostData struct {
	Charts     []hostChart
	ChartsErr  string
	Containers []containerRow
	Row        hostRow
	Tab        string
	Tabs       []pillLink
	Execs      []store.HostExecution
	T          tableQ
	Heads      []thCell
	ExecAll    int
	CT         tableQ
	CHeads     []thCell
	ContAll    int
	Clock      func(string) string
	// the agent's own lines, as flotillad logged them on arrival ("host log"), from VictoriaLogs
	Level   string
	Levels  []pillLink
	Log     string
	LogsErr string
}

func (u *Server) hostPage(w http.ResponseWriter, r *http.Request, s *store.Session, sid string) {
	rows, err := u.hostRows(r)
	if err != nil {
		http.Error(w, "hosts failed", 500)
		return
	}
	var row *hostRow
	for i := range rows {
		if rows[i].Name == r.PathValue("name") {
			row = &rows[i]
		}
	}
	if row == nil {
		http.NotFound(w, r)
		return
	}
	tab := r.URL.Query().Get("tab")
	switch tab {
	case "agent", "exec", "logs":
	default:
		tab = "over"
	}
	base := "/hosts/" + row.Name
	d := hostData{Row: *row, Tab: tab}
	for _, p := range [][2]string{{"over", "Observability"}, {"agent", "Host agent and allowlist"}, {"exec", "Executions"}, {"logs", "Logs"}} {
		href := base
		if p[0] != "over" {
			href += "?tab=" + p[0]
		}
		d.Tabs = append(d.Tabs, pillLink{Label: p[1], Href: href, On: tab == p[0]})
	}
	if tab == "over" {
		if row.Instance == "" {
			d.ChartsErr = "flotillad.yaml host_metrics names no metrics instance for " + row.Name
		} else if cs, err := u.hostCharts(r.Context(), row.Instance); err != nil {
			d.ChartsErr = err.Error()
		} else {
			d.Charts = cs
			d.containerTable(r, u.containers(r.Context(), row.Instance))
		}
	}
	if tab == "exec" {
		xs, err := u.API.Store.HostExecutions(r.Context(), row.Name)
		if err != nil {
			http.Error(w, "executions failed", 500)
			return
		}
		d.execTable(r, xs)
		for i := range d.Execs {
			d.Execs[i].SentAt = u.clock(d.Execs[i].SentAt)
		}
	}
	if tab == "logs" {
		d.Level = r.URL.Query().Get("level")
		for _, p := range [][2]string{{"", "All levels"}, {"warn", "Warnings and errors"}, {"error", "Errors"}} {
			href := base + "?tab=logs"
			if p[0] != "" {
				href += "&level=" + p[0]
			}
			d.Levels = append(d.Levels, pillLink{Label: p[1], Href: href, On: d.Level == p[0]})
		}
		if lines, err := u.logsQuery(r.Context(), `_time:24h "host log" host:`+logsQuote(row.Name)+levelFilter(d.Level), 300); err != nil {
			d.LogsErr = err.Error()
		} else {
			d.Log = logsText(lines, false)
		}
	}
	u.render(w, "host.html", 200, u.pageFor(s, "Host "+row.Name, "Hosts", r, d))
}

var execSorts = map[string]bool{"when": true, "action": true, "result": true}

// execTable filters, sorts and pages a host's executions, newest first; its keys are prefixed "e" so the tab keeps state apart from the containers' "c".
func (d *hostData) execTable(r *http.Request, xs []store.HostExecution) {
	t := readTableAs(r, "e", "when", true, func(c string) bool { return execSorts[c] })
	t.Anchor = "executions"
	q := strings.ToLower(t.Get("q"))
	var out []store.HostExecution
	for _, x := range xs {
		if q == "" || strings.Contains(strings.ToLower(x.Action+" "+x.CaseID), q) {
			out = append(out, x)
		}
	}
	less := func(a, b store.HostExecution) bool {
		switch t.Sort {
		case "action":
			return a.Action < b.Action
		case "result":
			return a.Status < b.Status
		}
		return a.SentAt < b.SentAt || (a.SentAt == b.SentAt && a.ID < b.ID)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if t.Desc {
			return less(out[j], out[i])
		}
		return less(out[i], out[j])
	})
	d.ExecAll = len(xs)
	d.Execs = window(out, &t)
	d.T = t
	d.Heads = []thCell{t.Th("when", "When"), t.Plain("Execution"), t.Plain("Case"), t.Th("action", "Action"), t.Plain("Approved by"), t.Th("result", "Result")}
}

var containerSorts = map[string]bool{"name": true, "up": true, "cpu": true, "mem": true}

// containerTable filters, sorts and pages a host's containers, the biggest in memory first; its keys are prefixed "c".
func (d *hostData) containerTable(r *http.Request, cs []containerRow) {
	t := readTableAs(r, "c", "mem", true, func(c string) bool { return containerSorts[c] })
	t.Anchor, t.Per = "containers", 20
	q := strings.ToLower(t.Get("q"))
	var out []containerRow
	for _, c := range cs {
		if q == "" || strings.Contains(strings.ToLower(c.Name), q) {
			out = append(out, c)
		}
	}
	less := func(a, b containerRow) bool {
		switch t.Sort {
		case "name":
			return a.Name < b.Name
		case "up":
			return a.UpSec < b.UpSec
		case "cpu":
			return a.CPUPct < b.CPUPct
		}
		return a.MemBytes < b.MemBytes
	}
	sort.SliceStable(out, func(i, j int) bool {
		if t.Desc {
			return less(out[j], out[i])
		}
		return less(out[i], out[j])
	})
	d.ContAll = len(cs)
	d.Containers = window(out, &t)
	d.CT = t
	d.CHeads = []thCell{t.Th("name", "Container"), t.Th("up", "Up"), t.Th("cpu", "CPU"), t.Th("mem", "Memory")}
}

type allowedRow struct {
	Name, Command, Timeout string
	PerHour                int
}

// allowedOf reads a host's reported allowlist (G10); none when its agent reported none.
func allowedOf(raw string) []allowedRow {
	if raw == "" {
		return nil
	}
	var as []hostwire.AllowedAction
	if json.Unmarshal([]byte(raw), &as) != nil {
		return nil
	}
	out := make([]allowedRow, len(as))
	for i, a := range as {
		out[i] = allowedRow{Name: a.Name, Command: strings.Join(a.Argv, " "), Timeout: a.Timeout, PerHour: a.PerHour}
	}
	return out
}

// hostCommand is the exact command a host's agent runs for an action, as it reported its config.
func (u *Server) hostCommand(ctx context.Context, host, action string) string {
	hosts, err := u.API.Store.Hosts(ctx)
	if err != nil {
		return ""
	}
	for _, h := range hosts {
		if h.Name != host || h.RevokedAt != nil {
			continue
		}
		for _, a := range allowedOf(h.Allowlist) {
			if a.Name == action {
				return a.Command
			}
		}
	}
	return ""
}
