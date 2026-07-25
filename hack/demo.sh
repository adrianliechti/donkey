#!/usr/bin/env bash
# Spins up the complete donkey demo on this machine. THIS is the only place a
# cluster gets created and the only place demo pieces (the fake identity
# provider) come in — real installations run `donkey server install` against
# an existing cluster with a real identity provider.
#
# The cluster and donkey's prerequisites come from devkube; donkey itself is
# installed with the very same command an admin would run.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
DONKEY="${ROOT}/bin/donkey"
CLUSTER="donkey"
CTX="kind-${CLUSTER}"
BASE="apps.test"
CA_FILE="${ROOT}/.build/platform-ca.crt"
TOKEN="${DONKEY_TOKEN:-dk_demo_local}"
case "$(uname -m)" in
  arm64|aarch64) PLATFORM="linux/arm64" ;;
  *)             PLATFORM="linux/amd64" ;;
esac

# donkey reads every app with its assistant, so the demo needs a model: an
# OpenAI-compatible proxy on host port 8080 (override with OPENAI_BASE_URL /
# OPENAI_API_KEY). Without one, nothing would deploy — so stop here.
AI_BASE="${OPENAI_BASE_URL:-http://host.docker.internal:8080/v1}"
AI_KEY="${OPENAI_API_KEY:-local}"
if [ -z "${OPENAI_BASE_URL:-}" ] && ! nc -z -w 2 127.0.0.1 8080 2>/dev/null; then
  cat >&2 <<'MSG'
No OpenAI-compatible endpoint on host port 8080.

donkey reads every app with its assistant (ports, health routes, data folders),
so the demo cannot deploy without one. Start your proxy on port 8080, or point
the demo elsewhere:

  OPENAI_BASE_URL=https://api.openai.com/v1 OPENAI_API_KEY=sk-… task demo
MSG
  exit 1
fi

say() { printf '\n\033[1m== %s ==\033[0m\n' "$*"; }

say "cluster + prerequisites (devkube: cert-manager, registry, Envoy Gateway)"
"${ROOT}/hack/cluster.sh"

say "building donkey + images"
(cd "${ROOT}" && task build && task server-image && task demo-idp-image)

say "demo identity provider (fake, auto-login)"
kubectl --context "${CTX}" apply -f "${ROOT}/hack/demo/fake-idp.yaml"
kubectl --context "${CTX}" -n donkey-identity rollout restart deployment fake-idp >/dev/null 2>&1 || true

say "the cluster's certificate authority (registry + app certificates)"
"${ROOT}/hack/demo-ca.sh" "${CTX}" "${CA_FILE}"

say "installing the control plane (real install command, demo settings)"
"${DONKEY}" --yes --context "${CTX}" server install \
  --image "registry.platform/donkey/donkey-server:dev" \
  --registry-url "registry.platform/donkey" \
  --registry-ca-secret "donkey-registry-ca" \
  --base-domain "${BASE}" \
  --gateway-service-type "ClusterIP" \
  --tls-issuer "platform" \
  --default-platform "${PLATFORM}" \
  --oidc-issuer "http://auth.donkey-identity" \
  --oidc-client-id "donkey-demo" \
  --oidc-client-secret "donkey-demo-secret" \
  --openai-base-url "${AI_BASE}" \
  --openai-api-key "${AI_KEY}" \
  --api-token "${TOKEN}"

cat <<EOF

To reach the cluster from this machine, open a second terminal and leave this
running:

  sudo devkube --cluster donkey connect

It watches the HTTPRoutes donkey creates and keeps every donkey hostname in
your /etc/hosts, cleaning them up when you stop it. Then come back and press
enter here.
EOF
read -r _

say "connecting this machine"
"${DONKEY}" login --server "https://donkey.${BASE}" --token "${TOKEN}" --ca-file "${CA_FILE}"

# Apps live in a space: <app>.<space>.<domain>, one namespace per space.
# The demo space holds two apps that talk to each other by plain service name.
say "deploying the backend — FastAPI + MCP over DuckDB (space: demo)"
cd "${ROOT}/testdata/demo/backend"
"${DONKEY}" --yes deploy

say "deploying the dashboard — React + Tailwind (space: demo)"
cd "${ROOT}/testdata/demo/dashboard"
"${DONKEY}" --yes deploy

say "demo ready"
cat <<EOF

  Dashboard:    https://dashboard.demo.${BASE}
                Sales figures read live from DuckDB. Sign in on the demo
                identity page — you are in as demo@example.com. Certificates
                come from the cluster's own CA (${CA_FILE}), so your browser
                asks you to accept it once.
  MCP endpoint: https://backend.demo.${BASE}/mcp
                The same data as tools an assistant can call (sales_summary,
                revenue_by_region, top_products, run_sql). No login: MCP
                clients are machines, so the backend is `public: true`.
  Admin console: https://donkey.${BASE}   (sign in with the API token)
  Identity:     http://auth.donkey-identity   (fake, approves everyone)

  Try next:     cd testdata/demo/backend && donkey files ls   (the DuckDB file)
                donkey logs -f · donkey status · donkey env set GREETING=hello
                donkey apps  (everything deployed, grouped by space)

  Tear down everything:  devkube --cluster donkey delete
EOF
