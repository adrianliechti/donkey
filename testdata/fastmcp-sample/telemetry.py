"""Export FastMCP's native request spans as Donkey's MCP operation metric."""

import os

from opentelemetry import metrics, trace
from opentelemetry.exporter.otlp.proto.http.metric_exporter import OTLPMetricExporter
from opentelemetry.sdk.metrics import Histogram, MeterProvider
from opentelemetry.sdk.metrics.export import AggregationTemporality, PeriodicExportingMetricReader
from opentelemetry.sdk.metrics.view import ExplicitBucketHistogramAggregation, View
from opentelemetry.sdk.resources import Resource
from opentelemetry.sdk.trace import ReadableSpan, SpanProcessor, TracerProvider
from opentelemetry.sdk.trace.sampling import ALWAYS_ON
from opentelemetry.trace import SpanKind, StatusCode


METRIC_NAME = "mcp.server.operation.duration"
BOUNDS = (0.01, 0.02, 0.05, 0.1, 0.2, 0.5, 1, 2, 5, 10, 30, 60, 120, 300)
DIMENSIONS = (
    "mcp.method.name",
    "gen_ai.tool.name",
    "gen_ai.prompt.name",
    "rpc.response.status_code",
    "error.type",
)


class OperationMetrics(SpanProcessor):
    def __init__(self, meter):
        self.duration = meter.create_histogram(
            METRIC_NAME, unit="s", description="Duration of inbound MCP requests"
        )

    def on_end(self, span: ReadableSpan) -> None:
        attrs = span.attributes or {}
        # FastMCP 4.0.2 marks its single inbound request span. Mounted-server
        # spans, client spans, notifications and provider delegation must not
        # count again. This intentionally relies on the pinned FastMCP version.
        if (
            span.instrumentation_scope is None
            or span.instrumentation_scope.name != "fastmcp"
            or span.kind != SpanKind.SERVER
            or attrs.get("fastmcp.span.seam") is not True
            or not attrs.get("mcp.method.name")
            or span.start_time is None
            or span.end_time is None
        ):
            return
        dimensions = {key: attrs[key] for key in DIMENSIONS if key in attrs}
        if span.status.status_code == StatusCode.ERROR and "error.type" not in dimensions:
            dimensions["error.type"] = "_OTHER"
        # No user/session/request IDs, resource URIs, arguments or results.
        self.duration.record((span.end_time - span.start_time) / 1e9, dimensions)


def configure_telemetry() -> None:
    if not (os.getenv("OTEL_EXPORTER_OTLP_ENDPOINT") or os.getenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT")):
        return
    # Resource and exporter consume Donkey's injected OTEL_* identity,
    # endpoint and authentication headers. Only metrics are exported.
    resource = Resource.create({})
    exporter = OTLPMetricExporter(
        preferred_temporality={Histogram: AggregationTemporality.DELTA}
    )
    reader = PeriodicExportingMetricReader(
        exporter, export_interval_millis=float(os.getenv("OTEL_METRIC_EXPORT_INTERVAL", "10000"))
    )
    provider = MeterProvider(
        resource=resource,
        metric_readers=[reader],
        views=[View(instrument_name=METRIC_NAME, aggregation=ExplicitBucketHistogramAggregation(BOUNDS))],
    )
    metrics.set_meter_provider(provider)
    # Every request contributes to metrics, even when its incoming trace was
    # not sampled. The SDK flushes metrics on normal process shutdown.
    tracer_provider = TracerProvider(resource=resource, sampler=ALWAYS_ON)
    tracer_provider.add_span_processor(OperationMetrics(provider.get_meter("donkey.fastmcp.sample")))
    trace.set_tracer_provider(tracer_provider)
