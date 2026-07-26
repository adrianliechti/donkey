#!/usr/bin/env bash
# Installs donkey into the demo cluster: the fake identity provider, the
# cluster's certificate authority, and the control plane itself. The install
# is the very same command an admin would run — only the settings are local.
#
# Assumes the cluster exists (hack/cluster.sh). Run `task demo:app` after this.
set -euo pipefail
# shellcheck source=hack/demo-lib.sh
. "$(dirname "$0")/demo-lib.sh"

require_ai

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

# The install applies the same :dev tag every time, so an already-running
# server never rolls on its own — without this, a re-run keeps serving the
# previous build (and its embedded console).
kubectl --context "${CTX}" -n donkey-system rollout restart deployment donkey-server >/dev/null 2>&1 || true

say "donkey is installed"
cat <<EOF

To reach the cluster from this machine, open a second terminal and leave this
running:

  sudo devkube --cluster donkey connect

It watches the HTTPRoutes donkey creates and keeps every donkey hostname in
your /etc/hosts, cleaning them up when you stop it. Then deploy the demo app:

  task demo:app
EOF
