"""In-process client: calls the gateway's OpenAI / Anthropic surface without
an HTTP hop (requests run through the same engine code as HTTP requests)."""
from __future__ import annotations

import asyncio
import json
from typing import Any, AsyncIterator, Optional

from ._http import Request, Response


class LMBPAPIError(Exception):
    def __init__(self, status: int, code: str, message: str, body: Any = None):
        super().__init__(message)
        self.status, self.code, self.message, self.body = status, code, message, body


def _error(status: int, v: Any) -> LMBPAPIError:
    e = (v or {}).get("error") if isinstance(v, dict) else None
    e = e if isinstance(e, dict) else {}
    return LMBPAPIError(status, e.get("code") or e.get("lmbp_code") or "ERROR", e.get("message") or (v or {}).get("detail", "request failed") if isinstance(v, dict) else "request failed", v)


class _Completions:
    def __init__(self, c: "InProcessClient"):
        self._c = c

    async def create(self, *, idempotency_key: Optional[str] = None, **req: Any) -> dict:
        """Non-streaming chat completion (OpenAI shape)."""
        return await self._c._json("POST", "/v1/chat/completions", {**req, "stream": False}, idempotency_key)

    async def stream(self, *, idempotency_key: Optional[str] = None, **req: Any) -> AsyncIterator[dict]:
        """Streaming chat completion: async iterator of chunks."""
        async for _, data in self._c._sse("/v1/chat/completions", {**req, "stream": True}, idempotency_key):
            if data == "[DONE]":
                return
            v = json.loads(data)
            if "error" in v:
                raise LMBPAPIError(502, v["error"].get("code", "ERROR"), v["error"].get("message", ""), v)
            yield v


class _Chat:
    def __init__(self, c: "InProcessClient"):
        self.completions = _Completions(c)


class _Messages:
    def __init__(self, c: "InProcessClient"):
        self._c = c

    async def create(self, *, idempotency_key: Optional[str] = None, **req: Any) -> dict:
        """Anthropic Messages, non-streaming."""
        return await self._c._json("POST", "/v1/messages", {**req, "stream": False}, idempotency_key)

    async def stream(self, *, idempotency_key: Optional[str] = None, **req: Any) -> AsyncIterator[dict]:
        """Anthropic Messages, streaming: async iterator of named events (ping filtered)."""
        async for _, data in self._c._sse("/v1/messages", {**req, "stream": True}, idempotency_key):
            v = json.loads(data)
            if v.get("type") == "error":
                err = v.get("error") or {}
                raise LMBPAPIError(502, err.get("lmbp_code", "ERROR"), err.get("message", ""), v)
            if v.get("type") != "ping":
                yield v


class InProcessClient:
    def __init__(self, server: Any, tenant: str, end_user: str):
        self._server, self.tenant, self.end_user = server, tenant, end_user
        self.chat = _Chat(self)
        self.messages = _Messages(self)

    async def models(self) -> list[dict]:
        """GET /v1/models (OpenAI shape)."""
        return (await self._json("GET", "/v1/models", None, None))["data"]

    async def _request(self, method: str, path: str, body: Any, idem: Optional[str]):
        g = self._server.engine
        t = g.tenants.get(self.tenant)
        if t is None:
            raise LMBPAPIError(401, "UNAUTHENTICATED", "unknown tenant " + self.tenant)
        raw = b"" if body is None else json.dumps(body).encode()
        hs = [(b"host", b"in-process"), (b"content-type", b"application/json"), (b"x-end-user", self.end_user.encode())]
        if idem:
            hs.append((b"idempotency-key", idem.encode()))
        scope = {"type": "http", "method": method, "path": path, "headers": hs, "query_string": b"", "scheme": "http",
                 "lmbp.in_process_tenant": t}
        q: asyncio.Queue = asyncio.Queue()
        start: asyncio.Future = asyncio.get_running_loop().create_future()
        gone = asyncio.Event()

        async def receive():
            await gone.wait()
            return {"type": "http.disconnect"}

        async def send(m):
            if m["type"] == "http.response.start":
                if not start.done():
                    start.set_result((m["status"], {k.decode(): v.decode() for k, v in m["headers"]}))
            elif m["type"] == "http.response.body":
                if m.get("body"):
                    await q.put(m["body"])
                if not m.get("more_body"):
                    await q.put(None)
        req = Request(scope, receive, send, raw)

        async def run():
            resp = await g.route(req)
            if isinstance(resp, Response) and not req.started:
                await resp.send_to(req)
        task = asyncio.ensure_future(run())
        task.add_done_callback(lambda f: (not start.done() and start.set_exception(f.exception() or RuntimeError("no response")))
                               if not f.cancelled() else None)
        status, headers = await start

        async def chunks():
            try:
                while True:
                    c = await q.get()
                    if c is None:
                        return
                    yield c
            finally:
                if not task.done():
                    task.cancel()
        return status, headers, chunks()

    async def _json(self, method, path, body, idem):
        status, _, it = await self._request(method, path, body, idem)
        data = b"".join([c async for c in it])
        v = json.loads(data) if data else None
        if status >= 400:
            raise _error(status, v)
        return v

    async def _sse(self, path, body, idem):
        status, headers, it = await self._request("POST", path, body, idem)
        if status >= 400 or not headers.get("content-type", "").startswith("text/event-stream"):
            data = b"".join([c async for c in it])
            raise _error(status, json.loads(data) if data else None)
        buf = ""
        async for c in it:
            buf += c.decode()
            while "\n\n" in buf:
                block, buf = buf.split("\n\n", 1)
                ev, data = "", ""
                for line in block.split("\n"):
                    if line.startswith("event: "):
                        ev = line[7:]
                    elif line.startswith("data: "):
                        data += line[6:]
                if data:
                    yield ev, data
