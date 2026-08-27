#!/usr/bin/env bash
# E2E ONLY — installs the authenticated TLS package mirror and the Docker
# config Secret consumed by donkey-packages.
set -euo pipefail
# shellcheck source=hack/demo-lib.sh
. "$(dirname "$0")/demo-lib.sh"

CERT_DIR="${ROOT}/.build/private-packages-kind"
PLATFORM_CA="${ROOT}/.build/platform-ca.crt"
BUNDLE="${ROOT}/.build/private-package-ca-bundle.crt"

(cd "${ROOT}" && go run ./hack/testcert -out "${CERT_DIR}" -hosts packages.donkey-system.svc)

kubectl --context "${CTX}" -n donkey-system create secret generic private-packages-tls \
  --from-file=tls.crt="${CERT_DIR}/tls.crt" --from-file=tls.key="${CERT_DIR}/tls.key" \
  --dry-run=client -o yaml | kubectl --context "${CTX}" apply -f - >/dev/null

kubectl --context "${CTX}" -n donkey-system create secret docker-registry package-registry-auth \
  --docker-server=oci.mirror.e2e.test --docker-username=e2e-reader --docker-password=e2e-token \
  --dry-run=client -o yaml | kubectl --context "${CTX}" apply -f - >/dev/null

cp "${PLATFORM_CA}" "${BUNDLE}"
printf '\n' >> "${BUNDLE}"
sed -n '/-----BEGIN CERTIFICATE-----/,/-----END CERTIFICATE-----/p' "${CERT_DIR}/ca.crt" >> "${BUNDLE}"
kubectl --context "${CTX}" -n donkey-system create secret generic donkey-registry-ca \
  --from-file=ca.crt="${BUNDLE}" --dry-run=client -o yaml | \
  kubectl --context "${CTX}" apply -f - >/dev/null

kubectl --context "${CTX}" apply -f "${ROOT}/hack/demo/private-packages.yaml" >/dev/null
kubectl --context "${CTX}" -n donkey-system rollout status deployment private-packages --timeout=180s >/dev/null
echo "authenticated package mirror ready — https://packages.donkey-system.svc"
