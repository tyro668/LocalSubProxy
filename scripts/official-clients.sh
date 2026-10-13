#!/usr/bin/env bash
# Run the official openai / anthropic client libraries (Python and Node),
# unchanged, against a gateway. Called by mock/external (LSP2_OFFICIAL_CLIENTS)
# with LSP2_BASE_URL, LSP2_API_KEY, LSP2_END_USER and LSP2_MODEL set.
set -euo pipefail
cd "$(dirname "$0")/../examples/official-clients"
[ -d node_modules ] || npm ci --no-audit --no-fund -q >/dev/null 2>&1 || npm install --no-audit --no-fund -q >/dev/null
[ -x .venv/bin/python ] || { python3 -m venv .venv && .venv/bin/pip install -q -r requirements.txt; }
.venv/bin/python check.py
node check.mjs
