"""Audit finding 6: WebSocket frame limits count UTF-8 bytes, not characters."""
import asyncio

from lmbp_server import protocol as L
from lmbp_server._http import Request, WebSocketResponse, WSMsgType


def _run(text):
    async def go():
        sent = []
        q = asyncio.Queue()
        for m in ({"type": "websocket.connect"}, {"type": "websocket.receive", "text": text}):
            q.put_nowait(m)

        async def send(m):
            sent.append(m)
        req = Request({"type": "websocket", "headers": [(b"sec-websocket-protocol", b"lmbp")], "path": "/", "query_string": b""}, q.get, send)
        ws = WebSocketResponse(protocols=("lmbp",), max_msg_size=L.MAX_FRAME_BYTES)
        await ws.prepare(req)
        msg = await ws.receive()
        return msg, sent
    return asyncio.run(go())


def test_cjk_frame_over_limit_in_bytes_is_rejected():
    text = "中" * 100_000  # 100,000 chars but 300,000 UTF-8 bytes > 262,144
    assert len(text) < L.MAX_FRAME_BYTES < len(text.encode())
    msg, sent = _run(text)
    assert msg.type == WSMsgType.ERROR, "a frame over max_frame_bytes in UTF-8 bytes was accepted"
    assert any(m.get("type") == "websocket.close" and m.get("code") == 1009 for m in sent)


def test_cjk_frame_under_limit_is_accepted():
    text = "中" * 80_000  # 240,000 bytes
    msg, _ = _run(text)
    assert msg.type == WSMsgType.TEXT and msg.data == text
