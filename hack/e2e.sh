#!/usr/bin/env bash
# Server-mode end-to-end test on a local kind cluster:
#   server install → login → deploy (in-cluster build) → login gateway
#   redirect → public app → postgres addon → env vars → destroy.
# Uses an isolated HOME so the developer's real config/kubeconfig is untouched.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
DONKEY="${ROOT}/bin/donkey"

say() { printf '\n\033[1m== %s ==\033[0m\n' "$*"; }

WORK="$(mktemp -d)"
# Isolate the kubeconfig FIRST (kind-up writes into it), but keep the real
# HOME while building so go uses its normal caches.
export KUBECONFIG="${WORK}/kubeconfig"

say "cluster + registry"
"${ROOT}/hack/kind-up.sh"
REG_IP="$(docker inspect -f '{{(index .NetworkSettings.Networks "kind").IPAddress}}' donkey-registry)"

say "binaries + images"
(cd "${ROOT}" && task build && task server-image && task demo-idp-image)

export HOME="${WORK}/home"
mkdir -p "${HOME}"

case "$(uname -m)" in
  arm64|aarch64) PLATFORM="linux/arm64" ;;
  *)             PLATFORM="linux/amd64" ;;
esac

BASE="127.0.0.1.nip.io"
TOKEN="dk_e2e_$(date +%s)"

cleanup() {
  chmod -R u+w "${WORK}" 2>/dev/null || true
  rm -rf "${WORK}"
}
trap cleanup EXIT

wait_http() {
  local url="$1" want="$2"
  for _ in $(seq 1 30); do
    if curl -fskS --max-time 5 "${url}" 2>/dev/null | grep -q "${want}"; then
      return 0
    fi
    sleep 2
  done
  echo "FAIL: ${url} never served ${want}; last response:" >&2
  curl -skS --max-time 5 "${url}" 2>&1 | head -5 >&2 || true
  return 1
}

say "demo identity provider (fake, auto-login)"
kubectl --context kind-donkey apply -f "${ROOT}/hack/demo/fake-idp.yaml"
kubectl --context kind-donkey -n donkey-demo rollout restart deployment fake-idp >/dev/null 2>&1 || true

say "demo TLS certificate (apps are served over https — OIDC cookies need it)"
kubectl --context kind-donkey create namespace donkey-system --dry-run=client -o yaml | kubectl --context kind-donkey apply -f - >/dev/null
if ! kubectl --context kind-donkey -n donkey-system get secret donkey-gateway-tls >/dev/null 2>&1; then
  openssl req -x509 -newkey rsa:2048 -keyout "${WORK}/tls.key" -out "${WORK}/tls.crt" -days 30 -nodes \
    -subj "/CN=*.${BASE}" -addext "subjectAltName=DNS:*.${BASE},DNS:${BASE}" >/dev/null 2>&1
  kubectl --context kind-donkey -n donkey-system create secret tls donkey-gateway-tls \
    --cert="${WORK}/tls.crt" --key="${WORK}/tls.key"
fi

say "server install (real command, gateway → demo IdP)"
"${DONKEY}" --yes --context kind-donkey server install \
  --image "host.docker.internal:5001/donkey/donkey-server:dev" \
  --registry-url "${REG_IP}:5000/donkey" \
  --registry-insecure \
  --base-domain "${BASE}" \
  --https \
  --default-platform "${PLATFORM}" \
  --oidc-issuer "http://id.${BASE}" \
  --oidc-client-id "donkey-demo" \
  --oidc-client-secret "donkey-demo-secret" \
  --openai-base-url "http://host.docker.internal:8080/v1" \
  --openai-api-key "none" \
  --api-token "${TOKEN}"

say "expose the gateway on host ports + demo DNS"
"${ROOT}/hack/demo-expose.sh" kind-donkey
"${ROOT}/hack/demo-dns.sh" kind-donkey "${BASE}"

# Dev loop: the :dev image tag is mutable — force the server onto the image
# just pushed.
kubectl --context kind-donkey -n donkey-system rollout restart deployment donkey-server
kubectl --context kind-donkey -n donkey-system rollout status deployment donkey-server --timeout=180s

say "login"
"${DONKEY}" login --server "http://donkey.${BASE}" --token "${TOKEN}"

say "doctor"
"${DONKEY}" --yes doctor

say "identity provider is up"
wait_http "http://id.${BASE}/.well-known/openid-configuration" 'authorization_endpoint'
echo "fake-idp answers"

say "deploy fastapi sample (protected by default)"
APP="${WORK}/app"
cp -R "${ROOT}/testdata/fastapi-sample" "${APP}"
cat > "${APP}/donkey.yaml" <<'EOF'
name: e2e-fastapi
port: 8000
healthcheck:
  path: /healthz
EOF
cd "${APP}"
"${DONKEY}" --yes deploy

say "login protection: unauthenticated requests are sent to the IdP"
# Envoy needs a moment to program the SecurityPolicy — retry.
LOCATION=""
for _ in $(seq 1 45); do
  LOCATION="$(curl -sk -o /dev/null -w '%{redirect_url}' "https://e2e-fastapi.${BASE}/healthz")"
  echo "${LOCATION}" | grep -q "id.${BASE}" && break
  sleep 2
done
echo "redirects to: ${LOCATION}"
echo "${LOCATION}" | grep -q "id.${BASE}" || {
  echo "FAIL: expected a redirect to the identity provider" >&2; exit 1;
}
echo "protected app redirects to the IdP login"

say "full login round-trip (demo IdP with sign-in button)"
JAR="${WORK}/cookies.txt"
LOGIN_OK=""
for _ in $(seq 1 20); do
  rm -f "${JAR}"
  AUTH="$(curl -sk -c "${JAR}" -o /dev/null -w '%{redirect_url}' --max-time 15 "https://e2e-fastapi.${BASE}/healthz")"
  # "Click" the sign-in button: /authorize renders it, /approve is its link.
  APPROVE="${AUTH/\/authorize?//approve?}"
  if [ "${APPROVE}" != "${AUTH}" ] && curl -sk -L -c "${JAR}" -b "${JAR}" --max-time 20 "${APPROVE}" | grep -q '"ok":true'; then
    LOGIN_OK=yes
    break
  fi
  sleep 3
done
[ -n "${LOGIN_OK}" ] || { echo "FAIL: could not reach the app through the login flow" >&2; exit 1; }
echo "login flow completes (button click) and the app answers"

say "public app opt-out"
cat > "${APP}/donkey.yaml" <<'EOF'
name: e2e-fastapi
port: 8000
public: true
healthcheck:
  path: /healthz
EOF
"${DONKEY}" --yes deploy
wait_http "https://e2e-fastapi.${BASE}/healthz" '"ok":true'
echo "public app serves without login"

say "postgres addon"
"${DONKEY}" --yes addon add postgres
wait_http "https://e2e-fastapi.${BASE}/db-check" '"database":"ok"'
echo "DATABASE_URL binding OK"

say "env vars"
"${DONKEY}" --yes env set GREETING=hi-from-e2e
wait_http "https://e2e-fastapi.${BASE}/" 'hi-from-e2e'
echo "env var visible in app"

say "destroy"
"${DONKEY}" --yes destroy
for i in $(seq 1 60); do
  kubectl --context kind-donkey get namespace donkey-e2e-fastapi >/dev/null 2>&1 || break
  sleep 2
done
if kubectl --context kind-donkey get namespace donkey-e2e-fastapi >/dev/null 2>&1; then
  echo "namespace still present after destroy" >&2
  exit 1
fi
echo "namespace gone"

say "e2e passed"
