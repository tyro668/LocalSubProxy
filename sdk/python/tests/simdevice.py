"""Minimal simulated desktop device for SDK tests (see sdk/node/test-support/simdevice.mjs)."""
from __future__ import annotations

import asyncio
import base64
import json
import secrets

import httpx
import websockets

from lsp2_server import protocol as L


def proof(key, iss, aud, cred):
    return L.sign_jws(key, L.TYP_PROOF, "", {"iss": iss, "aud": aud, "iat": L.now(), "jti": L.random_id("p_"), "ath": L.ath(cred)})


async def start_pairing(base, tenant, name="test-pc"):
    key = L.SigningKey(secrets.token_bytes(32))
    async with httpx.AsyncClient() as c:
        r = await c.post(base + "/v2/device/authorize", data={"tenant": tenant, "public_key": L.b64(key.pub), "device_name": name})
    return key, r.json()


async def finish_pairing(base, key, a):
    async with httpx.AsyncClient() as c:
        r = await c.post(base + "/v2/token", data={"grant_type": "urn:ietf:params:oauth:grant-type:device_code", "device_code": a["device_code"],
                                                   "proof": proof(key, "", L.PROOF_TOKEN, a["device_code"])})
    assert r.status_code == 200, r.text
    return r.json()


async def connect(base, key, tok, models=("m1",)):
    ws = await websockets.connect(base.replace("http", "ws", 1) + "/v2/connect", subprotocols=[L.SUBPROTOCOL],
                                  additional_headers={"Authorization": "Bearer " + tok["access_token"]})
    await ws.send(json.dumps({"v": 2, "type": "hello", "id": "h1", "data": {
        "device_id": tok["device_id"], "client": "sdk-test", "models": list(models), "model_capabilities": {m: ["tools"] for m in models},
        "max_concurrency": 4, "capabilities": ["tools"], "proof": proof(key, tok["device_id"], L.PROOF_HELLO, tok["access_token"])}}))
    w = json.loads(await ws.recv())
    assert w["type"] == "welcome", w

    async def loop():
        async for raw in ws:
            e = json.loads(raw)
            if e["type"] == "ping":
                await ws.send(json.dumps({"v": 2, "type": "pong", "id": e.get("id")}))
            if e["type"] != "task.run":
                continue
            p = e["jws"].split(".")[1]
            run = json.loads(base64.urlsafe_b64decode(p + "=" * (-len(p) % 4)))
            seq = 0

            async def send(typ, data):
                nonlocal seq
                seq += 1
                await ws.send(json.dumps({"v": 2, "type": typ, "task_id": run["task_id"], "seq": seq, "data": data}))
            last = run["messages"][-1]
            c = last.get("content")
            text = c if isinstance(c, str) else "".join(x.get("text", "") for x in (c or []) if x.get("type") == "text")
            if run.get("tools") and last["role"] == "user":
                await send("task.delta", {"tool_calls": [{"index": 0, "id": "call_1", "type": "function",
                                                         "function": {"name": run["tools"][0]["function"]["name"], "arguments": '{"city":'}}]})
                await send("task.delta", {"tool_calls": [{"index": 0, "function": {"arguments": '"Paris"}'}}]})
                await send("task.done", {"finish_reason": "tool_calls", "usage": {"prompt_tokens": 5, "completion_tokens": 2}})
                continue
            await send("task.delta", {"text": "echo: "})
            await send("task.delta", {"text": "tool said " + last["content"] if last["role"] == "tool" else text})
            await send("task.done", {"finish_reason": "stop", "usage": {"prompt_tokens": 3, "completion_tokens": 4}})
    task = asyncio.ensure_future(loop())
    return ws, task


async def pair_and_connect(lsp, base, tenant, user):
    key, a = await start_pairing(base, tenant)
    ok = lsp.pairing.approve(tenant, user, a["user_code"])
    tok = await finish_pairing(base, key, a)
    ws, task = await connect(base, key, tok)
    for _ in range(50):
        if any(d.online for d in lsp.pairing.list_devices(tenant, user)):
            break
        await asyncio.sleep(0.02)
    return ws, task, ok.device_id
