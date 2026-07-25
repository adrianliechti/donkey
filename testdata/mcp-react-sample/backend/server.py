"""A minimal FastMCP server with a health endpoint for deployment."""

from fastmcp import FastMCP
from starlette.requests import Request
from starlette.responses import JSONResponse

mcp = FastMCP("donkey-demo")


@mcp.tool
def add(a: int, b: int) -> int:
    """Add two numbers."""
    return a + b


@mcp.tool
def greet(name: str) -> str:
    """Greet someone by name."""
    return f"Hello, {name}! Greetings from a donkey-deployed MCP server."


@mcp.custom_route("/healthz", methods=["GET"])
async def healthz(request: Request) -> JSONResponse:
    # CORS header so the React frontend can show backend status in the browser.
    return JSONResponse({"ok": True}, headers={"Access-Control-Allow-Origin": "*"})


if __name__ == "__main__":
    mcp.run(transport="http", host="0.0.0.0", port=8000)
