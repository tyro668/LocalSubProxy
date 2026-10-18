"""Minimal simulated desktop device for SDK tests (see sdk/node/test-support/simdevice.mjs)."""
from __future__ import annotations

import asyncio
import base64
import json
import secrets

import httpx
import websockets

from lmbp_server import e2ee as E
from lmbp_server import protocol as L


def proof(key, iss, aud, cred):
    return L.sign_jws(key, L.TYP_PROOF, "", {"iss": iss, "aud": aud, "iat": L.now(), "jti": L.random_id("p_"), "ath": L.ath(cred)})


async def start_pairing(base, tenant, name="test-pc"):
    key = L.SigningKey(secrets.token_bytes(32))
    async with httpx.AsyncClient() as c:
        r = await c.post(base + "/lmbp/v1/device/authorize", data={"tenant": tenant, "public_key": L.b64(key.pub), "device_name": name})
    return key, r.json()


async def finish_pairing(base, key, a):
    async with httpx.AsyncClient() as c:
        r = await c.post(base + "/lmbp/v1/token", data={"grant_type": "urn:ietf:params:oauth:grant-type:device_code", "device_code": a["device_code"],
                                                   "proof": proof(key, "", L.PROOF_TOKEN, a["device_code"])})
    assert r.status_code == 200, r.text
    return r.json()


async def connect(base, key, tok, models=("m1",), e2ee=False, max_concurrency=4, delay=0.0, on_run=None, flood=None, before_hello=None, manual=None, hello_patch=None, env_patch=None, on_welcome=None):
    x = E.x25519_key_pair() if e2ee else None
    ws = await websockets.connect(base.replace("http", "ws", 1) + "/lmbp/v1/connect", subprotocols=[L.SUBPROTOCOL],
                                  additional_headers={"Authorization": "Bearer " + tok["access_token"]})
    if before_hello is not None:  # e.g. revoke while the gateway awaits the hello
        await before_hello()
    hello = {
        "device_id": tok["device_id"], "client": "sdk-test", "models": list(models), "model_capabilities": {m: ["tools"] for m in models},
        "max_concurrency": max_concurrency, "capabilities": (["tools", "e2ee"] if x else ["tools"]) + (["flow"] if flood else []),
        **({"e2ee_key": E.sign_key_statement(key, tok["device_id"], x[1])} if x else {}), "proof": proof(key, tok["device_id"], L.PROOF_HELLO, tok["access_token"])}
    for k, v in (hello_patch or {}).items():
        if v is None:
            hello.pop(k, None)
        else:
            hello[k] = v
    await ws.send(json.dumps({"v": L.VERSION, "type": "hello", "id": "h1", "data": hello, **(env_patch or {})}))
    w = json.loads(await ws.recv())
    assert w["type"] == "welcome", w
    if on_welcome:
        on_welcome(w)  # e.g. keep the resume token
    # flood {n, size, stats}: answer every task with n deltas of size bytes,
    # honouring the gateway's flow window (§15.3); stats["sent"] counts bytes.
    fl = {"win": (w.get("data") or {}).get("flow", {}).get("task_window", float("inf")), "acked": 0, "frames": [],
          "wake": asyncio.Event()}

    async def do_flood(run):
        n, size, stats = flood["n"], flood["size"], flood["stats"]

        def unacked():
            return sum(ln for s, ln in fl["frames"] if s > fl["acked"])
        for i in range(n + 1):
            last = i == n
            m = json.dumps({"v": L.VERSION, "type": "task.done" if last else "task.delta", "task_id": run["task_id"], "seq": i + 1,
                            "data": {"finish_reason": "stop", "usage": {"prompt_tokens": 1, "completion_tokens": n}} if last else {"text": "x" * size}})
            while unacked() > 0 and unacked() + len(m) > fl["win"]:
                fl["wake"].clear()
                try:
                    await asyncio.wait_for(fl["wake"].wait(), 0.05)
                except asyncio.TimeoutError:
                    pass
            fl["frames"].append((i + 1, len(m)))
            await ws.send(m)
            stats["sent"] += len(m)

    pending = {}  # task_id -> (run, chunks) while an E2EE blob arrives

    async def run_e2ee(run, jwe):
        _, pt = E.open_jwe(x[0], jwe)
        req = json.loads(pt)
        k = L.unb64(req["response_key"])
        text = "echo:" + req["messages"][-1]["content"]
        for seq, (typ, v, extra) in enumerate([("task.delta", {"text": text}, {}),
                                               ("task.done", {"finish_reason": "stop"}, {"finish_reason": "stop", "usage": {"prompt_tokens": 1, "completion_tokens": 1}})], 1):
            await ws.send(json.dumps({"v": L.VERSION, "type": typ, "task_id": run["task_id"], "seq": seq,
                                      "data": {**extra, "e2ee": E.seal_chunk(k, req["rid"], typ, seq, json.dumps(v).encode())}}))

    listeners = []  # manual mode: per-task handlers fed every envelope

    async def loop():
        async for raw in ws:
            e = json.loads(raw)
            for fn in list(listeners):
                fn(e)
            if e["type"] == "ping":
                await ws.send(json.dumps({"v": L.VERSION, "type": "pong", "id": e.get("id")}))
            if e["type"] == "blob.chunk" and e.get("task_id") in pending:
                run, chunks = pending[e["task_id"]]
                chunks.append(L.unb64(e["data"]["data"]))
                if e["data"].get("last"):
                    del pending[e["task_id"]]
                    await run_e2ee(run, b"".join(chunks).decode())
                continue
            if flood and e["type"] == "task.cancel":
                flood["stats"]["cancelled"] = True
                continue
            if flood and e["type"] == "task.ack":
                fl["acked"] = max(fl["acked"], e["data"]["seq"])
                fl["wake"].set()
                continue
            if e["type"] != "task.run":
                continue
            p = e["jws"].split(".")[1]
            run = json.loads(base64.urlsafe_b64decode(p + "=" * (-len(p) % 4)))
            if on_run:
                on_run(run)  # tests inspect the signed task.run payload
            if manual:
                manual(run, ws, listeners)
                continue
            if run.get("e2ee"):
                pending[run["task_id"]] = (run, [])
                continue
            if flood:
                asyncio.ensure_future(do_flood(run))
                continue
            seq = 0

            async def send(typ, data):
                nonlocal seq
                if delay:  # hold the task (and its concurrency slot) before answering
                    await asyncio.sleep(delay)
                seq += 1
                await ws.send(json.dumps({"v": L.VERSION, "type": typ, "task_id": run["task_id"], "seq": seq, "data": data}))
            last = run["messages"][-1]
            c = last.get("content")
            text = c if isinstance(c, str) else "".join(x.get("text", "") for x in (c if isinstance(c, list) else []) if isinstance(x, dict) and x.get("type") == "text")
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


async def pair_and_connect(lsp, base, tenant, user, e2ee=False, **kw):
    key, a = await start_pairing(base, tenant)
    ok = lsp.pairing.approve(tenant, user, a["user_code"])
    tok = await finish_pairing(base, key, a)
    ws, task = await connect(base, key, tok, e2ee=e2ee, **kw)
    for _ in range(50):
        if any(d.online for d in lsp.pairing.list_devices(tenant, user)):
            break
        await asyncio.sleep(0.02)
    return ws, task, ok.device_id
