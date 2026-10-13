#!/usr/bin/env bash
# Run lsp2-conformance and the Go-connector end-to-end test against the
# Node and Python mock gateways (and the Go reference gateway).
#
#   scripts/mock-interop.sh            # node python go
#   scripts/mock-interop.sh node       # just one
set -euo pipefail
cd "$(dirname "$0")/.."
impls=("${@:-node python go}")
read -r -a impls <<<"${impls[*]}"
tmp=$(mktemp -d)
pids=()
cleanup() { for p in "${pids[@]}"; do kill "$p" 2>/dev/null || true; done; rm -rf "$tmp"; }
trap cleanup EXIT

export LSP_TENANT_KEY_ACME
LSP_TENANT_KEY_ACME=$(openssl rand -hex 24)   # never printed
go build -o "$tmp/lsp2-conformance" ./cmd/lsp2-conformance

start() { # impl port
  local impl=$1 port=$2
  case $impl in
    node)
      (cd mock/node && [ -d node_modules ] || npm ci --no-audit --no-fund >/dev/null)
      node mock/node/gateway.mjs --listen "127.0.0.1:$port" --tenant-profiles mock/tenant-profiles.example.json >"$tmp/$impl.log" 2>&1 & ;;
    python)
      [ -x mock/python/.venv/bin/python ] || { python3 -m venv mock/python/.venv && mock/python/.venv/bin/pip install -q -r mock/python/requirements.txt; }
      (cd mock/python && exec .venv/bin/python gateway.py --listen "127.0.0.1:$port" --tenant-profiles ../tenant-profiles.example.json) >"$tmp/$impl.log" 2>&1 & ;;
    go)
      go build -o "$tmp/localsubproxy" ./cmd/localsubproxy
      "$tmp/localsubproxy" server -listen "127.0.0.1:$port" -db "$tmp/go.db" -tenants acme:LSP_TENANT_KEY_ACME \
        -tenant-profiles mock/tenant-profiles.example.json -issuer "http://127.0.0.1:$port" -chat-ui >"$tmp/$impl.log" 2>&1 & ;;
    *) echo "unknown implementation $impl" >&2; exit 2 ;;
  esac
  pids+=($!)
  for _ in $(seq 50); do curl -fsS "http://127.0.0.1:$port/healthz" >/dev/null 2>&1 && return; sleep 0.2; done
  echo "$impl did not start:"; cat "$tmp/$impl.log"; exit 1
}

port=18900
fail=0
for impl in "${impls[@]}"; do
  port=$((port + 1))
  start "$impl" "$port"
  echo "== $impl (http://127.0.0.1:$port)"
  if LSP2_TENANT_KEY=$LSP_TENANT_KEY_ACME "$tmp/lsp2-conformance" -gateway "http://127.0.0.1:$port" -tenant acme >"$tmp/$impl.conf" 2>&1; then
    echo "   conformance: $(tail -1 "$tmp/$impl.conf")"
  else
    echo "   conformance: FAILED"; grep -v '^PASS' "$tmp/$impl.conf"; fail=1
  fi
  if LSP2_EXTERNAL_GATEWAY="http://127.0.0.1:$port" LSP2_TENANT=acme LSP2_TENANT_KEY=$LSP_TENANT_KEY_ACME \
      go test -count=1 ./mock/external -run TestExternalGateway >"$tmp/$impl.e2e" 2>&1; then
    echo "   connector e2e: ok"
  else
    echo "   connector e2e: FAILED"; tail -20 "$tmp/$impl.e2e"; fail=1
  fi
done
exit $fail
