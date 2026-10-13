"""Regression tests for the 2026-10 audit findings (see reports/LMBP审查问题修复报告.md)."""
import httpx

from lmbp_server import LMBPServer, MemoryStore, SQLiteStore, TenantConfig
from simdevice import pair_and_connect

KEY = "k" * 40
T = "acme"


def new(**kw):
    return LMBPServer([TenantConfig(T, KEY, {"display_name": "Acme"})], log=lambda *a: None, **kw)


def test_idempotency_store_keys_unambiguous(tmp_path):
    # Finding 1: (user a, key b|c) must not collide with (user a|b, key c).
    for st in (MemoryStore(), SQLiteStore(str(tmp_path / "i.db"))):
        st.put_idempotency(T, "a", "b|c", {"tenant": T, "owner": "a", "task_id": "task_a", "body_hash": "h"})
        assert st.get_idempotency(T, "a|b", "c") is None, type(st).__name__
        assert st.get_idempotency(T, "a", "b|c")["task_id"] == "task_a"
        st.put_idempotency(T, "a", "b|c", {"tenant": T, "owner": "a", "task_id": "task_other", "body_hash": "h"})
        assert st.get_idempotency(T, "a", "b|c")["task_id"] == "task_a"


async def test_idempotent_replay_never_crosses_users(serve_app):
    lsp = new()
    async with serve_app(lsp.asgi()) as base:
        ws, task, _ = await pair_and_connect(lsp, base, T, "a")
        body = {"model": "m1", "messages": [{"role": "user", "content": "secret-of-a"}]}
        async with httpx.AsyncClient(base_url=base, headers={"Authorization": "Bearer " + KEY}) as h:
            r1 = await h.post("/v1/chat/completions", json=body, headers={"X-End-User": "a", "Idempotency-Key": "b|c"})
            assert r1.status_code == 200 and "secret-of-a" in r1.text
            r2 = await h.post("/v1/chat/completions", json=body, headers={"X-End-User": "a|b", "Idempotency-Key": "c"})
            assert r2.status_code != 200 and "secret-of-a" not in r2.text, r2.text
            r3 = await h.post("/v1/chat/completions", json=body, headers={"X-End-User": "a", "Idempotency-Key": "b|c"})
            assert r3.status_code == 200 and r3.headers["x-task-id"] == r1.headers["x-task-id"]
        await ws.close()
        task.cancel()


async def test_in_process_pairing_rate_limited_shared_with_http(serve_app):
    # Finding 10: the lockout lives in the shared engine (in-process + HTTP).
    import pytest
    from lmbp_server import PairingError
    lsp = new()
    codes = []
    for i in range(12):
        try:
            lsp.pairing.approve(T, "u1", f"WRONG-{i:04d}")
            codes.append("ok")
        except PairingError as e:
            codes.append(e.code)
    assert codes[10:] == ["RATE_LIMITED", "RATE_LIMITED"], codes
    with pytest.raises(PairingError) as ei:
        lsp.pairing.deny(T, "u1", "WRONG-0000")
    assert ei.value.code == "RATE_LIMITED"
    async with serve_app(lsp.asgi()) as base:
        async with httpx.AsyncClient(base_url=base, headers={"Authorization": "Bearer " + KEY, "X-End-User": "u1"}) as h:
            r = await h.post("/lmbp/v1/tenant/device-approvals", json={"user_code": "WRONG-0000"})
            assert r.status_code == 429, r.text


async def test_client_cursor_resume_idempotency(serve_app):
    # Finding 7: SSE cursor + resume + idempotent replay from the in-process client.
    from lmbp_server import LMBPAPIError
    lsp = new()
    async with serve_app(lsp.asgi()) as base:
        ws, task, _ = await pair_and_connect(lsp, base, T, "u1")
        c = lsp.client(T, "u1")
        req = {"model": "m1", "messages": [{"role": "user", "content": "hello"}]}
        st = c.chat.completions.stream(idempotency_key="k-1", **req)
        ids, parts = [], []
        async for ch in st:
            ids.append(st.last_event_id)
            parts.append("".join(x["delta"].get("content") or "" for x in ch["choices"]))
        text = "".join(parts)
        assert text == "echo: hello" and st.task_id and len(ids) >= 3 and ids[0] == "0", (ids, st.task_id)
        rest = ""
        async for ch in c.chat.completions.resume(st.task_id, ids[1]):
            rest += "".join(x["delta"].get("content") or "" for x in ch["choices"])
        assert rest == "".join(parts[2:]) and rest != text
        again = c.chat.completions.stream(idempotency_key="k-1", **req)
        replay = ""
        async for ch in again:
            replay += "".join(x["delta"].get("content") or "" for x in ch["choices"])
        assert replay == text and again.task_id == st.task_id
        try:
            await c.chat.completions.create(idempotency_key="k-1", **req)
            raise AssertionError("key reuse with another body accepted")
        except LMBPAPIError as e:
            assert e.code == "IDEMPOTENCY_KEY_REUSED"
        async for _ in c.chat.completions.resume(st.task_id, "999"):
            raise AssertionError("events after the terminal one")
        await ws.close()
        task.cancel()


async def test_client_e2ee_round_trip(serve_app):
    # Finding 7: E2EE from business code (seal to the pinned device, open sealed frames).
    import pytest
    from lmbp_server import decode_e2ee_delta, seal_e2ee_request
    lsp = new()
    async with serve_app(lsp.asgi()) as base:
        ws, task, _ = await pair_and_connect(lsp, base, T, "u1", e2ee=True)
        dev = lsp.pairing.list_devices(T, "u1")[0]
        assert dev.e2ee_key and len(dev.public_key) == 32
        with pytest.raises(ValueError):
            seal_e2ee_request(dev, "not-the-pinned-jkt", {"model": "m1", "messages": []})
        sealed = seal_e2ee_request(dev, dev.jkt, {"model": "m1", "messages": [{"role": "user", "content": "secret"}]})
        assert "secret" not in sealed.jwe
        text, frames = "", 0
        async for ch in lsp.client(T, "u1").chat.completions.stream(model="m1", lmbp_e2ee=sealed.jwe):
            f = ch.get("lmbp_e2ee")
            if not f:
                continue
            frames += 1
            pt = sealed.open(f)
            if f["type"] == "task.delta":
                text += decode_e2ee_delta(pt).get("text", "")
            with pytest.raises(ValueError):
                sealed.open({**f, "seq": f["seq"] + 1})
        assert text == "echo:secret" and frames >= 2
        await ws.close()
        task.cancel()


import pytest as _pytest


@_pytest.mark.parametrize("mode", ["event", "state"])
async def test_no_ack_without_persistence(serve_app, mode):
    # Reliability 1: never ACK a device frame (or the terminal frame) that was
    # not persisted; the caller gets INTERNAL instead of an unpersisted result.
    import asyncio
    st = MemoryStore()
    logs = []
    append, put_task = st.append_event, st.put_task
    if mode == "event":
        st.append_event = lambda id, ev: (_ for _ in ()).throw(OSError("disk full")) if ev["seq"] == 2 and ev["type"] != "task.error" else append(id, ev)
    else:
        st.put_task = lambda rec: (_ for _ in ()).throw(OSError("disk full")) if rec.get("state") in ("SUCCEEDED", "FAILED", "CANCELLED") else put_task(rec)
    lsp = LMBPServer([TenantConfig(T, KEY, {"display_name": "Acme"})], store=st, log=lambda *a: logs.append(" ".join(map(str, a))))
    async with serve_app(lsp.asgi()) as base:
        ws, task, _ = await pair_and_connect(lsp, base, T, "u1")
        async with httpx.AsyncClient(base_url=base, headers={"Authorization": "Bearer " + KEY, "X-End-User": "u1"}) as h:
            r = await h.post("/v1/chat/completions", json={"model": "m1", "messages": [{"role": "user", "content": "hello"}]})
        assert r.status_code != 200 and "INTERNAL" in r.text, r.text
        await asyncio.sleep(0.1)
        t = lsp.engine.tasks.get(r.headers["x-task-id"])
        assert t is not None and t.acked < (2 if mode == "event" else 3), t and t.acked
        assert "disk full" in "\n".join(logs)
        await ws.close()
        task.cancel()
