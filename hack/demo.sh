#!/usr/bin/env bash
# Spins up the complete donkey demo on this machine. THIS is the only place
# a cluster gets created and the only place demo pieces (Pocket ID, nip.io
# DNS) come in — real installations run `donkey server install` against an
# existing cluster with a real identity provider.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
DONKEY="${ROOT}/bin/donkey"
BASE="127.0.0.1.nip.io"
CTX="kind-donkey"
TOKEN="${DONKEY_TOKEN:-dk_demo_local}"
case "$(uname -m)" in
  arm64|aarch64) PLATFORM="linux/arm64" ;;
  *)             PLATFORM="linux/amd64" ;;
esac

# AI endpoint for the demo: an OpenAI-compatible proxy on host port 8080
# (override with OPENAI_BASE_URL / OPENAI_API_KEY).
AI_BASE="${OPENAI_BASE_URL:-http://host.docker.internal:8080/v1}"
AI_KEY="${OPENAI_API_KEY:-none}"
if [ -z "${OPENAI_BASE_URL:-}" ] && ! nc -z -w 2 127.0.0.1 8080 2>/dev/null; then
  echo "AI: no proxy on host port 8080 — assistance will be inactive until one runs there"
fi

say() { printf '\n\033[1m== %s ==\033[0m\n' "$*"; }

say "demo cluster (kind) + registry + ingress"
"${ROOT}/hack/kind-up.sh"
REG_IP="$(docker inspect -f '{{(index .NetworkSettings.Networks "kind").IPAddress}}' donkey-registry)"

say "building donkey + images"
(cd "${ROOT}" && task build && task server-image && task demo-idp-image)

say "demo identity provider (fake, auto-login)"
kubectl --context "${CTX}" apply -f "${ROOT}/hack/demo/fake-idp.yaml"
kubectl --context "${CTX}" -n donkey-demo rollout restart deployment fake-idp >/dev/null 2>&1 || true

say "demo TLS certificate (apps are served over https — OIDC cookies need it)"
kubectl --context "${CTX}" create namespace donkey-system --dry-run=client -o yaml | kubectl --context "${CTX}" apply -f - >/dev/null
if ! kubectl --context "${CTX}" -n donkey-system get secret donkey-gateway-tls >/dev/null 2>&1; then
  TLSDIR="$(mktemp -d)"
  openssl req -x509 -newkey rsa:2048 -keyout "${TLSDIR}/tls.key" -out "${TLSDIR}/tls.crt" -days 30 -nodes \
    -subj "/CN=*.${BASE}" -addext "subjectAltName=DNS:*.${BASE},DNS:${BASE}" >/dev/null 2>&1
  kubectl --context "${CTX}" -n donkey-system create secret tls donkey-gateway-tls \
    --cert="${TLSDIR}/tls.crt" --key="${TLSDIR}/tls.key"
  rm -rf "${TLSDIR}"
fi

say "installing the control plane (real install command, demo settings)"
"${DONKEY}" --yes --context "${CTX}" server install \
  --image "host.docker.internal:5001/donkey/donkey-server:dev" \
  --registry-url "${REG_IP}:5000/donkey" \
  --registry-insecure \
  --base-domain "${BASE}" \
  --https \
  --default-platform "${PLATFORM}" \
  --oidc-issuer "http://id.${BASE}" \
  --oidc-client-id "donkey-demo" \
  --oidc-client-secret "donkey-demo-secret" \
  --openai-base-url "${AI_BASE}" \
  --openai-api-key "${AI_KEY}" \
  --api-token "${TOKEN}"
say "exposing the gateway on host ports + demo DNS"
"${ROOT}/hack/demo-expose.sh" "${CTX}"
"${ROOT}/hack/demo-dns.sh" "${CTX}" "${BASE}"
kubectl --context "${CTX}" -n donkey-system rollout restart deployment donkey-server >/dev/null
kubectl --context "${CTX}" -n donkey-system rollout status deployment donkey-server --timeout=180s >/dev/null

say "connecting this machine"
"${DONKEY}" login --server "http://donkey.${BASE}" --token "${TOKEN}"

say "deploying the sample app"
cd "${ROOT}/testdata/fastapi-sample"
"${DONKEY}" --yes deploy

say "demo ready"
cat <<EOF

  Sample app:   https://fastapi-sample.${BASE}
                Open it (accept the self-signed certificate warning),
                click "Sign in" on the demo identity page — and you're
                in as demo@example.com.
  Donkey API:   http://donkey.${BASE}
  Identity:     http://id.${BASE}   (fake, approves everyone)

  Try next:     cd testdata/fastapi-sample && donkey addon add postgres
                donkey env set GREETING=hello · donkey logs -f · donkey status

  Tear down everything:  kind delete cluster --name donkey && docker rm -f donkey-registry
EOF
