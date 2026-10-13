#!/usr/bin/env bash
# Validate all machine-readable lsp2 contracts and run the example skeletons.
# Needs: python3 (venv), node >= 18.4, go; cargo optional.
set -euo pipefail
cd "$(dirname "$0")"
VENV=$(realpath -m "${VENV:-.venv}")
[ -x "$VENV/bin/python" ] || { python3 -m venv "$VENV" && "$VENV/bin/pip" install -q -r tools/requirements.txt; }
echo "== JSON Schema meta-validation";      "$VENV/bin/python" tools/check_schemas.py
echo "== OpenAPI 3.1";                      "$VENV/bin/openapi-spec-validator" openapi.yaml
echo "== AsyncAPI 3.0";                     (cd tools && npm install --silent && node asyncapi-validate.mjs ../asyncapi.yaml)
echo "== Go reference vs vectors/schemas";  (cd .. && go test ./internal/lsp2/ ./internal/lsp2/conformance/)
echo "== Python skeleton";                  (cd examples/python && "$VENV/bin/python" test_vectors.py | tail -1)
echo "== Node skeleton";                    (cd examples/node && npm install --silent && node test_vectors.mjs | tail -1)
if command -v cargo >/dev/null; then echo "== Rust skeleton"; (cd examples/rust && cargo test --quiet --test vectors 2>&1 | grep "test result"); fi
