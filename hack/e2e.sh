#!/usr/bin/env bash
# Server-mode end-to-end test on a local devkube cluster:
#   server install → login → deploy (in-cluster build) → certificate →
#   login gateway redirect → full login round-trip → public app →
#   postgres addon → env vars → spaces → persistent storage → destroy.
#
# Apps live in a space: one namespace, <app>.<space>.<domain>, certificates
# from cert-manager. The test needs no sudo and no DNS anywhere: the CLI talks
# to the API through a port-forward, and every app request comes from a curl
# pod inside the cluster that resolves the hostnames to the gateway itself.
# That also checks the certificate chain cert-manager issued, which a
# `curl -k` never would.
#
# Uses an isolated HOME so the developer's real config is untouched.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
DONKEY="${ROOT}/bin/donkey"
CLUSTER="donkey"
CTX="kind-${CLUSTER}"
BASE="apps.test"

say() { printf '\n\033[1m== %s ==\033[0m\n' "$*"; }

WORK="$(mktemp -d)"
CA_FILE="${WORK}/platform-ca.crt"

# The assistant is part of every deploy, so the test needs a real endpoint.
AI_BASE="${OPENAI_BASE_URL:-http://host.docker.internal:8080/v1}"
AI_KEY="${OPENAI_API_KEY:-local}"
if [ -z "${OPENAI_BASE_URL:-}" ] && ! nc -z -w 2 127.0.0.1 8080 2>/dev/null; then
  echo "FAIL: no OpenAI-compatible endpoint on host port 8080 — donkey needs one to read apps." >&2
  echo "      Start your proxy, or set OPENAI_BASE_URL / OPENAI_API_KEY." >&2
  exit 1
fi

FORWARD_PID=""
cleanup() {
  if [ -n "${FORWARD_PID}" ]; then
    kill "${FORWARD_PID}" 2>/dev/null || true
  fi
  kubectl --context "${CTX}" -n donkey-identity delete pod e2e-client --ignore-not-found --wait=false >/dev/null 2>&1 || true
  chmod -R u+w "${WORK}" 2>/dev/null || true
  rm -rf "${WORK}"
}
trap cleanup EXIT

# curl_app runs inside the cluster and points every hostname at the gateway,
# so no DNS (and no /etc/hosts) is involved — while the certificate is still
# verified against the cluster's CA.
curl_app() {
  kubectl --context "${CTX}" -n donkey-identity exec e2e-client -- \
    curl --cacert /ca/ca.crt --resolve "*:443:${GATEWAY_IP}" "$@"
}

wait_http() {
  local url="$1" want="$2"
  for _ in $(seq 1 30); do
    if curl_app -fsS --max-time 5 "${url}" 2>/dev/null | grep -q "${want}"; then
      return 0
    fi
    sleep 2
  done
  echo "FAIL: ${url} never served ${want}; last response:" >&2
  curl_app -sS --max-time 5 "${url}" 2>&1 | head -5 >&2 || true
  return 1
}

say "cluster + prerequisites (devkube: cert-manager, registry, Envoy Gateway)"
"${ROOT}/hack/cluster.sh"

say "binaries + images"
(cd "${ROOT}" && task build && task server-image && task demo-idp-image)

# donkey's own config gets an isolated HOME; the cluster connection stays the
# developer's (devkube maintains the real kubeconfig).
export KUBECONFIG="${KUBECONFIG:-${HOME}/.kube/config}"
export HOME="${WORK}/home"
mkdir -p "${HOME}"

case "$(uname -m)" in
  arm64|aarch64) PLATFORM="linux/arm64" ;;
  *)             PLATFORM="linux/amd64" ;;
esac

say "demo identity provider (fake, auto-login; issuer = its service name)"
kubectl --context "${CTX}" apply -f "${ROOT}/hack/demo/fake-idp.yaml"
kubectl --context "${CTX}" -n donkey-identity rollout restart deployment fake-idp >/dev/null 2>&1 || true

say "the cluster's certificate authority"
"${ROOT}/hack/demo-ca.sh" "${CTX}" "${CA_FILE}"

say "server install (real command, gateway → demo IdP)"
"${DONKEY}" --yes --context "${CTX}" server install \
  --image "registry.platform/donkey/donkey-server:dev" \
  --registry-url "registry.platform/donkey" \
  --registry-ca-secret "donkey-registry-ca" \
  --domain "${BASE}" \
  --gateway-service-type "ClusterIP" \
  --tls-issuer "platform" \
  --default-platform "${PLATFORM}" \
  --oidc-issuer "http://auth.donkey-identity" \
  --oidc-client-id "donkey-demo" \
  --oidc-client-secret "donkey-demo-secret" \
  --openai-base-url "${AI_BASE}" \
  --openai-api-key "${AI_KEY}"

# Dev loop: the :dev image tag is mutable — force the server onto the image
# just pushed.
kubectl --context "${CTX}" -n donkey-system rollout restart deployment donkey-server
kubectl --context "${CTX}" -n donkey-system rollout status deployment donkey-server --timeout=180s

say "in-cluster test client"
GATEWAY_IP="$(kubectl --context "${CTX}" -n platform get svc \
  -l gateway.envoyproxy.io/owning-gateway-name=donkey -o jsonpath='{.items[0].spec.clusterIP}')"
[ -n "${GATEWAY_IP}" ] || { echo "FAIL: the donkey gateway has no service yet" >&2; exit 1; }
kubectl --context "${CTX}" -n donkey-identity create configmap e2e-ca \
  --from-file=ca.crt="${CA_FILE}" --dry-run=client -o yaml | \
  kubectl --context "${CTX}" apply -f - >/dev/null
kubectl --context "${CTX}" -n donkey-identity delete pod e2e-client --ignore-not-found >/dev/null
kubectl --context "${CTX}" -n donkey-identity run e2e-client --image=curlimages/curl:latest --restart=Never \
  --overrides='{"spec":{"volumes":[{"name":"ca","configMap":{"name":"e2e-ca"}}],"containers":[{"name":"e2e-client","image":"curlimages/curl:latest","command":["sleep","infinity"],"volumeMounts":[{"name":"ca","mountPath":"/ca"}]}]}}' >/dev/null
kubectl --context "${CTX}" -n donkey-identity wait --for=condition=Ready pod/e2e-client --timeout=120s
echo "test client ready (gateway ${GATEWAY_IP})"

say "login (through a port-forward — no DNS, no sudo)"
kubectl --context "${CTX}" -n donkey-system port-forward svc/donkey-server 18088:80 >"${WORK}/forward.log" 2>&1 &
FORWARD_PID=$!
for _ in $(seq 1 30); do
  curl -fsS --max-time 2 "http://127.0.0.1:18088/healthz" >/dev/null 2>&1 && break
  sleep 1
done
# A human signs in through the browser; the test mints its token straight
# from the cluster's session key — the same sealed format the gateway hands
# a real `donkey login`, verified by the server on this direct path.
TOKEN="$(DONKEY_SESSION_KEY="$(kubectl --context "${CTX}" -n donkey-system get secret donkey-system-config \
  -o jsonpath='{.data.session-key}' | base64 -d)" go run "${ROOT}/hack/mint-token")"
"${DONKEY}" login --server "http://127.0.0.1:18088" --token "${TOKEN}"

say "doctor"
"${DONKEY}" --yes doctor

say "identity provider is up"
wait_http "http://auth.donkey-identity/.well-known/openid-configuration" 'authorization_endpoint'
echo "fake-idp answers"

say "deploy fastapi sample (space e2e, protected by default)"
APP="${WORK}/app"
cp -R "${ROOT}/testdata/fastapi-sample" "${APP}"
cat > "${APP}/donkey.yaml" <<'EOF'
name: e2e-fastapi
space: e2e
port: 8000
healthcheck:
  path: /healthz
EOF
cd "${APP}"
"${DONKEY}" --yes deploy

say "certificate issued by cert-manager"
kubectl --context "${CTX}" -n donkey-system wait --for=condition=Ready \
  certificate/donkey-tls-e2e-e2e-fastapi --timeout=120s
echo "the app has its own certificate"

say "login protection: unauthenticated requests are sent to the IdP"
# Envoy needs a moment to program the SecurityPolicy — retry.
LOCATION=""
for _ in $(seq 1 45); do
  LOCATION="$(curl_app -s -o /dev/null -w '%{redirect_url}' "https://e2e-fastapi.e2e.${BASE}/healthz")"
  echo "${LOCATION}" | grep -q "auth.donkey-identity" && break
  sleep 2
done
echo "redirects to: ${LOCATION}"
echo "${LOCATION}" | grep -q "auth.donkey-identity" || {
  echo "FAIL: expected a redirect to the identity provider" >&2; exit 1;
}
echo "protected app redirects to the IdP login"

say "full login round-trip (demo IdP with sign-in button)"
LOGIN_OK=""
for _ in $(seq 1 20); do
  # The whole round-trip happens in the client pod: fetch the redirect, "click"
  # the sign-in button (/authorize renders it, /approve is its link), and land
  # back on the app with the session cookie.
  if kubectl --context "${CTX}" -n donkey-identity exec e2e-client -- sh -c "
      set -e
      rm -f /tmp/jar
      AUTH=\$(curl --cacert /ca/ca.crt --resolve '*:443:${GATEWAY_IP}' -s -c /tmp/jar -o /dev/null \
        -w '%{redirect_url}' --max-time 15 'https://e2e-fastapi.e2e.${BASE}/healthz')
      APPROVE=\$(echo \"\${AUTH}\" | sed 's|/authorize?|/approve?|')
      [ \"\${APPROVE}\" != \"\${AUTH}\" ]
      curl --cacert /ca/ca.crt --resolve '*:443:${GATEWAY_IP}' -s -L -c /tmp/jar -b /tmp/jar \
        --max-time 20 \"\${APPROVE}\"" | grep -q '"ok":true'; then
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
space: e2e
port: 8000
public: true
healthcheck:
  path: /healthz
EOF
"${DONKEY}" --yes deploy
wait_http "https://e2e-fastapi.e2e.${BASE}/healthz" '"ok":true'
echo "public app serves without login"

say "postgres addon"
"${DONKEY}" --yes addon add postgres
wait_http "https://e2e-fastapi.e2e.${BASE}/db-check" '"database":"ok"'
echo "DATABASE_URL binding OK"

say "env vars"
"${DONKEY}" --yes env set GREETING=hi-from-e2e
wait_http "https://e2e-fastapi.e2e.${BASE}/" 'hi-from-e2e'
echo "env var visible in app"

say "spaces overview"
APPS_OUT="$("${DONKEY}" apps)"
echo "${APPS_OUT}" | grep -q "e2e-fastapi" || {
  echo "FAIL: donkey apps should list the app under its space; got:" >&2
  echo "${APPS_OUT}" >&2; exit 1; }
kubectl --context "${CTX}" get namespace donkey-e2e >/dev/null || {
  echo "FAIL: the space should have one namespace" >&2; exit 1; }
echo "apps are grouped by space"

say "persistent storage (one disk, folder per path)"
cat > "${APP}/donkey.yaml" <<'EOF'
name: e2e-fastapi
space: e2e
port: 8000
public: true
healthcheck:
  path: /healthz
storage:
  size: 1Gi
  paths:
    - /app/data
    - /app/uploads
EOF
"${DONKEY}" --yes deploy
wait_http "https://e2e-fastapi.e2e.${BASE}/healthz" '"ok":true'
kubectl --context "${CTX}" -n donkey-e2e get pvc e2e-fastapi-data >/dev/null || {
  echo "FAIL: expected one claim for the app" >&2; exit 1; }
POD="$(kubectl --context "${CTX}" -n donkey-e2e get pod -l donkey.dev/app=e2e-fastapi -o jsonpath='{.items[0].metadata.name}')"
kubectl --context "${CTX}" -n donkey-e2e exec "${POD}" -- sh -c 'echo survives > /app/data/marker && echo also > /app/uploads/marker'
kubectl --context "${CTX}" -n donkey-e2e delete pod "${POD}" --wait=true >/dev/null
kubectl --context "${CTX}" -n donkey-e2e rollout status deployment e2e-fastapi --timeout=180s >/dev/null
POD="$(kubectl --context "${CTX}" -n donkey-e2e get pod -l donkey.dev/app=e2e-fastapi -o jsonpath='{.items[0].metadata.name}')"
kubectl --context "${CTX}" -n donkey-e2e exec "${POD}" -- cat /app/data/marker | grep -q survives || {
  echo "FAIL: data did not survive the pod" >&2; exit 1; }
kubectl --context "${CTX}" -n donkey-e2e exec "${POD}" -- cat /app/uploads/marker | grep -q also || {
  echo "FAIL: the second folder lost its data" >&2; exit 1; }
echo "both folders keep their data across a restart"

say "destroy"
"${DONKEY}" destroy --force
for _ in $(seq 1 60); do
  kubectl --context "${CTX}" get namespace donkey-e2e >/dev/null 2>&1 || break
  sleep 2
done
if kubectl --context "${CTX}" get namespace donkey-e2e >/dev/null 2>&1; then
  echo "namespace still present after destroy" >&2
  exit 1
fi
if kubectl --context "${CTX}" -n donkey-system get certificate donkey-tls-e2e-e2e-fastapi >/dev/null 2>&1; then
  echo "the app's certificate outlived it" >&2
  exit 1
fi
echo "namespace and certificate gone"

say "e2e passed"
