"""Run: python test_vectors.py  (from this directory; needs cryptography, jsonschema)."""
import json
import pathlib
import sys

from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey

import lmbp

ROOT = pathlib.Path(__file__).resolve().parents[2]
V = json.loads((ROOT / "testvectors" / "vectors.json").read_text("utf-8"))
NOW = V["now"]
fails = 0


def check(name, ok, detail=""):
    global fails
    print(("PASS " if ok else "FAIL ") + name + (" " + detail if detail and not ok else ""))
    fails += 0 if ok else 1


def priv(n):
    return Ed25519PrivateKey.from_private_bytes(lmbp.unb64u(V["keys"][n]["seed_b64url"]))


def pub(n):
    return lmbp.unb64u(V["keys"][n]["public_b64url"])


for n, k in V["keys"].items():
    p = priv(n).public_key().public_bytes_raw()
    check(f"key {n}: seed→public, RFC 7638 thumbprint", lmbp.b64u(p) == k["public_b64url"] and lmbp.thumbprint(p) == k["jkt"])
for c in V["base64url"]:
    check(f"base64url {c['bytes_hex'] or '(empty)'}", lmbp.b64u(bytes.fromhex(c["bytes_hex"])) == c["b64url"])
for c in V["ath"]:
    check("ath", lmbp.ath(c["credential"]) == c["ath"])


def verify(kind, compact, key, exp):
    if kind == "access_token":
        c = lmbp.verify_access_token(key, compact, exp["aud"], NOW)
        if c["cnf"]["jkt"] != exp["cnf.jkt"]:
            raise lmbp.LmbpError("jkt")
    elif kind == "client_assertion":
        c = json.loads(lmbp.verify_jws(key, compact, lmbp.TYP_ASSERTION))
        if c["aud"] != exp["aud"]:
            raise lmbp.LmbpError("aud")
    elif kind == "proof":
        lmbp.verify_proof(key, compact, exp["iss"], exp["aud"], exp["credential"], NOW)
    elif kind == "task_run":
        lmbp.verify_task_run(key, {"task_id": exp["envelope_task_id"], "jws": compact})


for j in V["jws"]:
    sig = lmbp.b64u(priv(j["signed_by"]).sign(j["signing_input"].encode()))
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
mine = lmbp.issue_access_token(priv("gateway"), at["protected_header"]["kid"], pl["sub"], pl["tenant"], pub("device"),
                               ttl=pl["exp"] - pl["iat"], now=pl["iat"], jti=pl["jti"])
check("issue_access_token == reference compact", mine == at["compact"], mine)

# verify_hello with the vector hello frame.
hello = next(f["frame"] for f in V["valid_frames"] if f["name"] == "hello")
try:
    lmbp.verify_hello(pub("gateway"), lambda _id: pub("device"), hello, at["compact"], now=NOW)
    check("verify_hello(vector hello)", True)
except Exception as e:  # noqa: BLE001
    check("verify_hello(vector hello)", False, repr(e))

# sign_task_run → verify_task_run round trip, including non-ASCII and </script>.
tr = next(j for j in V["jws"] if j["name"] == "task_run")
env = lmbp.sign_task_run(priv("gateway"), "k", tr["payload"])
check("sign_task_run round trip", lmbp.verify_task_run(pub("gateway"), env) == tr["payload"])
check("sse_chunk shape", lmbp.sse_chunk("t", "m", 1, "hi").startswith(b'data: {"id":"chatcmpl-t"'))

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

# ---- extensions: E2EE, flow control, resume ----
import lmbp_ext as x  # noqa: E402

E = V["e2ee"]
try:
    xp, kid = x.verify_key_statement(pub("device"), E["key_statement"]["pinned_jkt"], E["key_statement"]["device_id"], E["key_statement"]["compact"], NOW)
    check("e2ee key statement verifies against pinned jkt", lmbp.b64u(xp) == E["key_statement"]["expect_x25519_public_b64url"] and kid == E["key_statement"]["expect_kid"])
except Exception as e:  # noqa: BLE001
    check("e2ee key statement verifies against pinned jkt", False, repr(e))
xpriv = lmbp.unb64u(E["device_x25519"]["private_b64url"])
from cryptography.hazmat.primitives.asymmetric.x25519 import X25519PrivateKey  # noqa: E402
check("e2ee X25519 public + RFC 7638 kid", lmbp.b64u(X25519PrivateKey.from_private_bytes(xpriv).public_key().public_bytes_raw()) == E["device_x25519"]["public_b64url"]
      and x.x25519_thumbprint(lmbp.unb64u(E["device_x25519"]["public_b64url"])) == E["device_x25519"]["kid"])
J = E["jwe"]
check("e2ee Concat KDF (RFC 7518 §4.6.2) reproduces cek", lmbp.b64u(x.concat_kdf(lmbp.unb64u(J["shared_secret_b64url"]))) == J["cek_b64url"])
mine = x.seal_jwe(lmbp.unb64u(E["device_x25519"]["public_b64url"]), J["recipient_kid"], J["plaintext_utf8"].encode(), lmbp.unb64u(J["ephemeral_private_b64url"]), lmbp.unb64u(J["iv_b64url"]))
check("e2ee seal_jwe with vector ephemeral key/IV: ciphertext and tag match", mine.split(".")[2:] == J["compact"].split(".")[2:] or mine == J["compact"], mine)
try:
    h, pt = x.open_jwe(xpriv, J["compact"])
    check("e2ee open_jwe(reference JWE)", pt.decode() == J["plaintext_utf8"] and h["cty"] == x.CTY_REQUEST)
except Exception as e:  # noqa: BLE001
    check("e2ee open_jwe(reference JWE)", False, repr(e))
h, pt = x.open_jwe(xpriv, mine)
check("e2ee seal→open round trip", pt.decode() == J["plaintext_utf8"])
for c in E["chunks"]:
    k = lmbp.unb64u(c["response_key_b64url"])
    check(f"e2ee chunk seq {c['seq']} {c['type']}: seal matches", x.seal_chunk(k, c["rid"], c["type"], c["seq"], c["plaintext_utf8"].encode()) == c["sealed_b64url"])
    check(f"e2ee chunk seq {c['seq']}: nonce/aad", x.chunk_nonce(c["seq"]).hex() == c["nonce_hex"] and x.chunk_aad(c["rid"], c["type"], c["seq"]).decode() == c["aad"])
k0 = lmbp.unb64u(E["chunks"][0]["response_key_b64url"])
for n in E["negative"]:
    try:
        if n["kind"] == "jwe":
            x.open_jwe(xpriv, n["compact"])
        elif n["kind"] == "chunk":
            x.open_chunk(k0, n["rid"], n["type"], n["seq"], n["sealed_b64url"])
        else:
            x.verify_key_statement(pub("device"), n["pinned_jkt"], n["device_id"], n["compact"], NOW)
        check(f"e2ee negative {n['name']}: rejected", False, "accepted")
    except Exception:  # noqa: BLE001
        check(f"e2ee negative {n['name']}: rejected", True)
for f in V["flow"]:
    ok = True
    if f["role"] == "sender":
        sent = {}
        for st in f["steps"]:
            if st["op"] == "send":
                tu = sum(sz for _, sz in sent.get(st["task"], []))
                cu = sum(sz for l in sent.values() for _, sz in l)
                can = x.can_send(f["task_window"], f["conn_window"], tu, cu, st["size"])
                ok &= can == (st["expect"] == "send")
                if can:
                    l = sent.setdefault(st["task"], [])
                    l.append(((l[-1][0] if l else 0) + 1, st["size"]))
            else:
                sent[st["task"]] = [e for e in sent.get(st["task"], []) if e[0] > st["seq"]]
    else:
        inb = x.Inbound(f["task_window"], f["conn_window"])
        for st in f["steps"]:
            if st["op"] == "recv":
                ok &= inb.accept(st["task"], st["seq"], st["size"]) == st["expect"]
            else:
                inb.acked(st["task"], st["seq"])
    check(f"flow {f['name']}", ok)
for r in V["resume"]:
    unacked = [s for s in r["sent"] if s > r["acked"]]
    check(f"resume {r['name']}", x.retransmit(unacked, r["peer_recv_seq"]) == r["retransmit"])

print(f"\n{fails} failed")
sys.exit(1 if fails else 0)
