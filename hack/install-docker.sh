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

say "building donkey + images"
(cd "${ROOT}" && task build && task server-image && task demo-idp-image)

say "installing the control plane (real install command, demo settings)"
"${DONKEY}" --yes server install \
  --runtime docker \
  --image "donkey/donkey-server:dev" \
  --domain "${DOMAIN}" \
  --tls-mode "ca" \
  --default-platform "${PLATFORM}" \
  --oidc-issuer "http://auth.donkey-identity" \
  --oidc-client-id "donkey-demo" \
  --oidc-client-secret "donkey-demo-secret" \
  --openai-base-url "${AI_BASE}" \
  --openai-api-key "${AI_KEY}"

say "demo identity provider (fake, auto-login)"
docker rm -f donkey-identity >/dev/null 2>&1 || true
# The issuer URL has no port, so the container listens on :80 itself — no
# Service in front to remap it here (Docker lets unprivileged users bind 80).
docker run -d --name donkey-identity --restart=unless-stopped \
  --network donkey --network-alias auth.donkey-identity \
  -e ISSUER=http://auth.donkey-identity -e LISTEN=:80 \
  donkey/fake-idp:dev >/dev/null
echo "fake-idp running as auth.donkey-identity on the donkey network"

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
Tear down:  docker rm -f donkey-server donkey-gateway donkey-buildkitd donkey-registry donkey-identity && docker volume rm donkey-data donkey-registry donkey-buildkit
EOF
