#!/usr/bin/env bash
# Creates a local kind cluster wired for donkey development:
#   - a plain-HTTP registry container, reachable as host.docker.internal:5001
#     from the host and mirrored into the cluster's containerd
#   - Envoy Gateway (the Gateway API implementation donkey targets)
#   - host ports 80/443 mapped to NodePorts 30080/30443, where
#     hack/demo-expose.sh later pins the Envoy service
set -euo pipefail

CLUSTER="${CLUSTER:-donkey}"
REG_NAME="${REG_NAME:-donkey-registry}"
REG_PORT="${REG_PORT:-5001}"
REG_HOST="host.docker.internal:${REG_PORT}"

if [ "$(docker inspect -f '{{.State.Running}}' "${REG_NAME}" 2>/dev/null || true)" != 'true' ]; then
  docker rm -f "${REG_NAME}" 2>/dev/null || true
  docker run -d --restart=always -p "127.0.0.1:${REG_PORT}:5000" --name "${REG_NAME}" registry:2
fi

if ! kind get clusters 2>/dev/null | grep -qx "${CLUSTER}"; then
cat <<EOF | kind create cluster --name "${CLUSTER}" --config=-
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
containerdConfigPatches:
- |-
  [plugins."io.containerd.grpc.v1.cri".registry]
    config_path = "/etc/containerd/certs.d"
nodes:
- role: control-plane
  extraPortMappings:
  - containerPort: 30080
    hostPort: 80
    protocol: TCP
  - containerPort: 30443
    hostPort: 443
    protocol: TCP
EOF
fi

# Make sure the current $KUBECONFIG knows the cluster (kind state lives in
# docker, so a fresh kubeconfig needs an explicit export).
kind export kubeconfig --name "${CLUSTER}"

if [ "$(docker inspect -f='{{json .NetworkSettings.Networks.kind}}' "${REG_NAME}")" = 'null' ]; then
  docker network connect "kind" "${REG_NAME}"
fi
REG_IP="$(docker inspect -f '{{(index .NetworkSettings.Networks "kind").IPAddress}}' "${REG_NAME}")"

# The same registry is known under three names: from the host/buildkitd
# (host.docker.internal:5001), by container name, and by kind-network IP
# (what donkey-server and the in-cluster buildkit use). Mirror all three.
for host in "${REG_HOST}" "${REG_NAME}:5000" "${REG_IP}:5000"; do
  REGISTRY_DIR="/etc/containerd/certs.d/${host}"
  for node in $(kind get nodes --name "${CLUSTER}"); do
    docker exec "${node}" mkdir -p "${REGISTRY_DIR}"
    cat <<EOF | docker exec -i "${node}" cp /dev/stdin "${REGISTRY_DIR}/hosts.toml"
[host."http://${REG_NAME}:5000"]
EOF
  done
done

# Envoy Gateway (https://gateway.envoyproxy.io/docs/tasks/quickstart/)
# Pinned to a stable release — the quickstart's "latest" is a rolling dev
# build that expects experimental Gateway API CRDs.
ENVOY_GATEWAY_VERSION="${ENVOY_GATEWAY_VERSION:-v1.8.3}"
kubectl --context "kind-${CLUSTER}" apply --server-side -f \
  "https://github.com/envoyproxy/gateway/releases/download/${ENVOY_GATEWAY_VERSION}/install.yaml"
kubectl --context "kind-${CLUSTER}" wait --timeout=5m -n envoy-gateway-system \
  deployment/envoy-gateway --for=condition=Available

echo "kind cluster '${CLUSTER}' ready — context kind-${CLUSTER}, registry ${REG_HOST}, Envoy Gateway installed"
