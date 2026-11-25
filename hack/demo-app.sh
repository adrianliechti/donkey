#!/usr/bin/env bash
# Deploy the PostgreSQL guestbook to the local kind installation.
set -euo pipefail
# shellcheck source=hack/demo-lib.sh
. "$(dirname "$0")/demo-lib.sh"

require_ai

say "connecting this machine"
# A human runs `donkey login` and signs in through the browser; the script
# mints its token straight from the cluster's session key instead.
TOKEN="$(mint_token)"
"${DONKEY}" login --server "https://${BASE}" --token "${TOKEN}" --ca-file "${CA_FILE}"

say "deploying the PostgreSQL guestbook in the demo space"
(cd "${ROOT}/testdata/addons-sample" && "${DONKEY}" --space demo --yes deploy)

cat <<EOF
  Guestbook: https://addons-sample.demo.${BASE}
  Admin console: https://${BASE}
EOF
