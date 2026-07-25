#!/usr/bin/env bash
# DEMO ONLY — split-horizon DNS fix for nip.io dev domains.
# *.127.0.0.1.nip.io resolves to 127.0.0.1, which inside a pod is the pod
# itself. Patch CoreDNS to answer those names with the Envoy gateway
# service's ClusterIP so in-cluster callers (Envoy's OIDC discovery of the
# identity provider) reach the same endpoints browsers do. Real DNS setups
# never need this.
set -euo pipefail

CONTEXT="${1:-kind-donkey}"
DOMAIN="${2:-127.0.0.1.nip.io}"
SELECTOR="gateway.envoyproxy.io/owning-gateway-name=donkey,gateway.envoyproxy.io/owning-gateway-namespace=donkey-system"

if kubectl --context "${CONTEXT}" -n kube-system get cm coredns -o jsonpath='{.data.Corefile}' | grep -q "${DOMAIN}"; then
  echo "cluster DNS already patched for *.${DOMAIN}"
  exit 0
fi

GATEWAY_IP=""
for _ in $(seq 1 60); do
  GATEWAY_IP="$(kubectl --context "${CONTEXT}" -n envoy-gateway-system get svc -l "${SELECTOR}" -o jsonpath='{.items[0].spec.clusterIP}' 2>/dev/null || true)"
  [ -n "${GATEWAY_IP}" ] && break
  sleep 2
done
if [ -z "${GATEWAY_IP}" ]; then
  echo "Envoy service for the donkey Gateway never appeared" >&2
  exit 1
fi

kubectl --context "${CONTEXT}" -n kube-system get cm coredns -o jsonpath='{.data.Corefile}' > /tmp/donkey-corefile
python3 - "$GATEWAY_IP" "$DOMAIN" <<'PYEOF'
import sys
ip, domain = sys.argv[1], sys.argv[2]
path = '/tmp/donkey-corefile'
src = open(path).read()
block = (f'    template IN A {domain} {{\n'
         f'        answer "{{{{ .Name }}}} 60 IN A {ip}"\n'
         f'        fallthrough\n'
         f'    }}\n')
patched = src.replace('.:53 {\n', '.:53 {\n' + block, 1)
if patched == src:
    raise SystemExit('unexpected Corefile layout')
open(path, 'w').write(patched)
PYEOF
kubectl --context "${CONTEXT}" -n kube-system create configmap coredns \
  --from-file=Corefile=/tmp/donkey-corefile --dry-run=client -o yaml | \
  kubectl --context "${CONTEXT}" -n kube-system apply -f -
kubectl --context "${CONTEXT}" -n kube-system rollout restart deployment coredns
kubectl --context "${CONTEXT}" -n kube-system rollout status deployment coredns --timeout=120s
echo "cluster DNS patched: *.${DOMAIN} → ${GATEWAY_IP} (envoy gateway)"
