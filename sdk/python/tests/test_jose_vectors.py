"""Audit finding 4: shared strict-JOSE vectors (spec/testvectors/jose.json).
The SDK gateway verifies proofs, client assertions, access tokens and e2ee key
statements; task_run/metadata are device-side and not implemented here."""
import json
from pathlib import Path

from lmbp_server import protocol as L

F = json.loads((Path(__file__).resolve().parents[3] / "spec/testvectors/jose.json").read_text())


def pub(k):
    return L.unb64(F["keys"][k]["public_b64url"])


KINDS = {
    "proof": lambda v: L.verify_proof(pub(v["verify_with_key"]), v["compact"], v["context"]["iss"], v["context"]["aud"], v["context"]["credential"], t=F["now"]),
    "client_assertion": lambda v: L.verify_jwt(pub(v["verify_with_key"]), v["compact"], L.TYP_ASSERTION, v["context"]["aud"], F["now"]),
    "access_token": lambda v: L.verify_jwt(pub(v["verify_with_key"]), v["compact"], L.TYP_ACCESS, v["context"]["aud"], F["now"]),
    "e2ee_key": lambda v: L.verify_e2ee_key(pub(v["verify_with_key"]), v["context"]["device_id"], v["compact"], t=F["now"]),
}


def test_jose_vectors():
    bad, n = [], 0
    for v in F["valid"]:
        if v["kind"] in KINDS:
            try:
                KINDS[v["kind"]](v)
            except Exception as e:  # noqa: BLE001
                bad.append(f"valid {v['name']} rejected: {e}")
    for v in F["negative"]:
        if v["kind"] in KINDS:
            n += 1
            try:
                KINDS[v["kind"]](v)
                bad.append(f"negative {v['name']} accepted ({v['must_reject_because']})")
            except Exception:  # noqa: BLE001
                pass
    print(f"checked {n} negative vectors; {len(bad)} wrong")
    assert bad == []
