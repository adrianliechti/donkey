#!/usr/bin/env bash
# Installs donkey into the demo cluster: the fake identity provider, the
# cluster's certificate authority, and the control plane itself. The install
# is the very same command an admin would run — only the settings are local.
#
# Assumes the cluster exists (task setup:kind). Run `task demo:app` after this.
set -euo pipefail
# shellcheck source=hack/demo-lib.sh
. "$(dirname "$0")/demo-lib.sh"

require_ai

say "building donkey + images"
(cd "${ROOT}" && task build && task server-image:kind && task demo-idp-image:kind && task private-packages-image:kind)

say "demo identity provider (fake, auto-login)"
kubectl --context "${CTX}" apply -f "${ROOT}/hack/demo/fake-idp.yaml"
kubectl --context "${CTX}" -n donkey-identity rollout restart deployment fake-idp >/dev/null 2>&1 || true

say "the registry's certificate authority"
"${ROOT}/hack/demo-ca.sh" "${CTX}" "${ROOT}/.build/platform-ca.crt"

say "authenticated package mirror"
"${ROOT}/hack/install-private-packages-kind.sh"

say "installing the control plane (real install command, demo settings)"
"${DONKEY}" --yes --context "${CTX}" server install \
  --file "${ROOT}/hack/demo/private-packages-kind-install.yaml" \
  --image "registry.platform/donkey/donkey-server:dev" \
  --registry-url "registry.platform/donkey" \
  --registry-ca-secret "donkey-registry-ca" \
  --domain "${BASE}" \
  --gateway-service-type "ClusterIP" \
  --tls-mode "ca" \
  --default-platform "${PLATFORM}" \
  --oidc-issuer "http://auth.donkey-identity" \
  --oidc-client-id "donkey-demo" \
  --oidc-client-secret "donkey-demo-secret" \
  --openai-base-url "${AI_BASE}" \
  --openai-api-key "${AI_KEY}" \
  --exa-base-url "${EXA_BASE}" \
  --exa-api-key "${EXA_KEY}" \
  ${AI_MODEL:+--ai-model "${AI_MODEL}"}

# The install applies the same :dev tag every time. Restart every service
# using that image so a re-run consistently runs the newly built code.
donkey_deployments=(donkey-orchestrator donkey-server donkey-gateway donkey-ai donkey-proxy donkey-telemetry donkey-packages)
kubectl --context "${CTX}" -n donkey-system rollout restart deployment "${donkey_deployments[@]}" >/dev/null
for deployment in "${donkey_deployments[@]}"; do
  kubectl --context "${CTX}" -n donkey-system rollout status deployment "${deployment}" --timeout=180s >/dev/null
done

say "the gateway's certificate authority (what the CLI and browser trust)"
"${ROOT}/hack/demo-gateway-ca.sh" "${CTX}" "${CA_FILE}"

say "donkey is installed"
cat <<EOF

To reach the cluster from this machine, open a second terminal and leave this
running:

  sudo devkube --cluster donkey connect

It keeps donkey's hostnames in your /etc/hosts while it runs. (NOTE: devkube
needs a loop version that discovers donkey's hostnames from route ConfigMaps.)
Then deploy the demo app:

  task demo:app

Or skip DNS entirely and verify the installation end to end:

  task verify
EOF
