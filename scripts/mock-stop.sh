#!/usr/bin/env bash
# Stop the stack started by scripts/mock-start.sh (keeps keys and data in
# .mock/; remove that directory to start from scratch).
set -eu
repo=$(cd "$(dirname "$0")/.." && pwd)
dir=${LMBP_MOCK_DIR:-$repo/.mock}
n=0
for f in "$dir"/run/*.pid; do
  [ -f "$f" ] || continue
  pid=$(cat "$f")
  if kill "$pid" 2>/dev/null; then
    i=0
    while kill -0 "$pid" 2>/dev/null && [ $i -lt 50 ]; do sleep 0.1; i=$((i + 1)); done
    kill -9 "$pid" 2>/dev/null || true
    echo "stopped $(basename "$f" .pid) (pid $pid)"
    n=$((n + 1))
  fi
  rm -f "$f"
done
[ $n -gt 0 ] || echo "nothing running"
