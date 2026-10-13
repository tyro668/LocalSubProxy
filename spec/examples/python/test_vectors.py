"""Run: python test_vectors.py  (from this directory; needs cryptography, jsonschema)."""
import json
import pathlib
import sys

from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey

import lsp2

ROOT = pathlib.Path(__file__).resolve().parents[2]
V = json.loads((ROOT / "testvectors" / "vectors.json").read_text("utf-8"))
NOW = V["now"]
fails = 0


def check(name, ok, detail=""):
    global fails
    print(("PASS " if ok else "FAIL ") + name + (" " + detail if detail and not ok else ""))
    fails += 0 if ok else 1


def priv(n):
    return Ed25519PrivateKey.from_private_bytes(lsp2.unb64u(V["keys"][n]["seed_b64url"]))


def pub(n):
    return lsp2.unb64u(V["keys"][n]["public_b64url"])


for n, k in V["keys"].items():
    p = priv(n).public_key().public_bytes_raw()
    check(f"key {n}: seed→public, RFC 7638 thumbprint", lsp2.b64u(p) == k["public_b64url"] and lsp2.thumbprint(p) == k["jkt"])
for c in V["base64url"]:
    check(f"base64url {c['bytes_hex'] or '(empty)'}", lsp2.b64u(bytes.fromhex(c["bytes_hex"])) == c["b64url"])
for c in V["ath"]:
    check("ath", lsp2.ath(c["credential"]) == c["ath"])


def verify(kind, compact, key, exp):
    if kind == "access_token":
        c = lsp2.verify_access_token(key, compact, exp["aud"], NOW)
        if c["cnf"]["jkt"] != exp["cnf.jkt"]:
            raise lsp2.Lsp2Error("jkt")
    elif kind == "client_assertion":
        c = json.loads(lsp2.verify_jws(key, compact, lsp2.TYP_ASSERTION))
        if c["aud"] != exp["aud"]:
            raise lsp2.Lsp2Error("aud")
    elif kind == "proof":
        lsp2.verify_proof(key, compact, exp["iss"], exp["aud"], exp["credential"], NOW)
    elif kind == "task_run":
        lsp2.verify_task_run(key, {"task_id": exp["envelope_task_id"], "jws": compact})


for j in V["jws"]:
    sig = lsp2.b64u(priv(j["signed_by"]).sign(j["signing_input"].encode()))
    check(f"jws {j['name']}: deterministic Ed25519 signature", sig == j["signature_b64url"])
    try:
        verify(j["kind"], j["compact"], pub(j["signed_by"]), j["verify_with"])
        check(f"jws {j['name']}: verifies", True)
    except Exception as e:  # noqa: BLE001
        check(f"jws {j['name']}: verifies", False, repr(e))
for n in V["negative"]:
    try:
        verify(n["kind"], n["compact"], pub(n["verify_with_key"]), n["verify_with"])
        check(f"negative {n['name']}: rejected", False, "accepted")
    except Exception:  # noqa: BLE001
        check(f"negative {n['name']}: rejected", True)

# issue_access_token reproduces the reference token byte-for-byte (same member order, no HTML escaping needed).
at = next(j for j in V["jws"] if j["name"] == "access_token")
pl = at["payload"]
mine = lsp2.issue_access_token(priv("gateway"), at["protected_header"]["kid"], pl["sub"], pl["tenant"], pub("device"),
                               ttl=pl["exp"] - pl["iat"], now=pl["iat"], jti=pl["jti"])
check("issue_access_token == reference compact", mine == at["compact"], mine)

# verify_hello with the vector hello frame.
hello = next(f["frame"] for f in V["valid_frames"] if f["name"] == "hello")
try:
    lsp2.verify_hello(pub("gateway"), lambda _id: pub("device"), hello, at["compact"], now=NOW)
    check("verify_hello(vector hello)", True)
except Exception as e:  # noqa: BLE001
    check("verify_hello(vector hello)", False, repr(e))

# sign_task_run → verify_task_run round trip, including non-ASCII and </script>.
tr = next(j for j in V["jws"] if j["name"] == "task_run")
env = lsp2.sign_task_run(priv("gateway"), "k", tr["payload"])
check("sign_task_run round trip", lsp2.verify_task_run(pub("gateway"), env) == tr["payload"])
check("sse_chunk shape", lsp2.sse_chunk("t", "m", 1, "hi").startswith(b'data: {"id":"chatcmpl-t"'))

# Frames against JSON Schema 2020-12.
try:
    from jsonschema import Draft202012Validator
    from referencing import Registry, Resource

    sdir = ROOT / "schemas"
    reg = Registry()
    for f in sdir.glob("*.json"):
        doc = json.loads(f.read_text("utf-8"))
        Draft202012Validator.check_schema(doc)
        reg = reg.with_resource(doc["$id"], Resource.from_contents(doc))
    env_schema = json.loads((sdir / "envelope.json").read_text("utf-8"))
    val = Draft202012Validator(env_schema, registry=reg)
    for f in V["valid_frames"]:
        errs = list(val.iter_errors(f["frame"]))
        check(f"schema valid frame {f['name']}", not errs, str(errs[:1]))
    for f in V["invalid_frames"]:
        check(f"schema rejects {f['name']}", not val.is_valid(f["frame"]))
except ImportError:
    print("SKIP schema checks (pip install jsonschema)")

print(f"\n{fails} failed")
sys.exit(1 if fails else 0)
