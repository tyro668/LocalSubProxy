"""In-process client: calls the gateway's OpenAI / Anthropic surface without
an HTTP hop (requests run through the same engine code as HTTP requests)."""
from __future__ import annotations

import asyncio
import json
from urllib.parse import quote
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


class IncompleteStreamError(Exception):
    """Raised by a stream that ended (EOF) before its terminator: ``data: [DONE]``
    for chat completions, ``message_stop`` for Messages, or an error event. The
    response is NOT complete; resume from ``last_event_id`` with
    ``chat.completions.resume(task_id, last_event_id)``, or repeat the request
    with the same ``idempotency_key`` and ``last_event_id``."""
    code = "INCOMPLETE_STREAM"

    def __init__(self, task_id: str, last_event_id: str):
        super().__init__(f"lmbp: stream ended before its terminator (task {task_id!r}, last event id {last_event_id!r})")
        self.task_id, self.last_event_id = task_id, last_event_id


class LMBPStream:
    """A streaming response: ``async for`` it. ``last_event_id`` is the SSE id of
    the last event yielded (the resume cursor) and ``task_id`` the gateway task.
    Resume with the same request + idempotency_key + last_event_id, or with
    ``chat.completions.resume(task_id, last_event_id)``."""

    def __init__(self, make):
        self.last_event_id = ""
        self.task_id = ""
        self._make, self._it = make, None

    def __aiter__(self):
        return self

    async def __anext__(self):
        if self._it is None:
            self._it = self._make(self)
        return await self._it.__anext__()

    async def aclose(self):
        if self._it is not None:
            await self._it.aclose()


class _aclosing:
    """contextlib.aclosing (3.10+): close a nested async generator now, not at GC."""

    def __init__(self, g):
        self.g = g

    async def __aenter__(self):
        return self.g

    async def __aexit__(self, *exc):
        await self.g.aclose()


def _chat_events(c, method, path, body, hdrs):
    async def gen(st):
        async with _aclosing(c._sse(path, body, hdrs, method, st)) as evs:
            async for _, data, eid in evs:
                if data == "[DONE]":
                    return
                v = json.loads(data)
                if "error" in v:
                    raise LMBPAPIError(502, v["error"].get("code", "ERROR"), v["error"].get("message", ""), v)
                if eid:
                    st.last_event_id = eid
                yield v
        raise IncompleteStreamError(st.task_id, st.last_event_id)
    return LMBPStream(gen)


def _hdrs(idem, last_event_id=None):
    h = {}
    if idem:
        h["idempotency-key"] = idem
    if last_event_id:
        h["last-event-id"] = last_event_id
    return h


class _Completions:
    def __init__(self, c: "InProcessClient"):
        self._c = c

    async def create(self, *, idempotency_key: Optional[str] = None, **req: Any) -> dict:
        """Non-streaming chat completion (OpenAI shape). E2EE: pass lmbp_e2ee=sealed.jwe, no messages."""
        return await self._c._json("POST", "/v1/chat/completions", {**req, "stream": False}, _hdrs(idempotency_key))

    def stream(self, *, idempotency_key: Optional[str] = None, last_event_id: Optional[str] = None, **req: Any) -> LMBPStream:
        """Streaming chat completion: ``async for`` chunks (see LMBPStream for the resume cursor)."""
        return _chat_events(self._c, "POST", "/v1/chat/completions", {**req, "stream": True}, _hdrs(idempotency_key, last_event_id))

    def resume(self, task_id: str, last_event_id: str = "") -> LMBPStream:
        """Re-attach to a task's stream after last_event_id ("" replays all); ends at once past the terminal event."""
        return _chat_events(self._c, "GET", "/lmbp/v1/tenant/tasks/" + quote(task_id, safe="") + "/events", None, _hdrs(None, last_event_id))


class _Chat:
    def __init__(self, c: "InProcessClient"):
        self.completions = _Completions(c)


class _Messages:
    def __init__(self, c: "InProcessClient"):
        self._c = c

    async def create(self, *, idempotency_key: Optional[str] = None, **req: Any) -> dict:
        """Anthropic Messages, non-streaming."""
        return await self._c._json("POST", "/v1/messages", {**req, "stream": False}, _hdrs(idempotency_key))

    def stream(self, *, idempotency_key: Optional[str] = None, last_event_id: Optional[str] = None, **req: Any) -> LMBPStream:
        """Anthropic Messages, streaming: ``async for`` named events (ping filtered)."""
        c = self._c

        async def gen(st):
            async with _aclosing(c._sse("/v1/messages", {**req, "stream": True}, _hdrs(idempotency_key, last_event_id), "POST", st)) as evs:
                async for _, data, eid in evs:
                    v = json.loads(data)
                    if v.get("type") == "error":
                        err = v.get("error") or {}
                        raise LMBPAPIError(502, err.get("lmbp_code", "ERROR"), err.get("message", ""), v)
                    if eid:
                        st.last_event_id = eid
                    if v.get("type") != "ping":
                        yield v
                    if v.get("type") == "message_stop":
                        return
            raise IncompleteStreamError(st.task_id, st.last_event_id)
        return LMBPStream(gen)


class _Failed:
    __slots__ = ("err",)

    def __init__(self, err: BaseException):
        self.err = err


class InProcessClient:
    def __init__(self, server: Any, tenant: str, end_user: str):
        self._server, self.tenant, self.end_user = server, tenant, end_user
        self.chat = _Chat(self)
        self.messages = _Messages(self)

    async def models(self) -> list[dict]:
        """GET /v1/models (OpenAI shape)."""
        return (await self._json("GET", "/v1/models", None, {}))["data"]

    async def _request(self, method: str, path: str, body: Any, extra: dict):
        g = self._server.engine
        t = g.tenants.get(self.tenant)
        if t is None:
            raise LMBPAPIError(401, "UNAUTHENTICATED", "unknown tenant " + self.tenant)
        raw = b"" if body is None else json.dumps(body).encode()
        hs = [(b"host", b"in-process"), (b"content-type", b"application/json"), (b"x-end-user", self.end_user.encode())]
        for k, v in (extra or {}).items():
            hs.append((k.encode(), v.encode()))
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
                    start.set_result((m["status"], {k.decode().lower(): v.decode() for k, v in m["headers"]}))
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

        def finished(f: asyncio.Future):
            # One terminal path for the producer: a failure reaches the waiter
            # for headers, or the body consumer as an error item; never a hang.
            err = None if f.cancelled() else f.exception()
            if not start.done():
                start.set_exception(err or RuntimeError("lmbp in-process handler returned no response"))
            elif err is not None:
                q.put_nowait(_Failed(err))
            else:
                q.put_nowait(None)  # idempotent: a second terminator is never read
        task.add_done_callback(finished)

        def release():
            gone.set()
            if not task.done():
                task.cancel()
        try:
            status, headers = await start
        except BaseException:
            release()  # cancelled (or failed) before headers: stop the engine
            raise

        async def chunks():
            try:
                while True:
                    c = await q.get()
                    if c is None:
                        return
                    if isinstance(c, _Failed):
                        raise c.err
                    yield c
            finally:
                release()  # mid-stream cancel, consumer close, or done
        return status, headers, chunks()

    async def _json(self, method, path, body, extra):
        status, _, it = await self._request(method, path, body, extra)
        data = b"".join([c async for c in it])
        v = json.loads(data) if data else None
        if status >= 400:
            raise _error(status, v)
        return v

    async def _sse(self, path, body, extra, method="POST", st=None):
        status, headers, it = await self._request(method, path, body, extra)
        if st is not None:
            st.task_id = headers.get("x-task-id", "")
        if status >= 400 or not headers.get("content-type", "").startswith("text/event-stream"):
            data = b"".join([c async for c in it])
            raise _error(status, json.loads(data) if data else None)
        try:
            buf = ""
            async for c in it:
                buf += c.decode()
                while "\n\n" in buf:
                    block, buf = buf.split("\n\n", 1)
                    ev, data, eid = "", "", ""
                    for line in block.split("\n"):
                        if line.startswith("event: "):
                            ev = line[7:]
                        elif line.startswith("data: "):
                            data += line[6:]
                        elif line.startswith("id: "):
                            eid = line[4:]
                    if data:
                        yield ev, data, eid
        finally:
            await it.aclose()  # deterministic release (nested generators are not)
