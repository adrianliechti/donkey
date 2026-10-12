#!/usr/bin/env bash
# Connects this machine to the demo cluster and deploys the demo app: an
# AI-run helpdesk — a FastAPI + MCP backend on postgres/s3/openai, and a
# React dashboard in front of it.
#
# Assumes donkey is installed (task demo:setup) and `devkube connect` is
# running in another terminal.
set -euo pipefail
# shellcheck source=hack/demo-lib.sh
. "$(dirname "$0")/demo-lib.sh"

require_ai

say "connecting this machine"
# A human runs `donkey login` and signs in through the browser; the script
# mints its token straight from the cluster's session key instead.
TOKEN="$(mint_token)"
"${DONKEY}" login --server "https://${BASE}" --token "${TOKEN}" --ca-file "${CA_FILE}"

# Apps live in a space: <app>.<space>.<domain>, one namespace per space.
# The demo space holds two apps that talk to each other by plain service name.
say "deploying the backend — FastAPI + MCP helpdesk (space: demo)"
(cd "${ROOT}/examples/demo/backend" && "${DONKEY}" --yes deploy)

say "deploying the dashboard — React + Tailwind (space: demo)"
(cd "${ROOT}/examples/demo/dashboard" && "${DONKEY}" --yes deploy)

say "demo ready"
cat <<EOF

  The demo app: https://dashboard.demo.${BASE}
                Packmule Support — an AI-run helpdesk, whole demo on one
                address. Sign in on the demo identity page — you are in as
                demo@example.com. Certificates come from the gateway's own CA
                (${CA_FILE}), so your browser asks you to accept it once.
                  /api   the dashboard's own calls
                  /mcp   the same desk as tools an assistant can call
                         (tickets_queue, ticket_read, ticket_reply,
                         ticket_triage, attachment_read, desk_overview)
                Tickets live in the space's postgres, attachments in s3,
                and the AI triage / reply drafting / "Simulate a ticket"
                button run through the platform's openai proxy — all
                provisioned by the deploy, reached only via injected env.
                The backend behind it is internal: no hostname, no
                certificate, nothing on the internet — only the dashboard
                reaches it, as http://backend.
  Admin console: https://${BASE}   (the fake IdP signs you straight in)
  Identity:     http://auth.donkey-identity   (fake, approves everyone)

  Try next:     cd examples/demo/backend && donkey files ls
                (the desk's activity log, on the app's own disk)
                donkey logs -f · donkey status · donkey addon tunnel postgres
                donkey apps  (everything deployed, grouped by space)

  Tear down everything:  devkube --cluster donkey delete
EOF
