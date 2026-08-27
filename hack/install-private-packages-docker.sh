#!/usr/bin/env bash
# E2E ONLY — starts the authenticated TLS package mirror on donkey's Docker
# network and writes the install fragment that mounts its Docker config.
set -euo pipefail
# shellcheck source=hack/demo-lib.sh
. "$(dirname "$0")/demo-lib.sh"

CERT_DIR="${ROOT}/.build/private-packages-docker"
AUTH_DIR="${ROOT}/.build/private-package-auth-docker"
INSTALL_FILE="${ROOT}/.build/private-packages-docker-install.yaml"
HOST="donkey-private-packages:8443"
DOCKER_CONFIG_HOST="oci.mirror.e2e.test"

(cd "${ROOT}" && go run ./hack/testcert -out "${CERT_DIR}" -hosts donkey-private-packages)
mkdir -p "${AUTH_DIR}"
AUTH="$(printf '%s' 'e2e-reader:e2e-token' | base64 | tr -d '\n')"
printf '{"auths":{"%s":{"auth":"%s"}}}\n' "${DOCKER_CONFIG_HOST}" "${AUTH}" > "${AUTH_DIR}/config.json"
chmod 600 "${AUTH_DIR}/config.json"

cat > "${INSTALL_FILE}" <<EOF
registry:
  caFile: ${CERT_DIR}/ca.crt
build:
  packages:
    npm: https://${HOST}/npm
    pypi: https://${HOST}/pypi
    go: https://${HOST}/go
    maven: https://${HOST}/maven
    nuget: https://${HOST}/nuget/index.json
    resourceHosts:
      - registry.npmjs.org
      - pypi.org
      - files.pythonhosted.org
      - storage.googleapis.com
      - api.nuget.org
    credentialHosts:
      - ${HOST}
    dockerConfigDir: ${AUTH_DIR}
    dockerConfigHost: ${DOCKER_CONFIG_HOST}
EOF

docker network inspect donkey >/dev/null 2>&1 || docker network create donkey >/dev/null
docker rm -f donkey-private-packages >/dev/null 2>&1 || true
docker run -d --name donkey-private-packages --restart=unless-stopped \
  --network donkey \
  --user "$(id -u):$(id -g)" \
  -e TLS_CERT=/tls/tls.crt -e TLS_KEY=/tls/tls.key \
  -e USERNAME=e2e-reader -e PASSWORD=e2e-token \
  -v "${CERT_DIR}:/tls:ro" \
  donkey/private-packages:dev >/dev/null

echo "authenticated package mirror ready — ${HOST}"
echo "install settings — ${INSTALL_FILE}"
