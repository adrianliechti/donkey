#!/usr/bin/env bash
# Runs the Go e2e suite (./e2e) against a RUNNING donkey installation:
#
#   hack/verify.sh kind      the devkube/kind install (task install:kind)
#   hack/verify.sh docker    the docker install (task install:docker)
#
# The suite itself is runtime-agnostic — it only speaks the server API and
# the gateway's https edge. This wrapper works out where those live and how
# to mint a token (the session key), then hands over to `go test`.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
RUNTIME="${1:-kind}"
if [ "$#" -gt 0 ]; then shift; fi

say() { printf '\n\033[1m== %s ==\033[0m\n' "$*"; }

FORWARD_PIDS=()
cleanup() {
  for pid in "${FORWARD_PIDS[@]:-}"; do
    kill "${pid}" 2>/dev/null || true
  done
}
trap cleanup EXIT

wait_port() {
  for _ in $(seq 1 30); do
    nc -z 127.0.0.1 "$1" 2>/dev/null && return 0
    sleep 1
  done
  echo "FAIL: nothing answering on 127.0.0.1:$1" >&2
  return 1
}

case "${RUNTIME}" in
kind)
  CTX="kind-${CLUSTER:-donkey}"
  export DONKEY_E2E_DOMAIN="${DONKEY_E2E_DOMAIN:-apps.test}"
  # Assigned before export: `export X="$(cmd)"` is one command whose status is
  # export's, so a failed kubectl would slip past `set -e` and hand the suite
  # an empty key.
  SESSION_KEY="$(kubectl --context "${CTX}" -n donkey-system get secret donkey-system-config \
    -o jsonpath='{.data.session-key}' | base64 -d)"
  [ -n "${SESSION_KEY}" ] || { echo "FAIL: no session key in donkey-system-config — is donkey installed?" >&2; exit 1; }
  export DONKEY_E2E_SESSION_KEY="${SESSION_KEY}"

  say "the gateway's certificate authority"
  CA="${ROOT}/.build/donkey-gateway-ca.crt"
  "${ROOT}/hack/demo-gateway-ca.sh" "${CTX}" "${CA}"
  export DONKEY_E2E_CA="${CA}"

  say "port-forwards (API and edge — no DNS, no sudo)"
  kubectl --context "${CTX}" -n donkey-system port-forward svc/donkey-server 18088:80 >/dev/null 2>&1 &
  FORWARD_PIDS+=($!)
  kubectl --context "${CTX}" -n donkey-system port-forward svc/donkey-gateway 18443:443 >/dev/null 2>&1 &
  FORWARD_PIDS+=($!)
  wait_port 18088
  wait_port 18443
  export DONKEY_E2E_SERVER="http://127.0.0.1:18088"
  export DONKEY_E2E_GATEWAY="127.0.0.1:18443"
  ;;

docker)
  export DONKEY_E2E_DOMAIN="${DONKEY_E2E_DOMAIN:-apps.localhost}"
  SESSION_KEY="$(docker inspect donkey-server \
    --format '{{range .Config.Env}}{{println .}}{{end}}' | sed -n 's/^DONKEY_SESSION_KEY=//p')"
  [ -n "${SESSION_KEY}" ] || { echo "FAIL: donkey-server is not running (no session key) — run task install:docker" >&2; exit 1; }
  export DONKEY_E2E_SESSION_KEY="${SESSION_KEY}"

  say "the gateway's certificate authority"
  CA="${ROOT}/.build/donkey-gateway-ca-docker.crt"
  mkdir -p "${ROOT}/.build"
  ok=""
  for _ in $(seq 1 30); do
    if docker run --rm -v donkey-data:/state busybox:1.37 cat /state/ca/ca.crt > "${CA}" 2>/dev/null && [ -s "${CA}" ]; then
      ok=yes; break
    fi
    sleep 2
  done
  [ -n "${ok}" ] || { echo "FAIL: the gateway never minted its CA — is it running with tls-mode ca?" >&2; exit 1; }
  export DONKEY_E2E_CA="${CA}"

  # Every space network must have donkey's own containers on it. Installing
  # recreates them, which drops the networks they were attached to — and
  # Docker's embedded DNS only answers for networks the asking container is
  # on, so a miss here means every app deployed before the last install is
  # unreachable until it is redeployed. The suite itself cannot catch that:
  # it deploys after installing, and deploying re-attaches.
  say "donkey's own containers are on every space network"
  for net in $(docker network ls --filter label=local.donkey.managed=true --filter label=local.donkey.installation=donkey --filter label=local.donkey.role=space --format '{{.Name}}'); do
    for own in donkey-gateway donkey-server; do
      if ! docker network inspect "${net}" --format '{{range .Containers}}{{.Name}} {{end}}' | grep -qw "${own}"; then
        echo "FAIL: ${own} is not attached to ${net} — apps in that space cannot be reached" >&2
        exit 1
      fi
    done
  done

  # The install publishes the API on the host loopback; the gateway on 443.
  export DONKEY_E2E_SERVER="http://127.0.0.1:8088"
  export DONKEY_E2E_GATEWAY="127.0.0.1:443"
  ;;

*)
  echo "usage: hack/verify.sh [kind|docker]" >&2
  exit 1
  ;;
esac

say "e2e suite (${RUNTIME})"
cd "${ROOT}"
go test -tags e2e -count=1 -timeout 60m -v "$@" ./e2e
