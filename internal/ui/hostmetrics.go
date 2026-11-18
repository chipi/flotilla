package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// A host's load from VictoriaMetrics (G4): node exporter's series for the host's metrics instance
// (flotillad.yaml host_metrics: {backup-01: example}). macOS and Linux exporters name memory and the data
// volume differently; each query covers both. Containers come from cAdvisor where a host runs it.

type vmResult struct {
	Metric map[string]string `json:"metric"`
	Value  []any             `json:"value"`
	Values [][]any           `json:"values"`
}

func (u *Server) vm(ctx context.Context, path string, q url.Values) ([]vmResult, error) {
	if u.MetricsURL == "" {
		return nil, fmt.Errorf("flotillad has no metrics address (vm_url)")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(u.MetricsURL, "/")+path, strings.NewReader(q.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("VictoriaMetrics did not answer: %w", err)
	}
	defer resp.Body.Close()
	var body struct {
		Status string `json:"status"`
		Data   struct {
			Result []vmResult `json:"result"`
		} `json:"data"`
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	if body.Status != "success" {
		return nil, fmt.Errorf("VictoriaMetrics: %s", body.Error)
	}
	return body.Data.Result, nil
}

func sampleFloat(v []any) (float64, bool) {
	if len(v) != 2 {
		return 0, false
	}
	s, ok := v[1].(string)
	if !ok {
		return 0, false
	}
	f, err := strconv.ParseFloat(s, 64)
	return f, err == nil
}

// hostQueries are the series a host card and chart draw, by a selector on its instance.
func hostQueries(sel string) map[string]string {
	return map[string]string{
		"cpu": `100 * (1 - avg(rate(node_cpu_seconds_total{mode="idle",` + sel + `}[5m])))`,
		"mem": `100 * max((node_memory_active_bytes{` + sel + `} + node_memory_wired_bytes{` + sel + `} + node_memory_compressed_bytes{` + sel + `}) / node_memory_total_bytes{` + sel + `}` +
			` or (1 - node_memory_MemAvailable_bytes{` + sel + `} / node_memory_MemTotal_bytes{` + sel + `}))`,
		"disk": `100 * max(1 - node_filesystem_avail_bytes{mountpoint=~"/|/System/Volumes/Data",` + sel + `} / node_filesystem_size_bytes{mountpoint=~"/|/System/Volumes/Data",` + sel + `})`,
		"net":  `sum(rate(node_network_receive_bytes_total{device!~"lo.*",` + sel + `}[5m]) + rate(node_network_transmit_bytes_total{device!~"lo.*",` + sel + `}[5m]))`,
		"io":   `sum(rate(node_disk_read_bytes_total{` + sel + `}[5m]) + rate(node_disk_written_bytes_total{` + sel + `}[5m]))`,
		"load": `max(node_load1{` + sel + `})`,
	}
}

type hostLoad struct {
	CPU, Mem, Disk string // "22%" or "–"
	OK             bool
}

func pct(f float64) string { return fmt.Sprintf("%.0f%%", f) }

// loadNow is a host's CPU, memory and disk now; "–" for what the host does not report.
func (u *Server) loadNow(ctx context.Context, instance string) hostLoad {
	l := hostLoad{CPU: "–", Mem: "–", Disk: "–"}
	if instance == "" {
		return l
	}
	qs := hostQueries(`instance="` + instance + `"`)
	for k, dst := range map[string]*string{"cpu": &l.CPU, "mem": &l.Mem, "disk": &l.Disk} {
		res, err := u.vm(ctx, "/api/v1/query", url.Values{"query": {qs[k]}})
		if err != nil || len(res) == 0 {
			continue
		}
		if f, ok := sampleFloat(res[0].Value); ok {
			*dst, l.OK = pct(f), true
		}
	}
	return l
}

type hostChart struct {
	Label, Now, Note string
	Spark            spark
}

func bytesRate(f float64) string {
	switch {
	case f >= 1e6:
		return fmt.Sprintf("%.1f MB/s", f/1e6)
	case f >= 1e3:
		return fmt.Sprintf("%.0f kB/s", f/1e3)
	}
	return fmt.Sprintf("%.0f B/s", f)
}

// charts are a host's last hour, one series per card.
func (u *Server) hostCharts(ctx context.Context, instance string) ([]hostChart, error) {
	now := u.clockNow()
	qs := hostQueries(`instance="` + instance + `"`)
	defs := []struct {
		key, label string
		format     func(float64) string
	}{
		{"cpu", "CPU", pct}, {"mem", "Memory", pct}, {"disk", "Disk", pct},
		{"io", "Disk IO", bytesRate}, {"net", "Network", bytesRate}, {"load", "Load", func(f float64) string { return fmt.Sprintf("%.2f", f) }},
	}
	var out []hostChart
	var firstErr error
	for _, d := range defs {
		res, err := u.vm(ctx, "/api/v1/query_range", url.Values{"query": {qs[d.key]}, "start": {fmt.Sprint(now.Add(-time.Hour).Unix())},
			"end": {fmt.Sprint(now.Unix())}, "step": {"60"}})
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if len(res) == 0 || len(res[0].Values) == 0 {
			continue
		}
		var series []float64
		peak, peakAt := 0.0, ""
		for _, v := range res[0].Values {
			f, ok := sampleFloat(v)
			if !ok {
				continue
			}
			series = append(series, f)
			if f >= peak {
				peak = f
				if ts, ok := v[0].(float64); ok {
					peakAt = time.Unix(int64(ts), 0).In(u.loc()).Format("15:04")
				}
			}
		}
		if len(series) == 0 {
			continue
		}
		c := hostChart{Label: d.label, Now: d.format(series[len(series)-1]), Note: "peak " + d.format(peak) + " at " + peakAt, Spark: makeSpark(series)}
		c.Spark.Label = d.label + " over the last hour"
		out = append(out, c)
	}
	if len(out) == 0 && firstErr != nil {
		return nil, firstErr
	}
	return out, nil
}

type containerRow struct {
	Name, Up, CPU, Mem string
}

// containers are a host's containers from cAdvisor, by memory; none when the host runs no cAdvisor.
func (u *Server) containers(ctx context.Context, instance string) []containerRow {
	sel := `instance="` + instance + `",name!=""`
	mem, err := u.vm(ctx, "/api/v1/query", url.Values{"query": {`max by (name)(container_memory_usage_bytes{` + sel + `})`}})
	if err != nil || len(mem) == 0 {
		return nil
	}
	cpu, _ := u.vm(ctx, "/api/v1/query", url.Values{"query": {`100 * sum by (name)(rate(container_cpu_usage_seconds_total{` + sel + `}[5m]))`}})
	start, _ := u.vm(ctx, "/api/v1/query", url.Values{"query": {`max by (name)(container_start_time_seconds{` + sel + `})`}})
	cpuBy, startBy := map[string]float64{}, map[string]float64{}
	for _, r := range cpu {
		if f, ok := sampleFloat(r.Value); ok {
			cpuBy[r.Metric["name"]] = f
		}
	}
	for _, r := range start {
		if f, ok := sampleFloat(r.Value); ok {
			startBy[r.Metric["name"]] = f
		}
	}
	type row struct {
		containerRow
		mem float64
	}
	var rows []row
	for _, r := range mem {
		f, _ := sampleFloat(r.Value)
		n := r.Metric["name"]
		c := containerRow{Name: n, CPU: fmt.Sprintf("%.1f%%", cpuBy[n]), Mem: fmt.Sprintf("%.0f MB", f/1e6), Up: "–"}
		if s := startBy[n]; s > 0 {
			c.Up = age(u.clockNow().Sub(time.Unix(int64(s), 0)))
		}
		rows = append(rows, row{c, f})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].mem > rows[j].mem })
	out := make([]containerRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.containerRow)
	}
	return out
}
