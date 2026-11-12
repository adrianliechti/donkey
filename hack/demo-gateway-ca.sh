#!/usr/bin/env bash
# DEV/DEMO ONLY — exports the donkey gateway's own certificate authority to a
# local PEM file, so the CLI (`donkey login --ca-file`) and curl can verify
# the https the gateway serves in tls-mode "ca".
#
# The gateway mints this CA the first time it starts with TLS, so this runs
# after `donkey server install` — and waits for the Secret to appear.
set -euo pipefail

CONTEXT="${1:-kind-donkey}"
OUT="${2:-.build/donkey-gateway-ca.crt}"

CA=""
for _ in $(seq 1 60); do
  CA="$(kubectl --context "${CONTEXT}" -n donkey-system get secret donkey-gateway-ca \
    -o jsonpath='{.data.ca\.crt}' 2>/dev/null || true)"
  [ -n "${CA}" ] && break
  sleep 2
done
if [ -z "${CA}" ]; then
  echo "the gateway never minted its CA — is donkey-gateway running with DONKEY_TLS_MODE=ca?" >&2
  exit 1
fi

mkdir -p "$(dirname "${OUT}")"
echo "${CA}" | base64 -d > "${OUT}"
echo "gateway certificate authority ready — ${OUT}"
