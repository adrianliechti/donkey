#!/usr/bin/env bash
# DEMO ONLY — makes donkey's Gateway reachable on the kind host ports.
# Attaches an EnvoyProxy config to the Gateway so Envoy Gateway itself
# renders the service as NodePort 30080 (kind maps host port 80 there).
# Patching the generated service directly doesn't stick — the controller
# owns and reverts it. Real clusters use the LoadBalancer default instead.
set -euo pipefail

CONTEXT="${1:-kind-donkey}"

kubectl --context "${CONTEXT}" apply --server-side --force-conflicts -f - <<'EOF'
apiVersion: gateway.envoyproxy.io/v1alpha1
kind: EnvoyProxy
metadata:
  name: donkey-demo-proxy
  namespace: donkey-system
spec:
  provider:
    type: Kubernetes
    kubernetes:
      envoyService:
        type: NodePort
        patch:
          type: StrategicMerge
          value:
            spec:
              ports:
                - port: 80
                  nodePort: 30080
                - port: 443
                  nodePort: 30443
EOF

# Attach it to the Gateway with a separate field manager, so donkey's own
# server-side applies (which don't set infrastructure) leave it alone.
kubectl --context "${CONTEXT}" -n donkey-system patch gateway donkey --type merge -p \
  '{"spec":{"infrastructure":{"parametersRef":{"group":"gateway.envoyproxy.io","kind":"EnvoyProxy","name":"donkey-demo-proxy"}}}}'

SELECTOR="gateway.envoyproxy.io/owning-gateway-name=donkey,gateway.envoyproxy.io/owning-gateway-namespace=donkey-system"
for _ in $(seq 1 60); do
  TYPE="$(kubectl --context "${CONTEXT}" -n envoy-gateway-system get svc -l "${SELECTOR}" -o jsonpath='{.items[0].spec.type}' 2>/dev/null || true)"
  NODEPORT="$(kubectl --context "${CONTEXT}" -n envoy-gateway-system get svc -l "${SELECTOR}" -o jsonpath='{.items[0].spec.ports[0].nodePort}' 2>/dev/null || true)"
  if [ "${TYPE}" = "NodePort" ] && [ "${NODEPORT}" = "30080" ]; then
    echo "Envoy service pinned to NodePort 30080 (host port 80)"
    exit 0
  fi
  sleep 2
done
echo "Envoy service never settled on NodePort 30080" >&2
exit 1
