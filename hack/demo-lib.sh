# shellcheck shell=bash
# Settings and helpers shared by the demo phases (cluster, setup, app).
# Sourced, never executed.

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DONKEY="${ROOT}/bin/donkey"
CLUSTER="donkey"
CTX="kind-${CLUSTER}"
BASE="apps.test"
CA_FILE="${ROOT}/.build/donkey-gateway-ca.crt"

# mint_token seals a CLI token from the installation's session key — the
# scripted stand-in for the browser round trip `donkey login` runs.
mint_token() {
  DONKEY_SESSION_KEY="$(kubectl --context "${CTX}" -n donkey-system get secret donkey-system-config \
    -o jsonpath='{.data.session-key}' | base64 -d)" \
    go run "${ROOT}/hack/mint-token"
}

case "$(uname -m)" in
  arm64|aarch64) PLATFORM="linux/arm64" ;;
  *)             PLATFORM="linux/amd64" ;;
esac

# donkey reads every app with its assistant, and the demos need a real one:
# donkey's own builder has the model WRITE each app's Dockerfile, so there is
# nothing a stub could stand in for. Point OPENAI_BASE_URL / OPENAI_API_KEY at
# a model, or run an OpenAI-compatible proxy on host port 8080.
AI_BASE="${OPENAI_BASE_URL:-http://host.docker.internal:8080/v1}"
AI_KEY="${OPENAI_API_KEY:-local}"
# Empty means "whatever the install defaults to". DONKEY_MODEL is the same
# variable the live assistant tests read, so one export covers both.
AI_MODEL="${DONKEY_MODEL:-}"

# Colour, but only when someone is actually looking at a terminal. Piped into
# a file or a CI log, escape codes are noise that also breaks grep — and these
# scripts are grepped, by demo-broken.sh checking its own output.
if [ -t 1 ] && [ -z "${NO_COLOR:-}" ]; then
  C_BOLD=$'\033[1m'; C_OK=$'\033[32m'; C_BAD=$'\033[31m'; C_DIM=$'\033[2m'; C_OFF=$'\033[0m'
else
  C_BOLD=''; C_OK=''; C_BAD=''; C_DIM=''; C_OFF=''
fi

say() { printf '\n%s== %s ==%s\n' "${C_BOLD}" "$*" "${C_OFF}"; }

# Without a model nothing would deploy, so the phases that deploy stop early
# rather than failing halfway through.
require_ai() {
  if [ -z "${OPENAI_BASE_URL:-}" ] && ! nc -z -w 2 127.0.0.1 8080 2>/dev/null; then
    cat >&2 <<'MSG'
No OpenAI-compatible endpoint on host port 8080.

donkey reads every app with its assistant (ports, health routes, data folders),
and with build.builder: ai the assistant writes the app's Dockerfile — so the
demo cannot deploy without a real model. Two ways out:

  OPENAI_BASE_URL=https://api.openai.com/v1 OPENAI_API_KEY=sk-… task demo
  … or run your own OpenAI-compatible proxy on port 8080
MSG
    exit 1
  fi
}
