#!/usr/bin/env bash
# DEV/DEMO ONLY — installs donkey straight onto the local Docker engine (no
# cluster, no kind): the same `donkey server install --runtime docker` an
# admin runs, with demo settings. The domain is apps.localhost — browsers
# and curl resolve *.localhost to 127.0.0.1, so no DNS and no /etc/hosts —
# and TLS comes from the gateway's own CA. The fake identity provider runs
# as one more container on the donkey network.
set -euo pipefail
# shellcheck source=hack/demo-lib.sh
. "$(dirname "$0")/demo-lib.sh"

require_ai

DOMAIN="${DONKEY_DOMAIN:-apps.localhost}"
# One issuer name has to work from both sides of the OIDC redirect: browsers
# resolve *.localhost to this machine, while Docker's network alias resolves
# the same name straight to the fake IdP container. Keeping the same port on
# both sides also keeps discovery and token exchange honest.
IDP_HOST="${DONKEY_IDP_HOST:-auth.${DOMAIN}}"
IDP_PORT="${DONKEY_IDP_PORT:-5556}"
IDP_ISSUER="${DONKEY_IDP_ISSUER:-http://${IDP_HOST}:${IDP_PORT}}"

say "building donkey + images"
(cd "${ROOT}" && task build && task server-image && task machine-image && task demo-idp-image && task private-packages-image)

say "authenticated package mirror"
"${ROOT}/hack/install-private-packages-docker.sh"

say "installing the control plane (real install command, demo settings)"
"${DONKEY}" --yes server install \
  --file "${ROOT}/.build/private-packages-docker-install.yaml" \
  --runtime docker \
  --image "donkey/donkey-server:dev" \
  --machine-image "donkey/donkey-machine:dev" \
  --domain "${DOMAIN}" \
  --tls-mode "ca" \
  --default-platform "${PLATFORM}" \
  --oidc-issuer "${IDP_ISSUER}" \
  --oidc-client-id "donkey-demo" \
  --oidc-client-secret "donkey-demo-secret" \
  --openai-base-url "${AI_BASE}" \
  --openai-api-key "${AI_KEY}" \
  --exa-base-url "${EXA_BASE}" \
  --exa-api-key "${EXA_KEY}" \
  ${AI_MODEL:+--ai-model "${AI_MODEL}"}

say "demo identity provider (fake, auto-login)"
docker rm -f donkey-identity >/dev/null 2>&1 || true
docker run -d --name donkey-identity --restart=unless-stopped \
  --network donkey --network-alias "${IDP_HOST}" \
  -p "127.0.0.1:${IDP_PORT}:${IDP_PORT}" -p "[::1]:${IDP_PORT}:${IDP_PORT}" \
  -e ISSUER="${IDP_ISSUER}" -e LISTEN=":${IDP_PORT}" \
  donkey/fake-idp:dev >/dev/null
echo "fake-idp ready at ${IDP_ISSUER} (host browser + donkey network)"

say "the gateway's certificate authority (what the CLI and curl trust)"
CA_DOCKER="${ROOT}/.build/donkey-gateway-ca-docker.crt"
mkdir -p "${ROOT}/.build"
ok=""
for _ in $(seq 1 30); do
  if docker run --rm -v donkey-data:/state busybox:1.37 cat /state/ca/ca.crt > "${CA_DOCKER}" 2>/dev/null && [ -s "${CA_DOCKER}" ]; then
    ok=yes; break
  fi
  sleep 2
done
[ -n "${ok}" ] || { echo "FAIL: the gateway never minted its CA" >&2; exit 1; }
echo "certificate authority ready — ${CA_DOCKER}"

say "donkey is installed (runtime: docker)"
cat <<EOF

Everything answers on this machine directly — no DNS setup needed:

  API + console:  https://${DOMAIN}
  Apps:           https://<app>.<space>.${DOMAIN}

Sign the CLI in with a minted token (the fake IdP has no browser here):

  TOKEN=\$(DONKEY_SESSION_KEY=\$(docker inspect donkey-server --format '{{range .Config.Env}}{{println .}}{{end}}' | sed -n 's/^DONKEY_SESSION_KEY=//p') go run ./hack/mint-token)
  ${DONKEY} login --server https://${DOMAIN} --token "\${TOKEN}" --ca-file ${CA_DOCKER}

Then:  ${DONKEY} doctor · cd examples/demo/backend && ${DONKEY} deploy
Verify end to end:  task verify:docker
Tear down:  docker rm -f donkey-server donkey-orchestrator donkey-gateway donkey-buildkitd donkey-registry donkey-identity && docker volume rm donkey-data donkey-registry donkey-buildkit
EOF
