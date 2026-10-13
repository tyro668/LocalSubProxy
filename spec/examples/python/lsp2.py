"""lsp2 critical-path primitives for a Python gateway (reference skeleton).

Executed against spec/testvectors/vectors.json by test_vectors.py.
Dependencies: cryptography (Ed25519). JSON Schema validation: jsonschema.
"""
from __future__ import annotations

import base64
import hashlib
import json
import secrets
import time
from typing import Any

from cryptography.exceptions import InvalidSignature
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey, Ed25519PublicKey

TYP_ACCESS_TOKEN = "at+jwt"
TYP_ASSERTION = "JWT"
TYP_PROOF = "lsp2-proof+jwt"
TYP_TASK = "lsp2-task+jws"
LEEWAY = 30      # seconds, access tokens
PROOF_SKEW = 60  # seconds, proof JWT iat


class Lsp2Error(Exception):
    pass


def b64u(b: bytes) -> str:
    return base64.urlsafe_b64encode(b).rstrip(b"=").decode("ascii")


def unb64u(s: str) -> bytes:
    # Strict: reject padding and non-alphabet characters (base64.urlsafe_b64decode is lenient).
    if "=" in s or any(c not in "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_" for c in s):
        raise Lsp2Error("invalid base64url")
    if len(s) % 4 == 1:
        raise Lsp2Error("invalid base64url length")
    return base64.urlsafe_b64decode(s + "=" * (-len(s) % 4))


def json_bytes(obj: Any) -> bytes:
    # Compact UTF-8 JSON. Byte-exactness across languages is NOT required: verifiers
    # always verify the bytes they received.
    return json.dumps(obj, separators=(",", ":"), ensure_ascii=False).encode("utf-8")


def thumbprint(pub: bytes) -> str:
    """RFC 7638 JWK thumbprint of an Ed25519 public key (members in lexicographic order)."""
    return b64u(hashlib.sha256(('{"crv":"Ed25519","kty":"OKP","x":"%s"}' % b64u(pub)).encode()).digest())


def ath(credential: str) -> str:
    return b64u(hashlib.sha256(credential.encode("utf-8")).digest())


def sign_jws(key: Ed25519PrivateKey, typ: str, payload: bytes, kid: str | None = None) -> str:
    header = {"alg": "EdDSA", "typ": typ}
    if kid:
        header["kid"] = kid
    h = json.dumps(dict(sorted(header.items())), separators=(",", ":")).encode()
    signing_input = b64u(h) + "." + b64u(payload)
    return signing_input + "." + b64u(key.sign(signing_input.encode("ascii")))


def verify_jws(pub: bytes, compact: str, typ: str) -> bytes:
    parts = compact.split(".")
    if len(parts) != 3:
        raise Lsp2Error("malformed JWS")
    header = json.loads(unb64u(parts[0]))
    if header.get("alg") != "EdDSA" or header.get("typ") != typ or "crit" in header:
        raise Lsp2Error("bad JWS header")
    try:
        Ed25519PublicKey.from_public_bytes(pub).verify(unb64u(parts[2]), (parts[0] + "." + parts[1]).encode("ascii"))
    except InvalidSignature as e:
        raise Lsp2Error("bad signature") from e
    return unb64u(parts[1])


def verify_access_token(gw_pub: bytes, token: str, aud: str, now: int | None = None) -> dict:
    now = int(time.time()) if now is None else now
    c = json.loads(verify_jws(gw_pub, token, TYP_ACCESS_TOKEN))
    if c.get("aud") != aud or now > c["exp"] + LEEWAY or now + LEEWAY < c["iat"]:
        raise Lsp2Error("token aud/time invalid")
    return c


def issue_access_token(gw_key: Ed25519PrivateKey, kid: str, device_id: str, tenant: str, device_pub: bytes,
                       ttl: int = 900, now: int | None = None, jti: str | None = None) -> str:
    now = int(time.time()) if now is None else now
    # Member order mirrors the Go reference only so that test_vectors can compare bytes.
    claims = {"iss": "lsp2-gateway", "sub": device_id, "aud": "lsp2-gateway", "tenant": tenant, "iat": now,
              "exp": now + ttl, "jti": jti or "jti_" + secrets.token_hex(16), "cnf": {"jkt": thumbprint(device_pub)}}
    return sign_jws(gw_key, TYP_ACCESS_TOKEN, json_bytes(claims), kid)


def verify_proof(dev_pub: bytes, proof: str, iss: str, aud: str, credential: str, now: int | None = None) -> dict:
    now = int(time.time()) if now is None else now
    c = json.loads(verify_jws(dev_pub, proof, TYP_PROOF))
    if c.get("aud") != aud or c.get("iss") != iss or not c.get("jti") or c.get("ath") != ath(credential):
        raise Lsp2Error("proof claims invalid")
    if abs(now - c["iat"]) > PROOF_SKEW:
        raise Lsp2Error("proof iat outside skew")
    return c  # caller MUST also reject a repeated jti


def verify_hello(gw_pub: bytes, device_pub_of, frame: dict, access_token: str, aud: str = "lsp2-gateway",
                 now: int | None = None) -> dict:
    """Gateway side of the handshake: access token (Authorization header) + hello proof."""
    claims = verify_access_token(gw_pub, access_token, aud, now)
    dev_pub = device_pub_of(claims["sub"])  # look up the registered key; check status == active
    if claims.get("cnf", {}).get("jkt") != thumbprint(dev_pub):
        raise Lsp2Error("token not bound to this device key")
    if frame.get("v") != 2 or frame.get("type") != "hello" or frame["data"].get("device_id") != claims["sub"]:
        raise Lsp2Error("bad hello")
    verify_proof(dev_pub, frame["data"]["proof"], claims["sub"], "lsp2:hello", access_token, now)
    return claims


def sign_task_run(gw_key: Ed25519PrivateKey, kid: str, payload: dict) -> dict:
    """Build the task.run envelope; the payload travels only inside the JWS."""
    return {"v": 2, "type": "task.run", "task_id": payload["task_id"],
            "jws": sign_jws(gw_key, TYP_TASK, json_bytes(payload), kid)}


def verify_task_run(gw_pub: bytes, frame: dict) -> dict:
    p = json.loads(verify_jws(gw_pub, frame["jws"], TYP_TASK))
    if p.get("task_id") != frame.get("task_id"):
        raise Lsp2Error("task_id mismatch")
    return p


def sse_chunk(task_id: str, model: str, created: int, text: str | None = None, finish: str | None = None) -> bytes:
    """One OpenAI chat.completion.chunk as an SSE event."""
    delta = {"content": text} if text is not None else {}
    obj = {"id": "chatcmpl-" + task_id, "object": "chat.completion.chunk", "created": created, "model": model,
           "choices": [{"index": 0, "delta": delta, "finish_reason": finish}]}
    return b"data: " + json_bytes(obj) + b"\n\n"


SSE_DONE = b"data: [DONE]\n\n"
