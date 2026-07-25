#!/usr/bin/env bash
# Deploys the broken apps: the ones that are supposed to FAIL.
#
# Every app in testdata/broken/ fails in a way that is invisible from
# the outside — the container starts, the log looks fine, and nothing answers.
# They exist to test the half of donkey that only runs when something goes
# wrong: reading the code, explaining the cause, and proposing the patch.
#
# So the assertions here are inverted. For each app this script checks that:
#
#   1. the deploy FAILED           — an app here that deploys is no longer
#                                    broken, and is testing nothing
#   2. donkey EXPLAINED it         — a "What happened:" line
#   3. donkey PROPOSED a patch     — an actual diff, not advice to go and look
#
# All three have to hold. A failure with no explanation is the outcome this
# suite exists to catch: it is what every deploy tool does, and it leaves the
# person who wrote the app with a rollout timeout and nowhere to go.
#
# Assumes donkey is installed and you are logged in (task demo:setup, then
# `devkube connect`). This is slow by nature: the loopback cases have to wait
# out a full rollout timeout before donkey has anything to diagnose.
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
  say "${dir}  (expected to fail: ${want})"

  n=$(( n + 1 ))
  work="${WORK}/${n}/$(basename "${dir}")"
  mkdir -p "${work}"
  cp -R "${ROOT}/testdata/broken/${dir}/." "${work}/"

  log="${WORK}/${n}.log"
  # --yes so nothing waits for an answer; the patch is still printed, with a
  # note that applying it needs an interactive run.
  if "${DONKEY}" --yes --space "${SPACE}" --app-dir "${work}" deploy 2>&1 | tee "${log}"; then
    results+=("deployed|${dir}|it is not broken any more")
    failures=$(( failures + 1 ))
    return
  fi

  if ! grep -q "What happened:" "${log}"; then
    results+=("unexplained|${dir}|failed with no diagnosis")
    failures=$(( failures + 1 ))
    return
  fi
  # A unified diff always carries a hunk header; advice never does. Not
  # anchored, because when colour is on an escape sequence comes first.
  if ! grep -q -- "@@ -" "${log}"; then
    results+=("no patch|${dir}|explained, but proposed no change")
    failures=$(( failures + 1 ))
    return
  fi
  results+=("ok|${dir}|explained and patched")
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
  Apply a fix:       cd testdata/broken/<app> && donkey deploy
                     (without --yes, so the patch can be accepted)
  Clean up:          kubectl delete namespace donkey-${SPACE}
EOF

if [ "${failures}" -ne 0 ]; then
  printf '\n%s%d broken app(s) were not explained and patched.%s\n' "${C_BAD}" "${failures}" "${C_OFF}" >&2
  exit 1
fi

printf '\nEvery broken app failed, was explained, and came back with a patch.\n'
