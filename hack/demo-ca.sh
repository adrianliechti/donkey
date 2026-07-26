#!/usr/bin/env bash
# DEV/DEMO ONLY — hands the registry's certificate authority to donkey:
# donkey-system/donkey-registry-ca, so donkey-server and the build engine
# trust the in-cluster registry (`--registry-ca-secret`).
#
# A real installation does the same thing with whatever CA its registry
# certificate comes from; only the source of the CA is demo-specific. (The
# CA of the https donkey serves is separate — the gateway's own, exported by
# hack/demo-gateway-ca.sh after the install.)
set -euo pipefail

CONTEXT="${1:-kind-donkey}"
OUT="${2:-.build/platform-ca.crt}"

kubectl --context "${CONTEXT}" create namespace donkey-system \
  --dry-run=client -o yaml | kubectl --context "${CONTEXT}" apply -f - >/dev/null

CA="$(kubectl --context "${CONTEXT}" -n cert-manager get secret platform-ca -o jsonpath='{.data.ca\.crt}')"
if [ -z "${CA}" ]; then
  echo "the cluster has no platform-ca secret — run task setup:kind first" >&2
  exit 1
fi

mkdir -p "$(dirname "${OUT}")"
echo "${CA}" | base64 -d > "${OUT}"

kubectl --context "${CONTEXT}" -n donkey-system create secret generic donkey-registry-ca \
  --from-file=ca.crt="${OUT}" --dry-run=client -o yaml | \
  kubectl --context "${CONTEXT}" apply -f - >/dev/null

echo "certificate authority ready — secret donkey-system/donkey-registry-ca, file ${OUT}"
