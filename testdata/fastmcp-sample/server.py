import os

from fastmcp import FastMCP
from starlette.requests import Request
from starlette.responses import JSONResponse


mcp = FastMCP(
    "Donkey FastMCP example",
    instructions="A tiny calculator used to verify Donkey's automatic MCP OAuth flow.",
)


@mcp.tool
def add(a: int, b: int) -> int:
    """Add two integers."""
    return a + b


@mcp.custom_route("/health", methods=["GET"])
async def health(_request: Request) -> JSONResponse:
    return JSONResponse({"ok": True})


if __name__ == "__main__":
    mcp.run(
        transport="http",
        host="0.0.0.0",
        port=int(os.environ.get("PORT", "8000")),
        path="/mcp",
        stateless_http=True,
        json_response=True,
    )
