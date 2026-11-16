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
	out := make([]hostRow, 0, len(hosts))
	for _, h := range hosts {
		row := hostRow{Host: h, Cap: api.HostCeiling, Agent: orDash(h.Version), Execs: n[h.Name], Exec24: fmt.Sprint(n[h.Name])}
		switch {
		case h.RevokedAt != nil:
			row.Status, row.Lamp, row.Cap, row.Cert = "revoked", "lamp lamp--idle", "–", "revoked "+u.clock(*h.RevokedAt)
		case h.EnrolledAt == nil:
			row.Status, row.Lamp, row.Cert = "not enrolled", "lamp lamp--idle", "–"
		case hostca.WorkerIdentityRe.MatchString(h.Name):
			row.Status, row.Lamp = "worker link", "lamp lamp--idle" // its state is on its fleet's page
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
	T     tableQ
	Tabs  []pillLink
	Heads []thCell
	Rows  []hostRow
	All   int
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
	d := hostsData{T: t, All: len(rows)}
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
	d.Heads = []thCell{t.Th("host", "Host"), t.Th("status", "Status"), t.Plain("Roles"), t.Plain("Autonomy cap"), t.Plain("CPU · mem · disk"),
		t.Th("exec", "Executions 24h"), t.Th("agent", "Host agent"), t.Th("cert", "Certificate")}
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
			d.Containers = u.containers(r.Context(), row.Instance)
		}
	}
	if tab == "exec" {
		xs, err := u.API.Store.HostExecutions(r.Context(), row.Name)
		if err != nil {
			http.Error(w, "executions failed", 500)
			return
		}
		sort.Slice(xs, func(i, j int) bool { return xs[i].SentAt > xs[j].SentAt })
		t := readTable(r, "", false, func(string) bool { return false })
		d.Execs = window(xs, &t)
		d.T = t
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
