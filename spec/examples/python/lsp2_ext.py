"""lsp2 extensions for a Python implementation: E2EE (JWE ECDH-ES/X25519/A256GCM,
sealed result frames, key statements) and flow control / resume bookkeeping.

Libraries: `cryptography` (X25519, AES-GCM, Ed25519). Equivalent choices:
python-jose/joserfc/jwcrypto can open the JWE too (alg ECDH-ES, enc A256GCM,
crv X25519) — but pin alg/enc and reject `zip` yourself.
Executed against spec/testvectors/vectors.json by test_vectors.py.
"""
from __future__ import annotations

import hashlib
import json
import struct

from cryptography.hazmat.primitives.asymmetric.x25519 import X25519PrivateKey, X25519PublicKey
from cryptography.hazmat.primitives.ciphers.aead import AESGCM

from lsp2 import Lsp2Error, b64u, json_bytes, thumbprint, unb64u, verify_jws

TYP_E2EE_KEY = "lsp2-e2ee-key+jwt"
CTY_REQUEST = "lsp2-request+json"
AAD_PREFIX = "lsp2-e2ee/v1|"


def x25519_thumbprint(pub: bytes) -> str:
    return b64u(hashlib.sha256(('{"crv":"X25519","kty":"OKP","x":"%s"}' % b64u(pub)).encode()).digest())


def verify_key_statement(device_pub: bytes, pinned_jkt: str, device_id: str, stmt: str, now: int) -> tuple[bytes, str]:
    """Returns (x25519 public key, kid). pinned_jkt is the device key thumbprint captured at pairing."""
    if thumbprint(device_pub) != pinned_jkt:
        raise Lsp2Error("device key does not match pinned thumbprint")
    c = json.loads(verify_jws(device_pub, stmt, TYP_E2EE_KEY))
    jwk = c.get("jwk", {})
    if c.get("iss") != device_id or c.get("sub") != device_id or jwk.get("kty") != "OKP" or jwk.get("crv") != "X25519":
        raise Lsp2Error("bad key statement")
    if now > c["exp"] or now + 60 < c["iat"]:
        raise Lsp2Error("key statement expired")
    x = unb64u(jwk["x"])
    if len(x) != 32 or jwk.get("kid") != x25519_thumbprint(x):
        raise Lsp2Error("bad e2ee key")
    return x, jwk["kid"]


def concat_kdf(z: bytes, enc: str = "A256GCM") -> bytes:
    """RFC 7518 §4.6.2 Concat KDF (SHA-256, one round) with empty apu/apv, keydatalen 256."""
    data = struct.pack(">I", 1) + z + struct.pack(">I", len(enc)) + enc.encode() + struct.pack(">I", 0) * 2 + struct.pack(">I", 256)
    return hashlib.sha256(data).digest()


def seal_jwe(recipient: bytes, kid: str, plaintext: bytes, eph_priv: bytes | None = None, iv: bytes | None = None) -> str:
    import os
    eph = X25519PrivateKey.from_private_bytes(eph_priv) if eph_priv else X25519PrivateKey.generate()
    iv = iv or os.urandom(12)
    z = eph.exchange(X25519PublicKey.from_public_bytes(recipient))
    hdr = {"alg": "ECDH-ES", "enc": "A256GCM", "epk": {"kty": "OKP", "crv": "X25519", "x": b64u(eph.public_key().public_bytes_raw())}, "kid": kid, "cty": CTY_REQUEST}
    prot = b64u(json_bytes(hdr))
    ct = AESGCM(concat_kdf(z)).encrypt(iv, plaintext, prot.encode("ascii"))
    return f"{prot}..{b64u(iv)}.{b64u(ct[:-16])}.{b64u(ct[-16:])}"


def open_jwe(priv: bytes, jwe: str) -> tuple[dict, bytes]:
    p = jwe.split(".")
    if len(p) != 5 or p[1] != "":
        raise Lsp2Error("malformed JWE (encrypted key must be empty for ECDH-ES)")
    h = json.loads(unb64u(p[0]))
    if h.get("alg") != "ECDH-ES" or h.get("enc") != "A256GCM" or "zip" in h or "crit" in h:
        raise Lsp2Error("unsupported JWE algorithm")
    epk = h.get("epk", {})
    if epk.get("kty") != "OKP" or epk.get("crv") != "X25519":
        raise Lsp2Error("bad epk")
    iv, ct, tag = unb64u(p[2]), unb64u(p[3]), unb64u(p[4])
    if len(iv) != 12 or len(tag) != 16:
        raise Lsp2Error("bad JWE encoding")
    z = X25519PrivateKey.from_private_bytes(priv).exchange(X25519PublicKey.from_public_bytes(unb64u(epk["x"])))
    try:
        return h, AESGCM(concat_kdf(z)).decrypt(iv, ct + tag, p[0].encode("ascii"))
    except Exception as e:  # noqa: BLE001
        raise Lsp2Error("JWE decryption failed") from e


def chunk_nonce(seq: int) -> bytes:
    return b"\x00" * 4 + struct.pack(">Q", seq)


def chunk_aad(rid: str, typ: str, seq: int) -> bytes:
    return f"{AAD_PREFIX}{rid}|{typ}|{seq}".encode()


def seal_chunk(key: bytes, rid: str, typ: str, seq: int, plaintext: bytes) -> str:
    return b64u(AESGCM(key).encrypt(chunk_nonce(seq), plaintext, chunk_aad(rid, typ, seq)))


def open_chunk(key: bytes, rid: str, typ: str, seq: int, sealed: str) -> bytes:
    try:
        return AESGCM(key).decrypt(chunk_nonce(seq), unb64u(sealed), chunk_aad(rid, typ, seq))
    except Exception as e:  # noqa: BLE001
        raise Lsp2Error("chunk authentication failed") from e


# ---- flow control (§15) ----

def can_send(task_window: int, conn_window: int, task_used: int, conn_used: int, n: int) -> bool:
    """Normative sender rule; a frame always fits an empty window."""
    return (task_used + n <= task_window or task_used == 0) and (conn_used + n <= conn_window or conn_used == 0)


class Inbound:
    """Receiver bookkeeping: duplicate/gap detection and window enforcement."""

    def __init__(self, task_window: int, conn_window: int):
        self.tw, self.cw, self.conn_used, self.tasks = task_window, conn_window, 0, {}

    def accept(self, task: str, seq: int, size: int) -> str:
        t = self.tasks.setdefault(task, {"recv": 0, "sizes": [], "used": 0})
        if seq <= t["recv"]:
            return "duplicate"
        if seq != t["recv"] + 1:
            return "gap"
        if (t["used"] + size > self.tw and t["used"] > 0) or (self.conn_used + size > self.cw and self.conn_used > 0):
            return "overflow"
        t["recv"] = seq
        t["sizes"].append((seq, size))
        t["used"] += size
        self.conn_used += size
        return "accepted"

    def acked(self, task: str, seq: int) -> None:
        t = self.tasks.get(task)
        while t and t["sizes"] and t["sizes"][0][0] <= seq:
            _, n = t["sizes"].pop(0)
            t["used"] -= n
            self.conn_used -= n


def retransmit(unacked: list[int], peer_recv: int) -> list[int]:
    """Frames to resend after resume: everything unacknowledged above the peer's recv_seq, in order."""
    return [s for s in unacked if s > peer_recv]
