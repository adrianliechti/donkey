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
  docker compose --project-name donkey-policy-demo --env-file "$bundle/.env" \
    -f "$bundle/compose.yaml" -f "$demo/compose.demo.yaml" "$@"
)

verify() {
  go build -o "$work/mint-token" ./hack/mint-token
  (
    set -a
    source "$bundle/.env"
    set +a
    DONKEY_DEMO_TOKEN="$(MINT_TOKEN_EMAIL=demo@example.com "$work/mint-token")" python3 "$demo/verify.py"
  )
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
    curl --fail --silent --show-error --retry 40 --retry-connrefused --retry-delay 2 http://127.0.0.1:8088/readyz >/dev/null
    verify
    ;;
  verify) verify ;;
  status) compose ps ;;
  stop) compose stop ;;
  *) echo "Usage: $0 [up|verify|status|stop]" >&2; exit 2 ;;
esac
