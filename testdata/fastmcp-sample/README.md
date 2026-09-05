# FastMCP on Donkey

This is a complete remote MCP server. `mode: api` asks Donkey to put OAuth
discovery, PKCE authorization and resource-bound access tokens in front
of FastMCP. The application only implements its tools; it does not contain
identity-provider or OAuth configuration.

The MCP endpoint is `/mcp` and the example tool is `add(a, b)`.
