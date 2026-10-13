"""End-to-end encryption from business code (spec §17).

The gateway relays the sealed request and the sealed response frames but
cannot read them; only your backend and the user's device can::

    dev = server.pairing.list_devices(tenant, user)[0]
    sealed = seal_e2ee_request(dev, pinned_jkt, {"model": m, "messages": msgs})
    async for c in client.chat.completions.stream(model=m, lmbp_e2ee=sealed.jwe):
        if c.get("lmbp_e2ee"):
            text += decode_e2ee_delta(sealed.open(c["lmbp_e2ee"])).get("text", "")

pinned_jkt is the device key thumbprint your backend recorded at pairing
(ApproveResult.jkt); pinning stops a compromised gateway from substituting its
own encryption key.
"""
from __future__ import annotations

import hashlib
import json
import os
import struct
from dataclasses import dataclass
from typing import Any

from cryptography.hazmat.primitives.asymmetric.x25519 import X25519PrivateKey, X25519PublicKey
from cryptography.hazmat.primitives.ciphers.aead import AESGCM

from . import protocol as L

TYP_E2EE_KEY = "lmbp-e2ee-key+jwt"
CTY_REQUEST = "lmbp-request+json"


def verify_key_statement(device_pub: bytes, pinned_jkt: str, device_id: str, stmt: str, t: int | None = None) -> tuple[bytes, str]:
    """Verify a device key statement against the pinned identity thumbprint; returns (X25519 key, kid)."""
    if not pinned_jkt:
        raise ValueError("pinned device thumbprint required")
    if L.thumbprint(device_pub) != pinned_jkt:
        raise ValueError("device key does not match pinned thumbprint")
    L.verify_e2ee_key(device_pub, device_id, stmt, t)
    jwk = json.loads(L.unb64(stmt.split(".")[1]))["jwk"]
    return L.unb64(jwk["x"]), jwk["kid"]


def _kdf(z: bytes, enc: str = "A256GCM") -> bytes:
    return hashlib.sha256(struct.pack(">I", 1) + z + struct.pack(">I", len(enc)) + enc.encode() + struct.pack(">II", 0, 0) + struct.pack(">I", 256)).digest()


def _nonce(seq: int) -> bytes:
    return b"\x00" * 4 + struct.pack(">Q", seq)


def _aad(rid: str, typ: str, seq: int) -> bytes:
    return f"lmbp-e2ee/v1|{rid}|{typ}|{seq}".encode()


def seal_jwe(recipient: bytes, kid: str, plaintext: bytes) -> str:
    eph = X25519PrivateKey.generate()
    z = eph.exchange(X25519PublicKey.from_public_bytes(recipient))
    hdr = {"alg": "ECDH-ES", "enc": "A256GCM", "epk": {"kty": "OKP", "crv": "X25519", "x": L.b64(eph.public_key().public_bytes_raw())},
           "kid": kid, "cty": CTY_REQUEST}
    prot = L.b64(json.dumps(hdr, separators=(",", ":")).encode())
    iv = os.urandom(12)
    ct = AESGCM(_kdf(z)).encrypt(iv, plaintext, prot.encode("ascii"))
    return f"{prot}..{L.b64(iv)}.{L.b64(ct[:-16])}.{L.b64(ct[-16:])}"


def open_chunk(key: bytes, rid: str, typ: str, seq: int, sealed: str) -> bytes:
    """Decrypt one sealed response frame; fails if altered, reordered or from another request."""
    try:
        return AESGCM(key).decrypt(_nonce(seq), L.unb64(sealed), _aad(rid, typ, seq))
    except Exception as e:  # noqa: BLE001
        raise ValueError("e2ee chunk authentication failed") from e


@dataclass
class E2EESealed:
    jwe: str            # pass as lmbp_e2ee
    rid: str
    response_key: bytes

    def open(self, frame: dict) -> bytes:
        return open_chunk(self.response_key, self.rid, frame["type"], frame["seq"], frame["data"])


def seal_e2ee_request(dev: Any, pinned_jkt: str, req: dict) -> E2EESealed:
    """Seal req ({model, messages, tools?, ...}) to dev's X25519 key (a DeviceInfo, verified against pinned_jkt)."""
    if not getattr(dev, "e2ee_key", ""):
        raise ValueError("device has no e2ee key")
    x, kid = verify_key_statement(dev.public_key, pinned_jkt, dev.device_id, dev.e2ee_key)
    key = os.urandom(32)
    try:
        if req.get("response_key") and len(L.unb64(req["response_key"])) == 32:
            key = L.unb64(req["response_key"])
    except ValueError:
        pass
    r = {**req, "rid": req.get("rid") or L.random_id("rid_"), "iat": req.get("iat") or L.now(), "response_key": L.b64(key)}
    return E2EESealed(seal_jwe(x, kid, json.dumps(r).encode()), r["rid"], key)


def decode_e2ee_delta(p: bytes) -> dict:
    """Parse an opened task.delta frame ({text?, tool_calls?, ...})."""
    return json.loads(p)


# ---- device side (desktop apps / test devices) ----

def x25519_key_pair() -> tuple[bytes, bytes]:
    k = X25519PrivateKey.generate()
    return k.private_bytes_raw(), k.public_key().public_bytes_raw()


def sign_key_statement(identity: L.SigningKey, device_id: str, xpub: bytes, lifetime: int = 30 * 86400) -> str:
    iat = L.now()
    return L.sign_jws(identity, TYP_E2EE_KEY, L.thumbprint(identity.pub), {"iss": device_id, "sub": device_id, "iat": iat, "exp": iat + lifetime,
                      "jwk": {"kty": "OKP", "crv": "X25519", "x": L.b64(xpub), "kid": L.x25519_thumbprint(xpub), "use": "enc", "alg": "ECDH-ES"}})


def open_jwe(priv: bytes, jwe: str) -> tuple[dict, bytes]:
    p = jwe.split(".")
    if len(p) != 5 or p[1] != "":
        raise ValueError("malformed JWE")
    h = json.loads(L.unb64(p[0]))
    if h.get("alg") != "ECDH-ES" or h.get("enc") != "A256GCM" or "zip" in h or "crit" in h:
        raise ValueError("unsupported JWE algorithm")
    epk = h.get("epk") or {}
    if epk.get("kty") != "OKP" or epk.get("crv") != "X25519":
        raise ValueError("bad epk")
    iv, ct, tag = L.unb64(p[2]), L.unb64(p[3]), L.unb64(p[4])
    if len(iv) != 12 or len(tag) != 16:
        raise ValueError("bad JWE encoding")
    z = X25519PrivateKey.from_private_bytes(priv).exchange(X25519PublicKey.from_public_bytes(L.unb64(epk["x"])))
    return h, AESGCM(_kdf(z)).decrypt(iv, ct + tag, p[0].encode("ascii"))


def seal_chunk(key: bytes, rid: str, typ: str, seq: int, plaintext: bytes) -> str:
    return L.b64(AESGCM(key).encrypt(_nonce(seq), plaintext, _aad(rid, typ, seq)))
