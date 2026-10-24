import unittest

from opentelemetry import trace
from opentelemetry.sdk.metrics import MeterProvider
from opentelemetry.sdk.metrics.export import InMemoryMetricReader
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.sampling import ALWAYS_ON
from opentelemetry.trace import SpanKind

from telemetry import DIMENSIONS, METRIC_NAME, OperationMetrics


class TelemetryTest(unittest.IsolatedAsyncioTestCase):
    async def test_native_spans_count_requests_and_tool_errors_once(self):
        reader = InMemoryMetricReader()
        meters = MeterProvider(metric_readers=[reader])
        traces = TracerProvider(sampler=ALWAYS_ON)
        traces.add_span_processor(OperationMetrics(meters.get_meter("test")))
        trace.set_tracer_provider(traces)
        from fastmcp import Client, FastMCP

        child = FastMCP("child")

        @child.tool
        def add(a: int, b: int) -> int:
            return a + b

        @child.tool
        def divide(a: float, b: float) -> float:
            return a / b

        server = FastMCP("test")
        server.mount(child)
        try:
            async with Client(server) as client:
                await client.list_tools()
                result = await client.call_tool("add", {"a": 3, "b": 4})
                self.assertEqual(result.data, 7)
                failed = await client.call_tool("divide", {"a": 1, "b": 0}, raise_on_error=False)
                self.assertTrue(failed.is_error)
            # A client/internal span or a non-seam SERVER span cannot inflate counts.
            tracer = traces.get_tracer("fastmcp")
            for kind in (SpanKind.CLIENT, SpanKind.INTERNAL, SpanKind.SERVER):
                with tracer.start_as_current_span("ignored", kind=kind, attributes={"mcp.method.name": "ignored"}):
                    pass
            points = [
                point
                for resource in reader.get_metrics_data().resource_metrics
                for scope in resource.scope_metrics
                for metric in scope.metrics
                if metric.name == METRIC_NAME
                for point in metric.data.data_points
            ]
            for point in points:
                self.assertLessEqual(set(point.attributes), set(DIMENSIONS))
                self.assertNotEqual(point.attributes["mcp.method.name"], "ignored")
            calls = [point for point in points if point.attributes["mcp.method.name"] == "tools/call"]
            self.assertEqual(sum(point.count for point in calls), 2)
            self.assertEqual(sum(point.count for point in calls if "error.type" in point.attributes), 1)
            self.assertEqual({point.attributes["gen_ai.tool.name"] for point in calls}, {"add", "divide"})
        finally:
            traces.shutdown()
            meters.shutdown()
