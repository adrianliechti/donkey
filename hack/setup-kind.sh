#!/usr/bin/env bash
# DEV/DEMO ONLY — brings up the local cluster donkey is tried out on.
#
# Everything here comes from devkube (https://github.com/adrianliechti/devkube):
# the Kubernetes cluster, and an in-cluster registry whose certificate the
# nodes already trust (signed by devkube's cert-manager `platform` CA). That
# is all donkey needs — TLS and routing are the donkey gateway's own — so the
# demo installs nothing by hand and patches nothing in the cluster.
set -euo pipefail

if ! command -v devkube >/dev/null; then
  echo "devkube is required: brew install adrianliechti/tap/devkube" >&2
  exit 1
fi

CLUSTER="${CLUSTER:-donkey}"

# Idempotent: devkube reconciles what is already there.
devkube --cluster "${CLUSTER}" create

kubectl --context "kind-${CLUSTER}" -n cert-manager wait --for=condition=Ready \
  certificate/platform-ca --timeout=180s
echo "cluster ready — context kind-${CLUSTER} (registry.platform)"
