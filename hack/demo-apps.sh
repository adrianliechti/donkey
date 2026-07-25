#!/usr/bin/env bash
# Deploys the sophisticated sample — the FastMCP + React monorepo — to a
# running donkey (set one up first with `task demo`, or `donkey login`
# against any server).
#
# The constellation demonstrates both access modes:
#   frontend  — protected by the login gateway; its own little server (a
#               BFF, server.js) proxies /api/* to the backend in-cluster,
#               so the browser never needs to reach the backend host.
#   backend   — public: it exposes an MCP endpoint, and MCP clients are
#               machines that can't do a browser login.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
DONKEY="${ROOT}/bin/donkey"
BASE="127.0.0.1.nip.io"

say() { printf '\n\033[1m== %s ==\033[0m\n' "$*"; }

say "MCP backend (FastMCP, Python)"
(cd "${ROOT}/testdata/mcp-react-sample/backend" && "${DONKEY}" --yes deploy)

say "React frontend (Vite)"
(cd "${ROOT}/testdata/mcp-react-sample/frontend" && "${DONKEY}" --yes deploy)

say "sample monorepo ready"
cat <<INFO

  Frontend:     https://frontend.${BASE}
                Behind the login gateway (sign in as demo@example.com).
                Shows backend connectivity fetched through its BFF.
  MCP server:   http://backend.${BASE}/mcp/
                Public — a real Model Context Protocol endpoint for MCP
                clients; health at http://backend.${BASE}/healthz
INFO
