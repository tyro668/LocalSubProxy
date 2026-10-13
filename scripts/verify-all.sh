#!/usr/bin/env bash
# Run the whole LMBP verification matrix and fail on any failure OR skip.
#
#   scripts/verify-all.sh                     # VERIFY_LOGS=dir keeps the per-step logs
#
# Every step runs in strict mode (CI=1, LMBP_REQUIRE_EXTERNAL=1): missing
# toolchains, gateways, official clients or schema validators are failures,
# never silent skips. Each step's log is scanned for skip markers
# (go "--- SKIP", node:test "# skipped N>0"/"# todo N>0", pytest "skipped",
# "SKIP " lines from the spec skeletons).
#
# Matrix:
#   1. Go: vet + all packages (reference gateway, Go SDK, conformance runner,
#      stores, connector) and the examples/go-nethttp module
#   2. Node SDK (sdk/node) and the Node mock (mock/node)
#   3. Python SDK (sdk/python) and the Python mock (mock/python)
#   4. spec/validate.sh: JSON Schema/OpenAPI/AsyncAPI, JOSE vectors in
#      Go/Node/Python, Python/Node/Rust protocol-example skeletons
#   5. scripts/mock-interop.sh over every gateway variant (go node python,
#      node-sqlite python-sqlite, sdk-go sdk-go-sqlite, express fastapi):
#      lmbp-conformance + Go connector e2e + official openai/anthropic clients
#   6. scripts/chat-ui-e2e.sh: headless-browser chat page on go/node/python
#
# Not in the matrix (no code to run, by design): Java is guidance plus
# illustrative code in the integration guide; the Flutter desktop shell
# (desktop/) is not part of LMBP. Passing this matrix shows the
# implementations agree with each other and with the shared vectors; it is
# not a proof of protocol compliance.
set -uo pipefail
cd "$(dirname "$0")/.."
export CI=1 LMBP_REQUIRE_EXTERNAL=1
if [ -n "${VERIFY_LOGS:-}" ]; then logs=$VERIFY_LOGS; mkdir -p "$logs"   # keep per-step logs
else logs=$(mktemp -d); trap 'rm -rf "$logs"' EXIT; fi
fail=0
results=()

need() {
  local missing=()
  for c in "$@"; do command -v "$c" >/dev/null || missing+=("$c"); done
  if [ ${#missing[@]} -gt 0 ]; then echo "missing toolchain: ${missing[*]} (required; nothing is skipped)"; exit 1; fi
}
need go node npm python3 cargo openssl curl "${CHROME:-google-chrome}"

skipped() { # log -> prints skip evidence, returns 0 if any
  grep -nE -e '--- SKIP' -e '^# (skipped|todo) [1-9]' -e '[0-9]+ skipped' -e '^SKIP ' -e '(^|[^A-Za-z])SKIP(PED)?:' -e '# SKIP' "$1" | grep -v 'test name does not match pattern' | head -5
}

step() { # name cmd...
  local name=$1; shift
  local log="$logs/$(echo "$name" | tr -c 'A-Za-z0-9' '_').log"
  printf '== %-58s ' "$name"
  local t0=$SECONDS
  if "$@" >"$log" 2>&1; then
    local s; s=$(skipped "$log")
    if [ -n "$s" ]; then
      echo "SKIPPED ($((SECONDS - t0))s)"; echo "$s" | sed 's/^/     /'; fail=1; results+=("SKIP $name")
    else
      echo "ok ($((SECONDS - t0))s)"; results+=("ok   $name")
    fi
  else
    echo "FAILED ($((SECONDS - t0))s)"; tail -40 "$log" | sed 's/^/     /'; fail=1; results+=("FAIL $name")
  fi
}

gopkgs() { go list ./... | grep -v '/mock/external$'; }  # external: run against live gateways in step 5
step "go vet" bash -c 'go vet $(go list ./... ) && (cd examples/go-nethttp && go vet ./...)'
step "go test (reference gateway, Go SDK, conformance, stores, connector)" bash -o pipefail -c "go test -count=1 -v $(gopkgs | tr '\n' ' ') | grep -E '^(--- |ok|FAIL|PASS|panic)'"
step "go test mock/external CI-mode guard" go test -count=1 -v ./mock/external -run TestExternalGatewayFailsInCIWithoutEnv
step "examples/go-nethttp build" bash -c 'cd examples/go-nethttp && go build -o /dev/null .'
step "Node SDK (sdk/node)" bash -c 'cd sdk/node && { [ -d node_modules ] || npm ci --no-audit --no-fund; } && npm test'
step "Node mock (mock/node)" bash -c 'cd mock/node && { [ -d node_modules ] || npm ci --no-audit --no-fund; } && node --test test/'
step "Python SDK (sdk/python)" bash -c 'cd sdk/python && { [ -x .venv/bin/python ] || { python3 -m venv .venv && .venv/bin/pip install -q -e ".[test]"; }; } && .venv/bin/python -m pytest -q -rs -p no:cacheprovider'
step "Python mock (mock/python)" bash -c 'cd mock/python && { [ -x .venv/bin/python ] || { python3 -m venv .venv && .venv/bin/pip install -q -r requirements.txt; }; } && .venv/bin/python -m unittest -v test_web'
step "spec/validate.sh (schemas, JOSE vectors, skeletons incl. Rust)" spec/validate.sh
step "mock-interop: conformance + connector e2e + official clients" scripts/mock-interop.sh go node python node-sqlite python-sqlite sdk-go sdk-go-sqlite express fastapi
step "chat UI e2e (headless Chrome; go node python)" scripts/chat-ui-e2e.sh

echo
printf '%s\n' "${results[@]}"
if [ $fail -ne 0 ]; then echo "verify-all: FAILED (a failure or a skip above)"; exit 1; fi
echo "verify-all: all ${#results[@]} steps passed, nothing skipped"
