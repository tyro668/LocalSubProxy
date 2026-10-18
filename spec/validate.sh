#!/usr/bin/env bash
# Validate all machine-readable lmbp contracts and run the example skeletons.
# Needs: python3 (venv), node >= 18.4, go; cargo (optional locally, required when CI/LMBP_REQUIRE_EXTERNAL is set).
set -euo pipefail
cd "$(dirname "$0")"
# Test runs go through scripts/strict-test.py: results come from go test -json,
# the node:test TAP reporter, pytest junit XML and cargo's result lines (never
# from filtered output), and any skip/todo/ignored test is a failure.
ST=$(realpath ../scripts/strict-test.py)
VENV=$(realpath -m "${VENV:-.venv}")
[ -x "$VENV/bin/python" ] || { python3 -m venv "$VENV" && "$VENV/bin/pip" install -q -r tools/requirements.txt; }
echo "== JSON Schema meta-validation";      "$VENV/bin/python" tools/check_schemas.py
echo "== OpenAPI 3.1";                      "$VENV/bin/openapi-spec-validator" openapi.yaml
echo "== AsyncAPI 3.0";                     (cd tools && npm install --silent && node asyncapi-validate.mjs ../asyncapi.yaml)
echo "== Spec examples vs strict JOSE vectors; spec §13.2 table vs lmbp-conformance -list"
(cd .. && $ST go ./internal/lmbp/ -run 'TestSpecExamples' -count=1)
(cd .. && $ST go ./internal/lmbp/conformance/ -run 'TestSpecCatalogMatches' -count=1)
echo "== welcome.json: max_concurrency required, max_frame_bytes exactly 262144 (boundary negatives in vectors.json); no other value in examples"
(cd .. && $ST go ./internal/lmbp/ -run 'TestSchemasAgainstVectors|TestVectors' -count=1)
if grep -nE '"?max_frame_bytes"?[": ]+[0-9]+' asyncapi.yaml openapi.yaml ../docs/*.md | grep -vE 'max_frame_bytes"?[": ]+262144\b'; then echo "FAIL an example uses max_frame_bytes other than 262144"; exit 1; fi
echo "== JOSE strict vectors (jose.json): Go reference, Node SDK, Python SDK"
(cd .. && $ST go ./internal/lmbp/ -run 'TestJOSEVectors' -count=1)
(cd ../sdk/node && npm run -s build && $ST node test/jose.test.mjs)
(cd ../sdk/python && { [ -x .venv/bin/python ] || { python3 -m venv .venv && .venv/bin/pip install -q -e '.[test]'; }; } && $ST pytest .venv/bin/python -q tests/test_jose_vectors.py)
echo "== Request-parameter vectors (params.json) + bundled schema copies: Go, Node SDK, Python SDK"
for n in defs.json task.run.payload.json hello.json models.update.json task.done.json task.error.json; do
  cmp schemas/$n ../sdk/node/schemas/$n && cmp schemas/$n ../sdk/python/src/lmbp_server/schemas/$n || { echo "SDK copy of $n is stale"; exit 1; }
done
(cd .. && $ST go ./internal/lmbp/gateway -run 'TestParamVectors|TestTaskRunSchemaBackstop' -count=1)
(cd ../sdk/node && $ST node test/params.test.mjs)
(cd ../sdk/python && $ST pytest .venv/bin/python -q tests/test_params.py)
echo "== Go reference vs vectors/schemas";  (cd .. && $ST go ./internal/lmbp/ ./internal/lmbp/conformance/)
echo "== Python skeleton (vectors.json + all jose.json cases)";                  (cd examples/python && "$VENV/bin/python" test_vectors.py)
echo "== Node skeleton (vectors.json + all jose.json cases)";                    (cd examples/node && npm install --silent && node test_vectors.mjs)
if command -v cargo >/dev/null; then echo "== Rust skeleton (vectors.json + all jose.json cases)"; (cd examples/rust && $ST cargo --quiet --test vectors --test ext)
elif [ -n "${CI:-}${LMBP_REQUIRE_EXTERNAL:-}" ]; then echo "FAIL Rust skeleton: cargo not installed (required in CI mode)"; exit 1
else echo "SKIP Rust skeleton (cargo not installed)"; fi
