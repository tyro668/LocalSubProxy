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
