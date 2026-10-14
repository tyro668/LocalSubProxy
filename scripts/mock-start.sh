#!/usr/bin/env bash
# Start a local mock stack for development: the fake model (Ollama-compatible
# /api/chat and OpenAI /v1) plus one gateway with the chat page, then print
# the chat URL, the dev login and the exact connector pairing command.
#
#   scripts/mock-start.sh [go|node|python|java|rust] [port]    # default: go 8080
#   (java/rust keep gateway state in memory: no --db, pairings end at a restart)
#   scripts/mock-stop.sh                              # stop everything
#
# State lives in .mock/ (gitignored, override with LMBP_MOCK_DIR): dev keys
# in .mock/env (created once, mode 600), the tenant profile, logs and pids.
# Keys are kept across restarts, so a paired connector keeps working.
# Works with macOS bash 3.2 / BSD tools and Linux.
set -eu
repo=$(cd "$(dirname "$0")/.." && pwd)
cd "$repo"
impl=${1:-go}
port=${2:-8080}
model_port=${LMBP_MOCK_MODEL_PORT:-11500}
models=${LMBP_MOCK_MODELS:-qwen3-8b}
dir=${LMBP_MOCK_DIR:-$repo/.mock}
case $impl in go|node|python|java|rust) ;; *) echo "usage: $0 [go|node|python|java|rust] [port]" >&2; exit 2 ;; esac
for t in go curl openssl; do command -v "$t" >/dev/null || { echo "missing tool: $t" >&2; exit 1; }; done

mkdir -p "$dir/bin" "$dir/logs" "$dir/run" "$dir/connector"
if [ -n "$(ls "$dir/run" 2>/dev/null)" ]; then
  echo "a mock stack is already running (see $dir/run); stop it first: scripts/mock-stop.sh" >&2
  exit 1
fi

# --- dev keys (generated once; never commit .mock/) ---
if [ ! -f "$dir/env" ]; then
  umask 077
  {
    echo "LSP_TENANT_KEY_ACME=$(openssl rand -hex 24)"
    echo "LSP_SIGNING_SEED=$(openssl rand -hex 32)"
    echo "LOCALSUBPROXY_DATA_KEY=$(openssl rand -hex 32)"
  } >"$dir/env"
  umask 022
fi
set -a; . "$dir/env"; set +a
export LMBP_SIGNING_SEED=$LSP_SIGNING_SEED   # node/python/java/rust read this name

gw="http://127.0.0.1:$port"
cat >"$dir/tenant-profiles.json" <<JSON
{
  "acme": {
    "api_key_env": "LSP_TENANT_KEY_ACME",
    "display_name": "Acme Chat (mock)",
    "homepage_uri": "$gw",
    "pairing_uri": "$gw/chat",
    "requested_models": ["$(echo "$models" | cut -d, -f1)"],
    "requested_scopes": ["lmbp.chat", "lmbp.tools", "lmbp.vision"]
  }
}
JSON

say() { printf '%s\n' "$*"; }
bg() { # name cmd...   start in the background, record the pid
  local name=$1; shift
  nohup "$@" >"$dir/logs/$name.log" 2>&1 &
  echo $! >"$dir/run/$name.pid"
}
wait_http() { # name url
  local i=0
  while [ $i -lt 100 ]; do
    curl -fsS "$2" >/dev/null 2>&1 && return 0
    kill -0 "$(cat "$dir/run/$1.pid")" 2>/dev/null || break
    sleep 0.2; i=$((i + 1))
  done
  say "$1 did not start; last log lines ($dir/logs/$1.log):"; tail -n 20 "$dir/logs/$1.log" >&2
  "$repo/scripts/mock-stop.sh" >/dev/null 2>&1 || true
  exit 1
}

say "building (go) ..."
go build -o "$dir/bin/lspmock" ./mock/cmd/lspmock
go build -o "$dir/bin/localsubproxy" ./cmd/localsubproxy

bg fakemodel "$dir/bin/lspmock" fakemodel -listen "127.0.0.1:$model_port" -models "$models"
wait_http fakemodel "http://127.0.0.1:$model_port/api/tags"

case $impl in
  go)
    bg gateway "$dir/bin/localsubproxy" server -listen "127.0.0.1:$port" -db "$dir/gateway.db" -tenants acme:LSP_TENANT_KEY_ACME \
      -tenant-profiles "$dir/tenant-profiles.json" -issuer "$gw" -chat-ui ;;
  node)
    command -v node >/dev/null || { echo "missing tool: node" >&2; exit 1; }
    (cd sdk/node && { [ -d node_modules ] || npm ci --no-audit --no-fund >/dev/null; } && npm run -s build)
    (cd mock/node && { [ -d node_modules ] || npm ci --no-audit --no-fund >/dev/null; })
    bg gateway node mock/node/gateway.mjs --listen "127.0.0.1:$port" --issuer "$gw" \
      --tenant-profiles "$dir/tenant-profiles.json" --db "$dir/gateway-node.db" ;;
  python)
    command -v python3 >/dev/null || { echo "missing tool: python3" >&2; exit 1; }
    [ -x mock/python/.venv/bin/python ] || { python3 -m venv mock/python/.venv && (cd mock/python && .venv/bin/pip install -q -r requirements.txt); }
    bg gateway mock/python/.venv/bin/python mock/python/gateway.py --listen "127.0.0.1:$port" --issuer "$gw" \
      --tenant-profiles "$dir/tenant-profiles.json" --db "$dir/gateway-python.db" ;;
  java)   # JDK 21+, no dependencies; in-memory state (no --db)
    for t in java javac jar; do command -v "$t" >/dev/null || { echo "missing tool: $t (JDK 21+)" >&2; exit 1; }; done
    mock/java/build.sh >/dev/null
    bg gateway java -jar mock/java/build/lmbp-mock.jar --listen "127.0.0.1:$port" --issuer "$gw" \
      --tenant-profiles "$dir/tenant-profiles.json" --web-dir "$repo/mock/web" ;;
  rust)   # in-memory state (no --db)
    command -v cargo >/dev/null || { echo "missing tool: cargo" >&2; exit 1; }
    (cd mock/rust && cargo build -q --release)
    bg gateway mock/rust/target/release/lmbp-mock --listen "127.0.0.1:$port" --issuer "$gw" \
      --tenant-profiles "$dir/tenant-profiles.json" --web-dir "$repo/mock/web" ;;
esac
wait_http gateway "$gw/healthz"

enc=$(printf '%s' "$gw" | sed -e 's/%/%25/g' -e 's/:/%3A/g' -e 's#/#%2F#g')
link="lmbp://pair?v=1&gateway=$enc&tenant=acme"
cdir="$dir/connector"
say ""
say "mock stack is up ($impl gateway)"
say "  chat page   $gw/chat"
say "  login       tenant: acme   user: alice   key: $LSP_TENANT_KEY_ACME"
say "  fake model  http://127.0.0.1:$model_port  (models: $models)"
say "  pair link   $link"
say ""
say "connect a device (in another terminal; confirm, then approve the printed code on the chat page):"
say "  LOCALSUBPROXY_DATA_KEY=$LOCALSUBPROXY_DATA_KEY $dir/bin/localsubproxy connector \\"
say "    -pair-link '$link' \\"
say "    -adapter-id local -ollama-url http://127.0.0.1:$model_port -models $models -capabilities vision \\"
say "    -identity $cdir/identity.json -adapter-file $cdir/adapters.enc -audit-db $cdir/audit.db"
say ""
say "logs: $dir/logs/   stop: scripts/mock-stop.sh"
