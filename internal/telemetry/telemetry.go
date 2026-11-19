// Package telemetry is flotillad's observability on the installation's conventions (RFC-0001 §6.2): JSON
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

// Logger writes JSON lines to stdout with service, environment and version on each; ERROR records
// also go to GlitchTip once Errors has been set up.
func Logger(env, version string) *slog.Logger {
	return slog.New(ErrorHandler{Handler: slog.NewJSONHandler(os.Stdout, nil)}).With("service", Service, "environment", env, "version", version)
}

// ShadowLogger is log for one shadow run: every line carries shadow=true and routine_version, so logs of a shadow are separable
// from the live routine's (a log query excludes shadow:true). The level is the line's own: a shadow's failure logs as loudly as a
// live one's, and what keeps it out of the live view is the mark, never a quieter level. (version is the build, already on every line.)
func ShadowLogger(log *slog.Logger, routineVersion int) *slog.Logger {
	return log.With("shadow", true, "routine_version", routineVersion)
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

// Pusher pushes samples to VictoriaMetrics' /api/v1/import/prometheus. A failed push (a connection
// error, a 5xx, a 429) is retried after 2 s and 5 s, then logged and dropped: telemetry never stops a
// run. Seen on the host (2026-11-08/09): the proxy's name does not resolve while a deploy recreates it,
// and the proxy or VictoriaMetrics answers 500/502 now and then; without a retry those samples were
// lost (a run's flotilla_run among them).
type Pusher struct {
	URL     string // VictoriaMetrics base; "" disables
	Env     string
	Version string
	Log     *slog.Logger
	HTTP    *http.Client
	// the waits between attempts; nil: 2 s, then 5 s
	Backoff []time.Duration
	Sleep   func(time.Duration)
}

func (p *Pusher) push(lines []string) {
	if p.URL == "" || len(lines) == 0 {
		return
	}
	c := p.HTTP
	if c == nil {
		c = &http.Client{Timeout: 8 * time.Second}
	}
	waits, sleep := p.Backoff, p.Sleep
	if waits == nil {
		waits = []time.Duration{2 * time.Second, 5 * time.Second}
	}
	if sleep == nil {
		sleep = time.Sleep
	}
	body := strings.Join(lines, "\n") + "\n"
	for attempt := 0; ; attempt++ {
		resp, err := c.Post(p.URL+"/api/v1/import/prometheus", "text/plain", bytes.NewBufferString(body))
		status := 0
		if err == nil {
			status = resp.StatusCode
			resp.Body.Close()
			if status/100 == 2 {
				return
			}
			if status/100 == 4 && status != 429 {
				p.Log.Warn("metric push refused", "status", status) // a bad sample: retrying cannot help
				return
			}
		}
		if attempt >= len(waits) {
			p.Log.Warn("metric push failed", "err", err, "status", status, "attempts", attempt+1)
			return
		}
		sleep(waits[attempt])
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

// isShadowKey says a routine key is a shadow version's ("<fleet>/<routine>@v<n>"): a loaded routine's id never holds "@v".
func isShadowKey(key string) bool { return strings.Contains(key, "@v") }

// Run records one live run: flotilla_run (1 per run; count with count_over_time), its duration, and the
// routine's spend so far today. A skipped run has no duration or spend to report. A shadow's key is refused here (ShadowRun is
// its way in): a sample under it without the shadow label would be a shadow's failure reading as a live one.
func (p *Pusher) Run(routine, status, autonomy string, durationS, spendDayUSD float64) {
	if isShadowKey(routine) {
		p.refuseShadowKey("Run", routine)
		return
	}
	lines := []string{p.sample("flotilla_run", map[string]string{"routine": routine, "status": status, "autonomy": autonomy, "version": p.Version}, 1)}
	if status != "skipped" {
		lines = append(lines,
			p.sample("flotilla_run_seconds", map[string]string{"routine": routine, "autonomy": autonomy}, durationS),
			p.sample("flotilla_spend_day_usd", map[string]string{"routine": routine}, spendDayUSD))
	}
	p.push(lines)
}

// ShadowRun records one shadow run, on its own series: the routine label is the shadow's key ("<fleet>/<routine>@v<n>"), every
// sample carries shadow="1" and routine_version, and nothing here is a freshness or last-success sample, so a failed, slow or
// stale shadow can never fire a stale alert or read as a live routine's run (the alert rules group by routine and read those two
// series). The live series are untouched.
func (p *Pusher) ShadowRun(key string, version int, status, autonomy string, durationS, spendDayUSD float64) {
	if !isShadowKey(key) {
		p.refuseShadowKey("ShadowRun", key)
		return
	}
	mark := map[string]string{"routine": key, "shadow": "1", "routine_version": fmt.Sprint(version)}
	with := func(extra map[string]string) map[string]string {
		l := map[string]string{}
		for k, v := range mark {
			l[k] = v
		}
		for k, v := range extra {
			l[k] = v
		}
		return l
	}
	lines := []string{p.sample("flotilla_run", with(map[string]string{"status": status, "autonomy": autonomy, "version": p.Version}), 1)}
	if status != "skipped" {
		lines = append(lines,
			p.sample("flotilla_run_seconds", with(map[string]string{"autonomy": autonomy}), durationS),
			p.sample("flotilla_spend_day_usd", with(nil), spendDayUSD))
	}
	p.push(lines)
}

func (p *Pusher) refuseShadowKey(fn, key string) {
	if p.Log != nil {
		p.Log.Error("a metric was asked for under the wrong kind of routine key; dropped", "func", fn, "routine", key)
	}
}

// StoreError records one store write that failed but did not fail its caller (a run's model calls, what it
// read, a case's spend, cleared incidents): flotilla_store_error{op} 1 per failure, counted with
// count_over_time, so a write that keeps failing is seen and not only logged.
func (p *Pusher) StoreError(op string) {
	p.push([]string{p.sample("flotilla_store_error", map[string]string{"op": op}, 1)})
}

// LastSuccess pushes the time of a routine's last ok run; the stale alert compares it with now.
func (p *Pusher) LastSuccess(routine string, at time.Time) {
	if at.IsZero() || isShadowKey(routine) { // a shadow's success is nobody's freshness
		return
	}
	p.push([]string{p.sample("flotilla_routine_last_success_timestamp_seconds", map[string]string{"routine": routine}, float64(at.Unix()))})
}

// Freshness pushes each routine's declared freshness, so the stale alert needs no per-routine threshold.
func (p *Pusher) Freshness(routine string, d time.Duration) {
	if isShadowKey(routine) {
		return
	}
	p.push([]string{p.sample("flotilla_routine_freshness_seconds", map[string]string{"routine": routine}, d.Seconds())})
}

// Up says flotillad is alive (pushed every minute).
func (p *Pusher) Up() {
	p.push([]string{p.sample("flotilla_up", map[string]string{"version": p.Version}, 1)})
}
