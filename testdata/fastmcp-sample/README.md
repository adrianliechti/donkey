# FastMCP on Donkey

This is a complete remote MCP server. `mode: api` asks Donkey to put OAuth
discovery, PKCE authorization and resource-bound access tokens in front
of FastMCP. The application only implements its tools; it does not contain
identity-provider or OAuth configuration.

The MCP endpoint is `/mcp`. Tools are `add(a, b)` and `divide(a, b)`;
`divide(1, 0)` returns an MCP tool error, even though the HTTP response succeeds.

Deploy from this folder with `donkey --space samples deploy`, then connect an
MCP client to `https://fastmcp-sample.samples.<domain>/mcp` and authorize it.
Open the app's Analytics tab to see gateway HTTP requests, authenticated callers,
and application-reported MCP operations, errors and latency.

Donkey's gateway measures HTTP traffic and verified callers automatically.
Tool names and operation outcomes come from the application. FastMCP already
creates [native OpenTelemetry spans](https://gofastmcp.com/servers/telemetry).
`telemetry.py` configures the SDK before FastMCP loads and converts its inbound
request spans into the `mcp.server.operation.duration` histogram. The adapter
uses FastMCP 4.0.2's `fastmcp.span.seam` marker to count each inbound request once,
including discovery, without counting mounted-server or client spans again.

The OTLP HTTP metric exporter uses Donkey's injected `OTEL_*` endpoint,
authentication headers and service identity. Exports run every 10 seconds
(`OTEL_METRIC_EXPORT_INTERVAL` overrides this in milliseconds). Without an OTLP
endpoint, the sample runs locally without exporting. Keep FastMCP's default
`native` telemetry mode enabled. Trace sampling is always on for accurate
operation counts; traces themselves are not exported to Donkey.

Only method, tool/prompt name, protocol error code when available, and error
type become metric attributes. Arguments, results, user/session/request IDs and
resource URIs are not exported. Gateway rejections and malformed messages that
never reach FastMCP's request dispatcher appear only in HTTP traffic. The
adapter preserves FastMCP's error classification; it does not reconstruct
JSON-RPC error codes absent from the span.

To test the adapter against the pinned FastMCP version:

```sh
python -m pip install -r requirements.txt
python -m unittest test_telemetry.py
```
