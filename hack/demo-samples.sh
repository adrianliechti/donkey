#!/usr/bin/env bash
# Deploys every sample in testdata/ and requires every one of them to come up.
#
# This is the suite. Each sample is a folder donkey has to work out on its own:
# no donkey.yaml worth the name, no Dockerfile in most of them, and a few
# written the way people actually write them rather than the way a builder
# would like. One red line fails this script.
#
# There used to be a second, non-fatal group for apps no detector recognized.
# That distinction died with the detector: every sample now takes the same
# path — donkey reads it and writes it a Dockerfile — so a Tcl app is not
# categorically harder than a Flask one, and there is nothing left to excuse.
# Compatibility repair cases live in testdata/broken/ and belong to
# demo-broken.sh, which also requires successful deployment.
#
# Assumes donkey is installed and you are logged in (task install:kind or
# task install:docker). Expect this to take a while: every app is built, run
# to prove it starts, and then waited on until it is healthy.
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

# Every folder donkey has to handle unaided. The comments say what each one is
# for — a sample nobody can explain is a sample nobody will maintain.
SAMPLES=(
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
  # The donkey.yaml is the sample: the addon the deploy has to provision
  # (postgres) before the app can start against it.
  "addons-sample"
  # These three say how they start (a Procfile, or a host and port read from
  # the environment) but still declare no port, health route or donkey.yaml.
  "flask-beginner"
  "streamlit-report"
  "django-blog"
  # Three of kitchen-sink's five apps are ordinary apps that happen to share a
  # repository. Finding them separately is the test, not building them.
  "kitchen-sink-sample/api"
  "kitchen-sink-sample/web"
  "kitchen-sink-sample/worker"
  # Its Dockerfile hardcodes `httpd -p 8080` and cannot read $PORT, so this one
  # only works if the port is taken from EXPOSE rather than assumed.
  "kitchen-sink-sample/gateway"
  # Tcl — nothing donkey knows anything about. The assistant has to work the
  # whole build out by reading the source, which is the point: an app donkey
  # has no opinion about is still an app it can deploy.
  "kitchen-sink-sample/legacy"
)

results=()
failed=0
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
  local dir="$1" start elapsed status work space
  space="$(space_for "${dir}")"
  say "${dir}  →  space ${space}"

  n=$(( n + 1 ))
  work="${WORK}/${n}/$(basename "${dir}")"
  mkdir -p "${work}"
  cp -R "${ROOT}/testdata/${dir}/." "${work}/"

  start="${SECONDS}"
  if "${DONKEY}" --yes --space "${space}" --app-dir "${work}" deploy; then
    status="ok"
  else
    status="failed"
    failed=1
  fi
  elapsed=$(( SECONDS - start ))
  results+=("${status}|${dir}|${elapsed}s")
}

for d in "${SAMPLES[@]}"; do
  deploy_sample "${d}"
done

say "results"
printf '\n'
for r in "${results[@]}"; do
  IFS='|' read -r status dir elapsed <<< "${r}"
  if [ "${status}" = "ok" ]; then
    printf '  %s✓%s  %-30s %s\n' "${C_OK}" "${C_OFF}" "${dir}" "${elapsed}"
  else
    printf '  %s✗%s  %-30s %s\n' "${C_BAD}" "${C_OFF}" "${dir}" "${elapsed}"
  fi
done

cat <<EOF

  Everything deployed:  donkey apps
  What one of them did: donkey --space ${SPACE} --app-dir testdata/<sample> logs
  Clean up (kind):      kubectl delete namespace donkey-${SPACE} donkey-${KITCHEN_SPACE}
  Clean up (docker):    docker rm -f \$(docker ps -aq --filter "name=donkey.${SPACE}." --filter "name=donkey.${KITCHEN_SPACE}.")
EOF

if [ "${failed}" -ne 0 ]; then
  printf '\n%sA sample did not deploy.%s\n' "${C_BAD}" "${C_OFF}" >&2
  exit 1
fi

printf '\nAll %d samples deployed.\n' "${#SAMPLES[@]}"
