#!/usr/bin/env bash
# Headless-browser E2E of the shared chat page (mock/web) on the Go, Node and
# Python gateways, paired with the real Go connector (Ollama adapter → the fake
# model's /api/chat). Screenshots go to $SHOTS (default /tmp/lsp2-chat-shots).
#
#   scripts/chat-ui-e2e.sh             # go node python
#   scripts/chat-ui-e2e.sh python      # just one
# Needs: go, node >= 18.17, python3, a Chrome/Chromium ($CHROME, default /usr/bin/google-chrome).
set -euo pipefail
cd "$(dirname "$0")/.."
bin=$(mktemp -d); trap 'rm -rf "$bin"' EXIT
go build -o "$bin/localsubproxy" ./cmd/localsubproxy
go build -o "$bin/lspmock" ./mock/cmd/lspmock
(cd mock/node && [ -d node_modules ] || npm ci --no-audit --no-fund >/dev/null)
[ -x mock/python/.venv/bin/python ] || { python3 -m venv mock/python/.venv && mock/python/.venv/bin/pip install -q -r mock/python/requirements.txt; }
(cd mock/web/e2e && [ -d node_modules ] || npm install --no-audit --no-fund >/dev/null)
LSP2_BIN="$bin" node mock/web/e2e/chat-ui.mjs "$@"
