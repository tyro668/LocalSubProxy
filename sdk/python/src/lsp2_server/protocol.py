"""LSP2 primitives: base64url, EdDSA JWS/JWT, proofs, thumbprints, origins and
the reliable flow-controlled delivery state machines (protocol-v2 §8, §15)."""
from __future__ import annotations

import asyncio
import base64
import hashlib
import json
import re
import secrets
import time
from urllib.parse import urlsplit

from cryptography.exceptions import InvalidSignature
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey, Ed25519PublicKey
from cryptography.hazmat.primitives.serialization import Encoding, PublicFormat

VERSION = 2
SUBPROTOCOL = "lsp2"
CAPABILITIES = ["session.token", "models.update", "resume", "flow", "tools", "multimodal", "e2ee"]
SCOPES = ["lsp2.chat", "lsp2.tools", "lsp2.vision", "lsp2.audio"]
TENANT_ID_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$")
MAX_FRAME_BYTES = 256 << 10
BLOB_CHUNK_BYTES = 96 << 10
MAX_BLOB_BYTES = 20 << 20
MAX_TASK_BLOB_BYTES = 32 << 20
INLINE_DATA_URI_BYTES = 16 << 10
BLOB_SCHEME = "lsp2-blob:"
TYP_ACCESS, TYP_ASSERTION, TYP_PROOF = "at+jwt", "JWT", "lsp2-proof+jwt"
TYP_TASK, TYP_METADATA, TYP_E2EE_KEY = "lsp2-task+jws", "lsp2-metadata+jwt", "lsp2-e2ee-key+jwt"
PROOF_TOKEN, PROOF_HELLO, PROOF_SESSION_TOKEN = "lsp2:token", "lsp2:hello", "lsp2:session.token"

_B64URL_RE = re.compile(r"^[A-Za-z0-9_-]*$")


def dumps(v) -> str:
    return json.dumps(v, separators=(",", ":"), ensure_ascii=False)


def b64(b: bytes) -> str:
    return base64.urlsafe_b64encode(b).rstrip(b"=").decode()


def unb64(s: str) -> bytes:
    if not isinstance(s, str) or not _B64URL_RE.match(s) or len(s) % 4 == 1:
        raise ValueError("bad base64url")
    return base64.urlsafe_b64decode(s + "=" * (-len(s) % 4))


def sha256(b: bytes) -> bytes:
    return hashlib.sha256(b).digest()


def sha_hex(s: str) -> str:
    return hashlib.sha256(s.encode()).hexdigest()


def random_id(prefix: str = "") -> str:
    return prefix + secrets.token_hex(16)


def now() -> int:
    return int(time.time())


def is_int(v) -> bool:
    return isinstance(v, int) and not isinstance(v, bool)


def ath(cred: str) -> str:
    return b64(sha256(cred.encode()))


def thumbprint(pub: bytes) -> str:
    return b64(sha256(('{"crv":"Ed25519","kty":"OKP","x":"%s"}' % b64(pub)).encode()))


def x25519_thumbprint(pub: bytes) -> str:
    return b64(sha256(('{"crv":"X25519","kty":"OKP","x":"%s"}' % b64(pub)).encode()))


def public_jwk(pub: bytes) -> dict:
    return {"kty": "OKP", "crv": "Ed25519", "x": b64(pub), "kid": thumbprint(pub), "alg": "EdDSA", "use": "sig"}


def token_audience(iss: str, tenant: str) -> str:
    return f"{iss}/tenants/{tenant}"


def assertion_audience(iss: str) -> str:
    return f"{iss}/v2/token"


class SigningKey:
    def __init__(self, seed: bytes):
        self.priv = Ed25519PrivateKey.from_private_bytes(seed)
        self.pub = self.priv.public_key().public_bytes(Encoding.Raw, PublicFormat.Raw)
        self.kid = thumbprint(self.pub)


def sign_jws(key: SigningKey, typ: str, kid: str, payload) -> str:
    h = {"alg": "EdDSA", "typ": typ}
    if kid:
        h["kid"] = kid
    body = payload if isinstance(payload, (bytes, str)) else dumps(payload)
    if isinstance(body, str):
        body = body.encode()
    inp = b64(dumps(h).encode()) + "." + b64(body)
    return inp + "." + b64(key.priv.sign(inp.encode()))


def verify_jws(pub: bytes, jws: str, typ: str) -> bytes:
    if not isinstance(jws, str):
        raise ValueError("malformed JWS")
    parts = jws.split(".")
    if len(parts) != 3:
        raise ValueError("malformed JWS")
    h = json.loads(unb64(parts[0]))
    if not isinstance(h, dict) or h.get("alg") != "EdDSA" or h.get("typ") != typ:
        raise ValueError("bad JWS header")
    if "crit" in h:
        raise ValueError("unsupported crit header")
    sig = unb64(parts[2])
    if len(pub) != 32 or len(sig) != 64:
        raise ValueError("bad JWS signature")
    try:
        Ed25519PublicKey.from_public_bytes(pub).verify(sig, (parts[0] + "." + parts[1]).encode())
    except InvalidSignature:
        raise ValueError("bad JWS signature") from None
    return unb64(parts[1])


def verify_jwt(pub: bytes, tok: str, typ: str, aud: str, t: int | None = None) -> dict:
    t = now() if t is None else t
    c = json.loads(verify_jws(pub, tok, typ))
    if not isinstance(c, dict):
        raise ValueError("bad token payload")
    if c.get("aud") != aud:
        raise ValueError("wrong audience")
    if not (is_int(c.get("exp")) and is_int(c.get("iat"))) or t > c["exp"] + 30 or t + 30 < c["iat"]:
        raise ValueError("token expired")
    return c


def unverified_claims(tok) -> dict:
    parts = str(tok or "").split(".")
    if len(parts) != 3:
        raise ValueError("malformed token")
    c = json.loads(unb64(parts[1]))
    if not isinstance(c, dict):
        raise ValueError("bad claims")
    return c


def verify_proof(pub: bytes, proof, device_id: str, aud: str, credential: str) -> dict:
    c = json.loads(verify_jws(pub, proof, TYP_PROOF))
    if (not isinstance(c, dict) or c.get("aud") != aud or c.get("iss", "") != device_id or not c.get("jti")
            or c.get("ath") != ath(credential)):
        raise ValueError("proof claims invalid")
    if not is_int(c.get("iat")) or abs(now() - c["iat"]) > 60:
        raise ValueError("proof iat outside allowed skew")
    return c


def verify_e2ee_key(device_pub: bytes, device_id: str, stmt: str) -> None:
    c = json.loads(verify_jws(device_pub, stmt, TYP_E2EE_KEY))
    k = c.get("jwk") if isinstance(c, dict) else None
    if c.get("sub") != device_id or c.get("iss") != device_id or not isinstance(k, dict) or k.get("kty") != "OKP" or k.get("crv") != "X25519":
        raise ValueError("bad e2ee key statement")
    if now() > c.get("exp", 0) or now() + 60 < c.get("iat", 0):
        raise ValueError("e2ee key statement expired")
    x = unb64(k.get("x", ""))
    if len(x) != 32 or k.get("kid") != x25519_thumbprint(x):
        raise ValueError("bad e2ee key")


def is_loopback_host(h: str) -> bool:
    h = h.strip("[]").lower()
    return h in ("localhost", "::1") or re.match(r"^127\.\d+\.\d+\.\d+$", h) is not None


def origin(raw: str) -> str:
    """Normalize scheme://host[:port]; http only for loopback hosts (§18.1)."""
    s = str(raw).strip().rstrip("/")
    u = urlsplit(s)
    if not u.netloc or "@" in u.netloc or u.query or u.fragment or u.path not in ("", "/"):
        raise ValueError(f"not an origin: {raw!r}")
    scheme = u.scheme.lower()
    host = (u.hostname or "").lower()
    if scheme == "http" and not is_loopback_host(host):
        raise ValueError(f"issuer {raw!r} must use https")
    if scheme not in ("http", "https"):
        raise ValueError(f"unsupported scheme in {raw!r}")
    if ":" in host:
        host = f"[{host}]"
    port = u.port
    if port and not ((scheme == "https" and port == 443) or (scheme == "http" and port == 80)):
        host += f":{port}"
    return f"{scheme}://{host}"


def can_send(flow, task_used: int, conn_used: int, n: int) -> bool:
    if not flow:
        return True
    return ((task_used + n <= flow["task_window"] or task_used == 0)
            and (conn_used + n <= flow["conn_window"] or conn_used == 0))


class OutboxClosed(Exception):
    pass


class Outbox:
    """Sending half: send() completes once the frame fits the peer's windows;
    unacknowledged frames are retransmitted on resume. write() must be
    synchronous and order-preserving (it enqueues onto the socket writer)."""

    def __init__(self):
        self.flow = None
        self.conn_used = 0
        self.tasks: dict[str, dict] = {}
        self.waiters: list[dict] = []
        self.write = None
        self.closed = False

    def set_flow(self, f):
        self.flow = f
        self._wake()

    def attach(self, write):
        self.write = write
        for t in self.tasks.values():
            t["written"] = -1

    def _task(self, tid):
        t = self.tasks.get(tid)
        if t is None:
            t = self.tasks[tid] = {"next": 0, "unacked": [], "used": 0, "written": 0}
        return t

    def _fits(self, tid, n):
        return can_send(self.flow, self._task(tid)["used"], self.conn_used, n)

    def _wake(self):
        while self.waiters:
            w = self.waiters[0]
            if not self.closed and not self._fits(w["task"], w["n"]):
                return
            self.waiters.pop(0)
            if not w["fut"].done():
                w["fut"].set_result(None)

    def _flush(self, tid):
        while True:
            t = self.tasks.get(tid)
            if t is None or self.write is None or t["written"] < 0:
                return
            f = next((f for f in t["unacked"] if f[0] > t["written"]), None)
            if f is None:
                return
            t["written"] = f[0]
            try:
                self.write(f[1])
            except Exception:
                return

    async def send(self, tid: str, build) -> int:
        def mk(seq):
            e = {"v": VERSION, **build(seq), "task_id": tid, "seq": seq}
            return dumps(e)
        if self.closed:
            raise OutboxClosed()
        t = self._task(tid)
        raw = mk(t["next"] + 1)
        if self.waiters or not self._fits(tid, len(raw.encode())):
            w = {"task": tid, "n": len(raw.encode()), "fut": asyncio.get_running_loop().create_future()}
            self.waiters.append(w)
            try:
                await w["fut"]
            except asyncio.CancelledError:
                if w in self.waiters:
                    self.waiters.remove(w)
                    self._wake()
                raise
            if self.closed:
                raise OutboxClosed()
            t = self._task(tid)
        seq = t["next"] + 1
        raw = mk(seq)
        n = len(raw.encode())
        t["next"] = seq
        t["unacked"].append((seq, raw, n))
        t["used"] += n
        self.conn_used += n
        self._flush(tid)
        return seq

    def ack(self, tid, seq):
        t = self.tasks.get(tid)
        if t is None:
            return
        while t["unacked"] and t["unacked"][0][0] <= seq:
            _, _, n = t["unacked"].pop(0)
            t["used"] -= n
            self.conn_used -= n
        self._wake()

    def retransmit(self, tid, peer_recv):
        self.ack(tid, peer_recv)
        t = self.tasks.get(tid)
        if t is not None:
            t["written"] = peer_recv
        self._flush(tid)

    def forget(self, tid):
        t = self.tasks.pop(tid, None)
        if t is not None:
            self.conn_used -= t["used"]
        self._wake()

    def close(self):
        self.closed = True
        self._wake()


ACCEPTED, DUPLICATE, GAP, OVERFLOW = range(4)


class Inbound:
    """Receiving half: detects duplicates/gaps and enforces our windows."""

    def __init__(self, flow):
        self.flow = flow
        self.conn_used = 0
        self.tasks: dict[str, dict] = {}

    def accept(self, tid, seq, size):
        t = self.tasks.setdefault(tid, {"recv": 0, "sizes": [], "used": 0})
        if seq <= t["recv"]:
            return DUPLICATE
        if seq != t["recv"] + 1:
            return GAP
        f = self.flow
        if f and ((t["used"] + size > f["task_window"] and t["used"] > 0) or (self.conn_used + size > f["conn_window"] and self.conn_used > 0)):
            return OVERFLOW
        t["recv"] = seq
        t["sizes"].append((seq, size))
        t["used"] += size
        self.conn_used += size
        return ACCEPTED

    def acked(self, tid, seq):
        t = self.tasks.get(tid)
        if t is None:
            return
        while t["sizes"] and t["sizes"][0][0] <= seq:
            _, n = t["sizes"].pop(0)
            t["used"] -= n
            self.conn_used -= n

    def recv(self, tid):
        t = self.tasks.get(tid)
        return t["recv"] if t else 0

    def reset_windows(self):
        self.conn_used = 0
        for t in self.tasks.values():
            t["used"], t["sizes"] = 0, []

    def forget(self, tid):
        t = self.tasks.pop(tid, None)
        if t is not None:
            self.conn_used -= t["used"]
