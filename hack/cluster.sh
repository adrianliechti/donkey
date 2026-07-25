#!/usr/bin/env bash
# DEV/DEMO ONLY — brings up the local cluster donkey is tried out on.
#
# Everything here comes from devkube (https://github.com/adrianliechti/devkube):
# the Kubernetes cluster, cert-manager with its `platform` CA ClusterIssuer,
# an in-cluster registry whose certificate the nodes already trust, and Envoy
# Gateway. Those are exactly donkey's prerequisites, so the demo installs
# nothing by hand and patches nothing in the cluster — no CoreDNS edits, no
# files on nodes, no NodePort pinning.
set -euo pipefail

if ! command -v devkube >/dev/null; then
  echo "devkube is required: brew install adrianliechti/tap/devkube" >&2
  exit 1
fi

CLUSTER="${CLUSTER:-donkey}"

# Idempotent: devkube reconciles what is already there.
devkube --cluster "${CLUSTER}" create
devkube --cluster "${CLUSTER}" install envoy

kubectl --context "kind-${CLUSTER}" -n cert-manager wait --for=condition=Ready \
  certificate/platform-ca --timeout=180s
echo "cluster ready — context kind-${CLUSTER} (cert-manager, registry.platform, Envoy Gateway)"
