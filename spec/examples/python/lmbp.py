"""lmbp critical-path primitives for a Python gateway (reference skeleton).

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
from urllib.parse import urlsplit

from cryptography.exceptions import InvalidSignature
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey, Ed25519PublicKey

TYP_ACCESS_TOKEN = "at+jwt"
TYP_ASSERTION = "JWT"
TYP_PROOF = "lmbp-proof+jwt"
TYP_TASK = "lmbp-task+jws"
TYP_METADATA = "lmbp-metadata+jwt"
LEEWAY = 30      # seconds, access tokens
PROOF_SKEW = 60  # seconds, proof JWT iat
METADATA_MAX_AGE = 86400
HEADER_MEMBERS = {"alg", "typ", "kid"}


class LmbpError(Exception):
    pass


def b64u(b: bytes) -> str:
    return base64.urlsafe_b64encode(b).rstrip(b"=").decode("ascii")


def unb64u(s: str) -> bytes:
    # Strict: reject padding and non-alphabet characters (base64.urlsafe_b64decode is lenient).
    if "=" in s or any(c not in "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_" for c in s):
        raise LmbpError("invalid base64url")
    if len(s) % 4 == 1:
        raise LmbpError("invalid base64url length")
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


def _no_constant(name: str):
    raise LmbpError(f"invalid JSON constant {name}")


def verify_jws(pub: bytes, compact: str, typ: str) -> dict:
    """§5.2: header exactly {alg: "EdDSA", typ, kid?}; kid, when present, is the
    RFC 7638 thumbprint of pub; the payload MUST be a JSON object (returned)."""
    parts = compact.split(".") if isinstance(compact, str) else []
    if len(parts) != 3:
        raise LmbpError("malformed JWS")
    try:
        header = json.loads(unb64u(parts[0]), parse_constant=_no_constant)
    except (ValueError, LmbpError) as e:
        raise LmbpError("bad JWS header") from e
    if not isinstance(header, dict):
        raise LmbpError("bad JWS header")
    extra = set(header) - HEADER_MEMBERS
    if extra:
        raise LmbpError(f"forbidden JWS header {sorted(extra)}")
    if header.get("alg") != "EdDSA":
        raise LmbpError("JWS alg must be EdDSA")
    if not isinstance(header.get("typ"), str) or header["typ"] != typ:
        raise LmbpError(f"JWS typ must be {typ}")
    if "kid" in header and (not isinstance(header["kid"], str) or header["kid"] != thumbprint(pub)):
        raise LmbpError("JWS kid does not match the verifying key")
    try:
        Ed25519PublicKey.from_public_bytes(pub).verify(unb64u(parts[2]), (parts[0] + "." + parts[1]).encode("ascii"))
    except (InvalidSignature, ValueError) as e:
        raise LmbpError("bad signature") from e
    try:
        payload = json.loads(unb64u(parts[1]), parse_constant=_no_constant)
    except (ValueError, LmbpError) as e:
        raise LmbpError("bad JWS payload") from e
    if not isinstance(payload, dict):
        raise LmbpError("JWS payload must be a JSON object")
    return payload


def _is_uint(v: Any) -> bool:
    # json.loads keeps 1.0 / 1e3 as float; bool is a subclass of int.
    return type(v) is int and 0 <= v <= 2**53 - 1


def require_claims(c: dict, ints=(), strs=(), opt=()) -> None:
    """Claim types (§5.2): NumericDates are JSON integers 0..2^53-1, required
    identifiers non-empty strings, optional ones strings."""
    for k in ints:
        if not _is_uint(c.get(k)):
            raise LmbpError(f"claim {k} must be a non-negative integer")
    for k in strs:
        if not isinstance(c.get(k), str) or c[k] == "":
            raise LmbpError(f"claim {k} must be a non-empty string")
    for k in opt:
        if not isinstance(c.get(k), str):
            raise LmbpError(f"claim {k} must be a string")


def time_range(c: dict) -> None:
    require_claims(c, ints=("iat", "exp"))
    if c["exp"] < c["iat"]:
        raise LmbpError("exp before iat")


def verify_jwt(pub: bytes, token: str, typ: str, aud: str, now: int | None = None) -> dict:
    """Access tokens (at+jwt) and client assertions (JWT): iss/sub/aud/jti, iat <= exp, aud, 30 s leeway."""
    now = int(time.time()) if now is None else now
    c = verify_jws(pub, token, typ)
    require_claims(c, strs=("iss", "sub", "aud", "jti"))
    time_range(c)
    if c["aud"] != aud:
        raise LmbpError("wrong audience")
    if now > c["exp"] + LEEWAY or now + LEEWAY < c["iat"]:
        raise LmbpError("token expired")
    return c


def verify_access_token(gw_pub: bytes, token: str, aud: str, now: int | None = None) -> dict:
    return verify_jwt(gw_pub, token, TYP_ACCESS_TOKEN, aud, now)


def verify_client_assertion(dev_pub: bytes, token: str, aud: str, now: int | None = None) -> dict:
    return verify_jwt(dev_pub, token, TYP_ASSERTION, aud, now)  # caller MUST reject a repeated jti


# §18: iss is the gateway's https origin; aud binds the token to one tenant.
EXAMPLE_ISSUER = "https://gw.example.com"


def token_audience(issuer: str, tenant: str) -> str:
    return f"{issuer}/tenants/{tenant}"


def issue_access_token(gw_key: Ed25519PrivateKey, kid: str, device_id: str, tenant: str, device_pub: bytes,
                       ttl: int = 900, now: int | None = None, jti: str | None = None, issuer: str = EXAMPLE_ISSUER) -> str:
    now = int(time.time()) if now is None else now
    # Member order mirrors the Go reference only so that test_vectors can compare bytes.
    claims = {"iss": issuer, "sub": device_id, "aud": token_audience(issuer, tenant), "tenant": tenant, "iat": now,
              "exp": now + ttl, "jti": jti or "jti_" + secrets.token_hex(16), "cnf": {"jkt": thumbprint(device_pub)}}
    return sign_jws(gw_key, TYP_ACCESS_TOKEN, json_bytes(claims), kid)


def verify_proof(dev_pub: bytes, proof: str, iss: str, aud: str, credential: str, now: int | None = None) -> dict:
    now = int(time.time()) if now is None else now
    c = verify_jws(dev_pub, proof, TYP_PROOF)
    require_claims(c, ints=("iat",), strs=("aud", "jti", "ath"), opt=("iss",))
    if c["aud"] != aud or c["iss"] != iss or c["ath"] != ath(credential):
        raise LmbpError("proof claims invalid")
    if abs(now - c["iat"]) > PROOF_SKEW:
        raise LmbpError("proof iat outside skew")
    return c  # caller MUST also reject a repeated jti


def verify_hello(gw_pub: bytes, device_pub_of, frame: dict, access_token: str, aud: str = token_audience(EXAMPLE_ISSUER, "acme"),
                 now: int | None = None) -> dict:
    """Gateway side of the handshake: access token (Authorization header) + hello proof."""
    claims = verify_access_token(gw_pub, access_token, aud, now)
    dev_pub = device_pub_of(claims["sub"])  # look up the registered key; check status == active
    if claims.get("cnf", {}).get("jkt") != thumbprint(dev_pub):
        raise LmbpError("token not bound to this device key")
    if frame.get("v") != 2 or frame.get("type") != "hello" or frame["data"].get("device_id") != claims["sub"]:
        raise LmbpError("bad hello")
    verify_proof(dev_pub, frame["data"]["proof"], claims["sub"], "lmbp:hello", access_token, now)
    return claims


def sign_task_run(gw_key: Ed25519PrivateKey, kid: str, payload: dict) -> dict:
    """Build the task.run envelope; the payload travels only inside the JWS."""
    return {"v": 2, "type": "task.run", "task_id": payload["task_id"], "seq": 1,
            "jws": sign_jws(gw_key, TYP_TASK, json_bytes(payload), kid)}


def verify_task_run(gw_pub: bytes, frame: dict, now: int | None = None) -> dict:
    """task.run (§5.6): task_id/device_id/model/nonce, iat <= exp, strict blob
    references, bound to the envelope task_id; expired after exp when now is given."""
    p = verify_jws(gw_pub, frame["jws"], TYP_TASK)
    require_claims(p, strs=("task_id", "device_id", "model", "nonce"))
    time_range(p)
    if "blobs" in p:
        if not isinstance(p["blobs"], list):
            raise LmbpError("blobs must be an array")
        for b in p["blobs"]:
            if not isinstance(b, dict):
                raise LmbpError("blob ref must be an object")
            require_claims(b, ints=("size",), strs=("id", "media_type", "sha256"))
            try:
                d = unb64u(b["sha256"])
            except (LmbpError, ValueError):
                d = b""
            if len(d) != 32:
                raise LmbpError("blob sha256 must be 32 bytes base64url")
    if p["task_id"] != frame.get("task_id"):
        raise LmbpError("task_id mismatch")
    if now is not None and now > p["exp"]:
        raise LmbpError("task.run expired")
    return p


def _origin(u: Any) -> str:
    try:
        s = urlsplit(u)
        port = s.port
    except (TypeError, ValueError, AttributeError):
        return ""
    if s.scheme not in ("http", "https") or not s.hostname:
        return ""
    default = {"http": 80, "https": 443}[s.scheme]
    return f"{s.scheme}://{s.hostname}" + (f":{port}" if port and port != default else "")


def verify_metadata(gw_pub: bytes, jws: str, issuer: str, tenant: str, now: int | None = None) -> dict:
    """Signed tenant metadata (§19.1): bound to issuer, tenant and token audience,
    speaks 2.0, names the SaaS, valid at most 24 h, endpoints on the issuer origin."""
    now = int(time.time()) if now is None else now
    m = verify_jws(gw_pub, jws, TYP_METADATA)
    require_claims(m, strs=("issuer", "tenant", "audience"))
    time_range(m)
    if not isinstance(m.get("protocol_versions"), list) or "2.0" not in m["protocol_versions"]:
        raise LmbpError("gateway does not speak LMBP 2.0")
    if m["issuer"] != issuer or m["tenant"] != tenant or m["audience"] != token_audience(issuer, tenant):
        raise LmbpError("metadata issuer/tenant/audience mismatch")
    saas = m.get("saas")
    if not isinstance(saas, dict) or not isinstance(saas.get("name"), str) or not saas["name"].strip():
        raise LmbpError("metadata lacks saas.name")
    if now > m["exp"] + LEEWAY or now + LEEWAY < m["iat"] or m["exp"] - m["iat"] > METADATA_MAX_AGE:
        raise LmbpError("metadata expired or validity too long")
    for u in (m.get("jwks_uri"), m.get("device_authorization_endpoint"), m.get("token_endpoint")):
        if _origin(u) != _origin(issuer):
            raise LmbpError(f"endpoint {u!r} is not on the issuer origin")
    return m


def sse_chunk(task_id: str, model: str, created: int, text: str | None = None, finish: str | None = None) -> bytes:
    """One OpenAI chat.completion.chunk as an SSE event."""
    delta = {"content": text} if text is not None else {}
    obj = {"id": "chatcmpl-" + task_id, "object": "chat.completion.chunk", "created": created, "model": model,
           "choices": [{"index": 0, "delta": delta, "finish_reason": finish}]}
    return b"data: " + json_bytes(obj) + b"\n\n"


SSE_DONE = b"data: [DONE]\n\n"
