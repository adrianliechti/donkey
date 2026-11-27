#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/../.."
demo=examples/service-policy
work=.build/service-policy-demo
bundle="$work/bundle"

compose() (
  set -a
  source "$bundle/.env"
  set +a
  files=(-f "$bundle/compose.yaml" -f "$demo/compose.demo.yaml")
  if [[ -f "$work/gateway-policy.json" && "${DEMO_BASE_ONLY:-}" != 1 ]]; then
    files+=(-f "$work/gateway-policy.json")
  fi
  docker compose --project-name donkey-policy-demo --env-file "$bundle/.env" \
    "${files[@]}" "$@"
)

with_token() {
  go build -o "$work/mint-token" ./hack/mint-token
  (
    set -a
    source "$bundle/.env"
    set +a
    DONKEY_DEMO_TOKEN="$(MINT_TOKEN_EMAIL=demo@example.com "$work/mint-token")" python3 -B "$@"
  )
}

ready() {
  curl --fail --silent --show-error --retry 40 --retry-connrefused --retry-delay 2 http://127.0.0.1:8088/readyz >/dev/null
}

observations() {
  docker run --rm --user 0 --volume donkey-policy-demo-data:/data \
    donkey/policy-upstream:local python3 /observations.py "$1"
}

sample_apps() {
  local ids id
  ids="$(docker ps -aq --filter "status=$2" --filter label=local.donkey.installation=donkey \
    --filter label=local.donkey.role=workload \
    --filter 'name=^/donkey\.(gateway-demo\.(healthy|critical|stale)|gateway-drift\.web)$')"
  if [[ -n "$ids" ]]; then
    while read -r id; do docker "$1" "$id" >/dev/null; done <<< "$ids"
  fi
}

case "${1:-up}" in
  up)
    mkdir -p "$work"
    tar --exclude=node_modules --exclude=dist -cf - cmd internal pkg web examples/skills go.mod go.sum hack/Dockerfile.server |
      docker build --build-arg VERSION=service-policy-demo -t donkey/service-policy-demo:local -f hack/Dockerfile.server -
    docker build -t donkey/policy-upstream:local -f "$demo/Dockerfile" .
    go build -o "$work/donkey" ./cmd/donkey
    "$work/donkey" server render -f "$demo/server.yaml" --output "$bundle" --force
    if [[ ! -f "$bundle/.env" ]]; then
      (umask 077
       { printf 'DONKEY_SESSION_KEY=%s\nDONKEY_TELEMETRY_KEY=%s\n' "$(openssl rand -hex 32)" "$(openssl rand -hex 32)"
       printf 'DONKEY_OIDC_CLIENT_SECRET=demo\nOPENAI_API_KEY=demo\n'; } > "$bundle/.env")
    fi
    compose up -d
    sample_apps start exited
    ready
    with_token "$demo/verify.py"
    ;;
  gateway)
    with_token "$demo/gateway.py" seed
    observations scan
    DEMO_BASE_ONLY=1 compose config --format json | python3 -B "$demo/gateway.py" policy
    with_token "$demo/gateway.py" enable
    compose up -d --wait orchestrator server
    ready
    observations age
    docker cp donkey-gateway:/var/lib/donkey/ca/ca.crt "$work/ca.crt"
    with_token "$demo/gateway.py" check
    ;;
  gateway-off)
    with_token "$demo/gateway.py" disable
    with_token "$demo/gateway.py" check-off
    ;;
  gateway-repair)
    with_token "$demo/gateway.py" repair
    observations clean
    with_token "$demo/gateway.py" check-repaired
    ;;
  gateway-check) with_token "$demo/gateway.py" check ;;
  verify) with_token "$demo/verify.py" ;;
  status) compose ps ;;
  stop) compose stop; sample_apps stop running ;;
  *) echo "Usage: $0 [up|verify|gateway|gateway-check|gateway-repair|gateway-off|status|stop]" >&2; exit 2 ;;
esac
