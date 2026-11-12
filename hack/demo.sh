#!/usr/bin/env bash
# The whole demo, end to end. THIS is the only place a cluster gets created
# and the only place demo pieces (the fake identity provider) come in — real
# installations run `donkey server install` against an existing cluster with a
# real identity provider.
#
# The phases are also tasks of their own, if you want to redo just one:
#   task setup:kind     the local cluster and donkey's prerequisites
#   task install:kind   the identity provider, the CA, and donkey itself
#   task demo:app       login and deploy the demo app
set -euo pipefail
. "$(dirname "$0")/demo-lib.sh"

require_ai

say "cluster + prerequisites (devkube: registry)"
"${ROOT}/hack/setup-kind.sh"

"${ROOT}/hack/install-kind.sh"

printf '\nLeave `sudo devkube --cluster donkey connect` running in a second terminal, then press enter here.\n'
read -r _

"${ROOT}/hack/demo-app.sh"
