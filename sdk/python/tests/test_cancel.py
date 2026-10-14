"""A4: the in-process client handles cancel while waiting for headers,
mid-stream cancel, consumer close and a producer failure the same way: the
caller gets an error (never a hang) and the engine's route is cancelled."""
import asyncio

import pytest

from lmbp_server.client import InProcessClient
from lmbp_server._http import StreamResponse


class FakeServer:
    def __init__(self, route):
        self.engine = type("E", (), {})()
        self.engine.tenants = {"t": object()}
        self.engine.route = route


def tracked_streamer(state):
    async def route(req):
        try:
            r = StreamResponse(200, {"content-type": "text/event-stream", "x-task-id": "task_1"})
            await r.prepare(req)
            await r.write(b'id: 0\ndata: {"choices":[{"delta":{"content":"a"}}]}\n\n')
            await asyncio.Event().wait()
        except asyncio.CancelledError:
            state["cancelled"] = True
            raise
    return route


async def within(aw, what, t=2.0):
    try:
        return await asyncio.wait_for(aw, t)
    except asyncio.TimeoutError:
        pytest.fail("timed out: " + what)


async def until(cond, what):
    for _ in range(200):
        if cond():
            return
        await asyncio.sleep(0.01)
    pytest.fail("timed out: " + what)


async def test_cancel_while_waiting_for_headers():
    state = {}

    async def route(req):
        try:
            await asyncio.Event().wait()
        except asyncio.CancelledError:
            state["cancelled"] = True
            raise
    c = InProcessClient(FakeServer(route), "t", "u")
    call = asyncio.ensure_future(c.models())
    await asyncio.sleep(0.05)
    call.cancel()
    with pytest.raises(asyncio.CancelledError):
        await call
    await until(lambda: state.get("cancelled"), "engine route cancelled with the caller")


async def test_mid_stream_cancel():
    state = {}
    c = InProcessClient(FakeServer(tracked_streamer(state)), "t", "u")
    st = c.chat.completions.stream(model="m", messages=[])
    first = await within(st.__anext__(), "first chunk")
    assert first["choices"][0]["delta"]["content"] == "a"
    nxt = asyncio.ensure_future(st.__anext__())
    await asyncio.sleep(0.05)
    nxt.cancel()
    with pytest.raises(asyncio.CancelledError):
        await nxt
    await until(lambda: state.get("cancelled"), "engine route cancelled after mid-stream cancel")


async def test_consumer_close():
    state = {}
    c = InProcessClient(FakeServer(tracked_streamer(state)), "t", "u")
    st = c.chat.completions.stream(model="m", messages=[])
    await within(st.__anext__(), "first chunk")
    await within(st.aclose(), "aclose")
    await until(lambda: state.get("cancelled"), "engine route cancelled after aclose()")


async def test_producer_dies_after_headers():
    async def route(req):
        r = StreamResponse(200, {"content-type": "text/event-stream", "x-task-id": "task_1"})
        await r.prepare(req)
        await r.write(b'id: 0\ndata: {"choices":[{"delta":{"content":"a"}}]}\n\n')
        await asyncio.sleep(0.01)
        raise RuntimeError("producer died")
    c = InProcessClient(FakeServer(route), "t", "u")
    st = c.chat.completions.stream(model="m", messages=[])
    await within(st.__anext__(), "first chunk")
    with pytest.raises(RuntimeError, match="producer died"):
        await within(st.__anext__(), "consumer after producer failure")


async def test_producer_dies_before_headers():
    async def route(req):
        raise RuntimeError("boom")
    c = InProcessClient(FakeServer(route), "t", "u")
    with pytest.raises(RuntimeError, match="boom"):
        await within(c.models(), "models")
