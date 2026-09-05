#!/usr/bin/env bash
# Browser E2E against task install:docker's local fake-IdP installation.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "${ROOT}"
npm install --prefix .build/browser --no-audit --no-fund --no-save playwright@1.62.1
.build/browser/node_modules/.bin/playwright install chromium
node "${1:-e2e/browser.mjs}"
