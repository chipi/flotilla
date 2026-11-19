"""Tracing for a routine's own process (ADR-0003, ADR-0004): the model client's spans, under the run's trace.

flotillad starts every run inside a span and hands the routine TRACEPARENT, FLOTILLA_OTLP_TRACES_URL (its own
otlp_traces_url, reached through the egress proxy), FLOTILLA_ENVIRONMENT and FLOTILLA_ROUTINE. setup() makes
the OpenTelemetry SDK export to that URL, with the run's span as the parent, so a routine's model calls show
up under the run in VictoriaTraces. Without the SDK (a laptop) or without a URL it does nothing: the
routine runs the same and only the spans are missing.

    session = telemetry.setup()      # once, at the start of the routine
    ...                              # model_client.Client() picks the tracer up
    session.shutdown()               # flushes; also registered at exit
"""
import atexit
import os


class Session:
    def __init__(self, provider=None, token=None, why=""):
        self.provider, self.token, self.why = provider, token, why

    @property
    def active(self):
        return self.provider is not None

    def shutdown(self):
        if self.provider is None:
            return
        try:
            self.provider.force_flush(timeout_millis=5000)
            self.provider.shutdown()
        finally:
            if self.token is not None:
                from opentelemetry import context
                context.detach(self.token)
            self.provider, self.token = None, None


def run_marks(environ):
    """What every span of the run carries beyond the run's identity: the routine's version (when flotillad said it), and for a shadow
    run (FLOTILLA_SHADOW=1) flotilla.shadow=true, so a trace query can leave shadows out. A live run has no shadow attribute."""
    out = {}
    v = str(environ.get("FLOTILLA_ROUTINE_VERSION", ""))
    if v.isdigit():
        out["flotilla.version"] = int(v)
    if environ.get("FLOTILLA_SHADOW") == "1":
        out["flotilla.shadow"] = True
    return out


def resource_attrs(environ):
    """The resource every span of the run carries: who and where, the run's identity, and run_marks."""
    routine = environ.get("FLOTILLA_ROUTINE", "")
    attrs = {
        "service.name": "flotilla-" + (routine.split("/")[0] or "routine"),
        "service.version": environ.get("FLOTILLA_VERSION", "dev"),
        "deployment.environment": environ.get("FLOTILLA_ENVIRONMENT", ""),
        "flotilla.routine": routine, "flotilla.run_id": environ.get("FLOTILLA_RUN_ID", "")}
    attrs.update(run_marks(environ))
    return attrs


def setup(environ=None, exporter=None):
    """Configure tracing for this process. `exporter` replaces the OTLP exporter (tests)."""
    environ = os.environ if environ is None else environ
    url = environ.get("FLOTILLA_OTLP_TRACES_URL", "")
    if not url and exporter is None:
        return Session(why="no FLOTILLA_OTLP_TRACES_URL")
    try:
        from opentelemetry import context, trace
        from opentelemetry.sdk.resources import Resource
        from opentelemetry.sdk.trace import TracerProvider
        from opentelemetry.sdk.trace.export import BatchSpanProcessor
        from opentelemetry.trace.propagation.tracecontext import TraceContextTextMapPropagator
    except ImportError:
        return Session(why="the OpenTelemetry SDK is not installed")
    if exporter is None:
        from opentelemetry.exporter.otlp.proto.http.trace_exporter import OTLPSpanExporter
        exporter = OTLPSpanExporter(endpoint=url, timeout=5)
    provider = TracerProvider(resource=Resource.create(resource_attrs(environ)))
    provider.add_span_processor(BatchSpanProcessor(exporter))
    trace.set_tracer_provider(provider)
    token = None
    if environ.get("TRACEPARENT"):
        token = context.attach(TraceContextTextMapPropagator().extract({"traceparent": environ["TRACEPARENT"]}))
    s = Session(provider, token)
    atexit.register(s.shutdown)
    return s
