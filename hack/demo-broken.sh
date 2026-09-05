#!/usr/bin/env bash
# Deploy compatibility repair cases with the live assistant. Every app must
# come up; a diagnosis alone is not a successful result. Source stays intact
# because deployments use temporary copies.
set -euo pipefail
# shellcheck source=hack/demo-lib.sh
. "$(dirname "$0")/demo-lib.sh"

require_ai

SPACE="${BROKEN_SPACE:-broken}"

results=()
failures=0
n=0

# Deployed from throwaway copies, like the samples: a deploy writes a
# donkey.yaml back into the folder, and one carrying a config from a previous
# run no longer starts from nothing.
WORK="$(mktemp -d)"
trap 'rm -rf "${WORK}"' EXIT

check() {
  local dir="$1" want="$2" log work
  say "${dir}  (repair: ${want})"

  n=$(( n + 1 ))
  work="${WORK}/${n}/$(basename "${dir}")"
  mkdir -p "${work}"
  cp -R "${ROOT}/testdata/broken/${dir}/." "${work}/"

  log="${WORK}/${n}.log"
  if "${DONKEY}" --yes --space "${SPACE}" --app-dir "${work}" deploy 2>&1 | tee "${log}"; then
    results+=("ok|${dir}|deployed successfully")
  else
    results+=("failed|${dir}|automatic repair did not produce a healthy app")
    failures=$(( failures + 1 ))
  fi
}

check "flask-loopback"     "binds 127.0.0.1, so nothing reaches it"
check "express-loopback"   "binds 127.0.0.1, so nothing reaches it"
check "streamlit-bare"     "started with python, so no server is ever created"
check "no-manifest"        "imports flask and declares it nowhere"

say "results (space: ${SPACE})"
printf '\n'
for r in "${results[@]}"; do
  IFS='|' read -r status dir note <<< "${r}"
  if [ "${status}" = "ok" ]; then
    printf '  %s✓%s  %-22s %s\n' "${C_OK}" "${C_OFF}" "${dir}" "${note}"
  else
    printf '  %s✗%s  %-22s %s (%s)\n' "${C_BAD}" "${C_OFF}" "${dir}" "${note}" "${status}"
  fi
done

cat <<EOF

  Read one of them:  donkey --space ${SPACE} --app-dir testdata/broken/<app> logs
  Check HTTP too:    task verify:builder
  Clean up:          kubectl delete namespace donkey-${SPACE}
EOF

if [ "${failures}" -ne 0 ]; then
  printf '\n%s%d repair case(s) did not deploy.%s\n' "${C_BAD}" "${failures}" "${C_OFF}" >&2
  exit 1
fi

printf '\nEvery repair case deployed successfully.\n'
