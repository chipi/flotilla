// Package telemetry is flotillad's observability on the homelab's conventions (RFC-0001 §6.2): JSON
// logs on stdout (Alloy ships container logs to VictoriaLogs), metrics pushed to VictoriaMetrics'
// Prometheus import, traces over OTLP/HTTP to VictoriaTraces. service="flotillad" and the configured
// environment label everything.
package telemetry

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

const Service = "flotillad"

// Logger writes JSON lines to stdout with service, environment and version on each.
func Logger(env, version string) *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("service", Service, "environment", env, "version", version)
}

// Tracing sets the global tracer provider. An empty endpoint leaves tracing off (a no-op tracer).
// The returned function flushes and stops it.
func Tracing(ctx context.Context, endpoint, env, version string) (func(context.Context) error, error) {
	otel.SetTextMapPropagator(propagation.TraceContext{})
	if endpoint == "" {
		return func(context.Context) error { return nil }, nil
	}
	exp, err := otlptracehttp.New(ctx, otlptracehttp.WithEndpointURL(endpoint))
	if err != nil {
		return nil, err
	}
	res := resource.NewWithAttributes(semconv.SchemaURL,
		semconv.ServiceName(Service), semconv.ServiceVersion(version), semconv.DeploymentEnvironment(env))
	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exp), sdktrace.WithResource(res))
	otel.SetTracerProvider(tp)
	return tp.Shutdown, nil
}

// Pusher pushes samples to VictoriaMetrics' /api/v1/import/prometheus. A failed push is logged and
// dropped: telemetry never stops a run.
type Pusher struct {
	URL     string // VictoriaMetrics base; "" disables
	Env     string
	Version string
	Log     *slog.Logger
	HTTP    *http.Client
}

func (p *Pusher) push(lines []string) {
	if p.URL == "" || len(lines) == 0 {
		return
	}
	c := p.HTTP
	if c == nil {
		c = &http.Client{Timeout: 8 * time.Second}
	}
	resp, err := c.Post(p.URL+"/api/v1/import/prometheus", "text/plain", bytes.NewBufferString(strings.Join(lines, "\n")+"\n"))
	if err != nil {
		p.Log.Warn("metric push failed", "err", err)
		return
	}
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		p.Log.Warn("metric push failed", "status", resp.StatusCode)
	}
}

func (p *Pusher) sample(name string, labels map[string]string, v float64) string {
	l := map[string]string{"service": Service, "environment": p.Env}
	for k, val := range labels {
		l[k] = val
	}
	keys := make([]string, 0, len(l))
	for k := range l {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s=%q", k, l[k])
	}
	return fmt.Sprintf("%s{%s} %g", name, strings.Join(parts, ","), v)
}

// Run records one run: flotilla_run (1 per run; count with count_over_time), its duration, and the
// job's spend so far today. A skipped run has no duration or spend to report.
func (p *Pusher) Run(job, status, stage string, durationS, spendDayUSD float64) {
	lines := []string{p.sample("flotilla_run", map[string]string{"job": job, "status": status, "stage": stage, "version": p.Version}, 1)}
	if status != "skipped" {
		lines = append(lines,
			p.sample("flotilla_run_seconds", map[string]string{"job": job, "stage": stage}, durationS),
			p.sample("flotilla_spend_day_usd", map[string]string{"job": job}, spendDayUSD))
	}
	p.push(lines)
}

// LastSuccess pushes the time of a job's last ok run; the stale alert compares it with now.
func (p *Pusher) LastSuccess(job string, at time.Time) {
	if at.IsZero() {
		return
	}
	p.push([]string{p.sample("flotilla_job_last_success_timestamp_seconds", map[string]string{"job": job}, float64(at.Unix()))})
}

// Freshness pushes each job's declared freshness, so the stale alert needs no per-job threshold.
func (p *Pusher) Freshness(job string, d time.Duration) {
	p.push([]string{p.sample("flotilla_job_freshness_seconds", map[string]string{"job": job}, d.Seconds())})
}

// Up says flotillad is alive (pushed every minute).
func (p *Pusher) Up() {
	p.push([]string{p.sample("flotilla_up", map[string]string{"version": p.Version}, 1)})
}
