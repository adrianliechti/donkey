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

Donkey's gateway measures HTTP traffic, verified callers and MCP operations
automatically. This sample has no telemetry SDK, exporter or instrumentation.
Its Streamable HTTP transport sends SSE responses, so tool failures demonstrate
why HTTP status alone cannot describe an MCP operation's outcome.

The gateway's MCP plugin observes method and tool name, matches JSON-RPC
responses, and records operation duration and errors. It exports no arguments,
results, resource URIs or request/session IDs. The Analytics tab shows both HTTP
traffic and MCP operations without a source selector. Metrics normally arrive
within 10 seconds.

The observer supports JSON and SSE responses with bounded parsing and temporary
request correlation. It cannot see traffic that bypasses the gateway or internal
work inside a tool. See the repository's traffic telemetry documentation for
observation limits. FastMCP's optional native OTel tracing remains available for
applications that want detailed traces in their own observability backend.

The repository's live test verifies OAuth, calls both tools, and waits for new
gateway HTTP and MCP metrics:

```sh
DONKEY_E2E_MCP_SPACE=samples hack/verify.sh docker -run '^TestFastMCPTelemetry$'
```
