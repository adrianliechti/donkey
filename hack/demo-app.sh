#!/usr/bin/env bash
# Connects this machine to the demo cluster and deploys the demo app: a
# FastAPI + MCP backend over DuckDB, and a React dashboard in front of it.
#
# Assumes donkey is installed (task demo:setup) and `devkube connect` is
# running in another terminal.
set -euo pipefail
# shellcheck source=hack/demo-lib.sh
. "$(dirname "$0")/demo-lib.sh"

require_ai

say "connecting this machine"
"${DONKEY}" login --server "https://donkey.${BASE}" --token "${TOKEN}" --ca-file "${CA_FILE}"

# Apps live in a space: <app>.<space>.<domain>, one namespace per space.
# The demo space holds two apps that talk to each other by plain service name.
say "deploying the backend — FastAPI + MCP over DuckDB (space: demo)"
(cd "${ROOT}/examples/demo/backend" && "${DONKEY}" --yes deploy)

say "deploying the dashboard — React + Tailwind (space: demo)"
(cd "${ROOT}/examples/demo/dashboard" && "${DONKEY}" --yes deploy)

say "demo ready"
cat <<EOF

  The demo app: https://dashboard.demo.${BASE}
                One address for the whole thing. Sales figures read live from
                DuckDB; sign in on the demo identity page — you are in as
                demo@example.com. Certificates come from the cluster's own CA
                (${CA_FILE}), so your browser asks you to accept it once.
                  /api   the dashboard's own calls
                  /mcp   the same data as tools an assistant can call
                         (sales_summary, revenue_by_region, top_products,
                         run_sql)
                The backend behind it is internal: no hostname, no certificate,
                nothing on the internet — only the dashboard reaches it, as
                http://backend.
  Admin console: https://donkey.${BASE}   (sign in with the API token)
  Identity:     http://auth.donkey-identity   (fake, approves everyone)

  Try next:     cd examples/demo/backend && donkey files ls   (the DuckDB file)
                donkey logs -f · donkey status · donkey env set GREETING=hello
                donkey apps  (everything deployed, grouped by space)

  Tear down everything:  devkube --cluster donkey delete
EOF
