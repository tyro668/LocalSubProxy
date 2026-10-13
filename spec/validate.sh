#!/usr/bin/env bash
# Validate all machine-readable lmbp contracts and run the example skeletons.
# Needs: python3 (venv), node >= 18.4, go; cargo optional.
set -euo pipefail
cd "$(dirname "$0")"
VENV=$(realpath -m "${VENV:-.venv}")
[ -x "$VENV/bin/python" ] || { python3 -m venv "$VENV" && "$VENV/bin/pip" install -q -r tools/requirements.txt; }
echo "== JSON Schema meta-validation";      "$VENV/bin/python" tools/check_schemas.py
echo "== OpenAPI 3.1";                      "$VENV/bin/openapi-spec-validator" openapi.yaml
echo "== AsyncAPI 3.0";                     (cd tools && npm install --silent && node asyncapi-validate.mjs ../asyncapi.yaml)
echo "== JOSE strict vectors (jose.json): Go reference, Node SDK, Python SDK"
(cd .. && go test ./internal/lmbp/ -run 'TestJOSEVectors' -count=1)
(cd ../sdk/node && npm run -s build && node --test test/jose.test.mjs 2>&1 | grep -E '^# (pass|fail)')
(cd ../sdk/python && { [ -x .venv/bin/python ] || { python3 -m venv .venv && .venv/bin/pip install -q -e '.[test]'; }; } && .venv/bin/python -m pytest -q tests/test_jose_vectors.py | tail -1)
echo "== Go reference vs vectors/schemas";  (cd .. && go test ./internal/lmbp/ ./internal/lmbp/conformance/)
echo "== Python skeleton";                  (cd examples/python && "$VENV/bin/python" test_vectors.py | tail -1)
echo "== Node skeleton";                    (cd examples/node && npm install --silent && node test_vectors.mjs | tail -1)
if command -v cargo >/dev/null; then echo "== Rust skeleton"; (cd examples/rust && cargo test --quiet --test vectors --test ext 2>&1 | grep "test result"); fi
