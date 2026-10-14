"""Check every spec/schemas/*.json against the JSON Schema 2020-12 meta-schema."""
import json, pathlib, sys
from jsonschema import Draft202012Validator
bad = 0
for f in sorted((pathlib.Path(__file__).resolve().parents[1] / "schemas").glob("*.json")):
    try:
        Draft202012Validator.check_schema(json.loads(f.read_text("utf-8")))
        print("OK  ", f.name)
    except Exception as e:  # noqa: BLE001
        bad += 1
        print("FAIL", f.name, e)
sys.exit(1 if bad else 0)
