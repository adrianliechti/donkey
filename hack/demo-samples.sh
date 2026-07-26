#!/usr/bin/env bash
# Deploys every sample in testdata/ into one space and reports what happened.
#
# This is the suite: the samples are deliberately awkward, so a red line here
# is information, not necessarily a bug. Two groups are treated differently:
#
#   should work   apps donkey is expected to get right with no help at all.
#                 One of these failing fails this script.
#   challenges    apps written the way people actually write them — binding
#                 loopback, missing dependencies, rejecting unknown hosts.
#                 They are reported, never fatal. What matters is whether
#                 donkey said something useful *before* the rollout timed out.
#
# Assumes donkey is installed and you are logged in (task demo:setup, then
# `devkube connect`). Expect this to take a while: it waits for each app to
# become healthy, and the challenges wait out a full rollout timeout.
set -euo pipefail
# shellcheck source=hack/demo-lib.sh
. "$(dirname "$0")/demo-lib.sh"

require_ai

SPACE="${SAMPLE_SPACE:-samples}"

# kitchen-sink-sample is one repository holding five apps, which is exactly
# what a space is for: they share a namespace and reach each other by plain
# service name. Its apps are also named the way apps in a monorepo are named —
# api, web, gateway — so keeping them out of the shared space avoids both a
# collision with anything added later and a console listing five strangers
# beside static-site.
KITCHEN_SPACE="${KITCHEN_SINK_SPACE:-kitchen-sink}"

space_for() {
  case "$1" in
    kitchen-sink-sample/*) printf '%s' "${KITCHEN_SPACE}" ;;
    *)                     printf '%s' "${SPACE}" ;;
  esac
}

# Folders donkey should handle unaided.
SHOULD_WORK=(
  "static-site"
  "fastapi-sample"
  "dockerfile-sample"
  "express-api"
  "nextjs-app"
  "My Cool App"
  "odd-port-sample"
  "sqlite-notes"
  "local-habits"
  "monorepo-root"
  # The donkey.yaml is the sample: two addons the deploy has to provision
  # (postgres and pocketbase) before the app can start against them.
  "addons-sample"
  # These three say how they start (a Procfile, or a host and port read from
  # the environment) but still declare no port, health route or donkey.yaml.
  "flask-beginner"
  "streamlit-report"
  "django-blog"
  # Three of kitchen-sink's five apps are ordinary apps that happen to share a
  # repository: railpack recognizes each one and produces a start command. It
  # is finding them separately that is the test, not building them.
  "kitchen-sink-sample/api"
  "kitchen-sink-sample/web"
  "kitchen-sink-sample/worker"
)

# Folders that exist because they are hard. Failing is an acceptable outcome;
# failing *silently* is not.
CHALLENGES=(
  # Its Dockerfile hardcodes `httpd -p 8080` and cannot read $PORT, so this one
  # only works if the port is taken from EXPOSE rather than assumed.
  "kitchen-sink-sample/gateway"
  # Tcl. No provider recognizes it, so detection fails by design and the
  # assistant has to write the build itself.
  "kitchen-sink-sample/legacy"
)

results=()
failed_strict=0
n=0

# Deploying an app writes a donkey.yaml (and sometimes a .donkey/Dockerfile)
# into its folder. Doing that here would quietly destroy the thing these
# samples exist to test: after one run they would all carry a config, and the
# next run would exercise YAML parsing instead of detection. So every sample
# is deployed from a throwaway copy and testdata/ is never written to.
#
# The copy keeps the original folder's exact basename, because that name is
# what donkey turns into the app's name — spaces, capitals and all, which is
# the whole point of "My Cool App".
WORK="$(mktemp -d)"
trap 'rm -rf "${WORK}"' EXIT

deploy_sample() {
  local dir="$1" group="$2" start elapsed status work space
  space="$(space_for "${dir}")"
  say "${dir}  →  space ${space}  (${group})"

  n=$(( n + 1 ))
  work="${WORK}/${n}/$(basename "${dir}")"
  mkdir -p "${work}"
  cp -R "${ROOT}/testdata/${dir}/." "${work}/"

  start="${SECONDS}"
  if "${DONKEY}" --yes --space "${space}" --app-dir "${work}" deploy; then
    status="ok"
  else
    status="failed"
    if [ "${group}" = "should work" ]; then
      failed_strict=1
    fi
  fi
  elapsed=$(( SECONDS - start ))
  results+=("${status}|${group}|${dir}|${elapsed}s")
}

for d in "${SHOULD_WORK[@]}"; do
  deploy_sample "${d}" "should work"
done

for d in "${CHALLENGES[@]}"; do
  deploy_sample "${d}" "challenge"
done

say "results"
printf '\n'
for r in "${results[@]}"; do
  IFS='|' read -r status group dir elapsed <<< "${r}"
  if [ "${status}" = "ok" ]; then
    printf '  %s✓%s  %-28s %-12s %s\n' "${C_OK}" "${C_OFF}" "${dir}" "${group}" "${elapsed}"
  else
    printf '  %s✗%s  %-28s %-12s %s\n' "${C_BAD}" "${C_OFF}" "${dir}" "${group}" "${elapsed}"
  fi
done

cat <<EOF

  Everything deployed:  donkey apps
  What one of them did: donkey --space ${SPACE} --app-dir testdata/<sample> logs
  Clean up:             kubectl delete namespace donkey-${SPACE} donkey-${KITCHEN_SPACE}
EOF

if [ "${failed_strict}" -ne 0 ]; then
  printf '\n%sA sample donkey should handle on its own did not deploy.%s\n' "${C_BAD}" "${C_OFF}" >&2
  exit 1
fi

printf '\nEvery sample donkey should handle on its own deployed.\n'
