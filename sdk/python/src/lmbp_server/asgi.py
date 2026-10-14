"""ASGI adapter: serves the engine under any ASGI server (uvicorn, hypercorn)
and mounts into FastAPI / Starlette, either wrapping the app or as middleware.
"""
from __future__ import annotations

import asyncio
import contextlib
from typing import Any, Awaitable, Callable, Optional

from ._http import Request, Response

ASGIApp = Callable[[dict, Callable[[], Awaitable[dict]], Callable[[dict], Awaitable[None]]], Awaitable[None]]
MAX_BODY = 48 << 20


class LMBPASGI:
    """ASGI application for the gateway. With ``app``, paths the gateway does
    not own are passed to it (so it works as a wrapper / middleware)."""

    def __init__(self, engine: Any, app: Optional[ASGIApp] = None):
        self.engine, self.app = engine, app

    async def __call__(self, scope: dict, receive, send) -> None:
        typ = scope["type"]
        if typ == "lifespan":
            if self.app is not None:
                return await self.app(scope, receive, send)
            while True:
                m = await receive()
                if m["type"] == "lifespan.startup":
                    await send({"type": "lifespan.startup.complete"})
                elif m["type"] == "lifespan.shutdown":
                    await send({"type": "lifespan.shutdown.complete"})
                    return
        if typ not in ("http", "websocket") or not self.engine.handles(_path(scope)):
            if self.app is not None:
                return await self.app(scope, receive, send)
            if typ == "http":
                return await Response(404, b'{"error":"not found"}', "application/json").send_to(Request(scope, receive, send, b""))
            if typ == "websocket":
                await receive()
                await send({"type": "websocket.close", "code": 1008})
            return
        scope = dict(scope, path=_path(scope))
        if typ == "websocket":
            return await self._websocket(scope, receive, send)
        return await self._http(scope, receive, send)

    async def _http(self, scope, receive, send):
        # Read the body first, then watch for the client going away: a
        # disconnect cancels the handler, which turns a dropped SSE stream into
        # task.cancel (§7) exactly like the other gateways.
        body = bytearray()
        while True:
            m = await receive()
            if m["type"] == "http.disconnect":
                return
            body += m.get("body", b"")
            if len(body) > MAX_BODY or not m.get("more_body"):
                break
        req = Request(scope, receive, send, bytes(body))
        handler = asyncio.ensure_future(self.engine.route(req))

        async def watch():
            while True:
                m = await receive()
                if m["type"] == "http.disconnect":
                    handler.cancel()
                    return
        watcher = asyncio.ensure_future(watch())
        try:
            resp = await handler
            if isinstance(resp, Response) and not req.started:
                await resp.send_to(req)
        except asyncio.CancelledError:
            if not handler.cancelled():
                raise
        finally:
            watcher.cancel()
            with contextlib.suppress(asyncio.CancelledError, Exception):
                await watcher

    async def _websocket(self, scope, receive, send):
        req = Request(scope, receive, send)
        resp = await self.engine.route(req)
        if isinstance(resp, Response) and not req.started:
            # Rejected upgrade: an HTTP status and body when the server
            # supports the ASGI websocket.http.response extension, else a
            # close before accept (servers answer 403).
            if "websocket.http.response" in scope.get("extensions", {}):
                hs = [(k.encode("latin-1"), str(v).encode("latin-1")) for k, v in resp.headers.items()]
                await send({"type": "websocket.http.response.start", "status": resp.status, "headers": hs})
                await send({"type": "websocket.http.response.body", "body": resp.body})
            else:
                m = await receive()
                if m["type"] == "websocket.connect":
                    await send({"type": "websocket.close", "code": 1008})


def _path(scope: dict) -> str:
    p, root = scope.get("path", "/"), scope.get("root_path", "")
    return p[len(root):] or "/" if root and p.startswith(root) else p


class LMBPMiddleware:
    """Starlette / FastAPI middleware: ``app.add_middleware(LMBPMiddleware, server=lsp)``."""

    def __init__(self, app: ASGIApp, server: Any):
        self._inner = LMBPASGI(server.engine, app)

    async def __call__(self, scope, receive, send):
        await self._inner(scope, receive, send)
