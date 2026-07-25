# Settings and helpers shared by the demo phases (cluster, setup, app).
# Sourced, never executed.

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DONKEY="${ROOT}/bin/donkey"
CLUSTER="donkey"
CTX="kind-${CLUSTER}"
BASE="apps.test"
CA_FILE="${ROOT}/.build/platform-ca.crt"
TOKEN="${DONKEY_TOKEN:-dk_demo_local}"

case "$(uname -m)" in
  arm64|aarch64) PLATFORM="linux/arm64" ;;
  *)             PLATFORM="linux/amd64" ;;
esac

# donkey reads every app with its assistant, so the demo needs a model: an
# OpenAI-compatible proxy on host port 8080 (override with OPENAI_BASE_URL /
# OPENAI_API_KEY).
AI_BASE="${OPENAI_BASE_URL:-http://host.docker.internal:8080/v1}"
AI_KEY="${OPENAI_API_KEY:-local}"

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
so the demo cannot deploy without one. Start your proxy on port 8080, or point
the demo elsewhere:

  OPENAI_BASE_URL=https://api.openai.com/v1 OPENAI_API_KEY=sk-… task demo
MSG
    exit 1
  fi
}
