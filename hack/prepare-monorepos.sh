#!/usr/bin/env bash
# Pinned upstream repositories for real project deployment tests.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
CORPUS="${ROOT}/.build/corpus"

checkout() {
  local repo="$1" name="$2" revision="$3" dir="${CORPUS}/$2"
  if [ ! -d "${dir}" ]; then
    mkdir -p "${dir}"
    git -C "${dir}" init --quiet
    git -C "${dir}" fetch --quiet --depth 1 "https://github.com/${repo}.git" "${revision}"
    git -C "${dir}" checkout --quiet --detach FETCH_HEAD
  fi
  if [ "$(git -C "${dir}" rev-parse HEAD)" != "${revision}" ]; then
    echo "${name} is not at the pinned revision ${revision}; keep your checkout and use a separate test corpus." >&2
    exit 1
  fi
}

checkout GoogleCloudPlatform/microservices-demo microservices-demo b9a978db9e01f4ad3dca9494a22cb9edc17548fe

# Exercise Donkey's AI builder from source. Keep deployment manifests as
# evidence for ports, settings and connections, but remove upstream build
# recipes and context exclusions from this disposable test checkout.
find "${CORPUS}/microservices-demo" -type d -name .git -prune -o \
  -type f \( -iname '*dockerfile*' -o -iname '*containerfile*' -o -iname '*dockerignore' \) -exec rm -f {} +

printf 'Prepared Google microservices-demo in %s\n' "${CORPUS}"
