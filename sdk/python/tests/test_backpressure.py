"""B2: an in-process caller that stops reading must stop the device (acks
follow delivery, §15.2), so the in-process body buffer and the gateway's
memory stay bounded; a close while the producer is blocked propagates."""
import asyncio

from simdevice import pair_and_connect
from test_sdk import T, new


async def test_slow_consumer_bounded(serve_app):
    lmbp = new()
    stats = {"sent": 0}
    n, size = 256, 32 * 1024  # 8 MiB; window 1 MiB
    async with serve_app(lmbp.asgi()) as base:
        ws, task, _ = await pair_and_connect(lmbp, base, T, "u1", flood={"n": n, "size": size, "stats": stats})
        c = lmbp.client(T, "u1")
        st = c.chat.completions.stream(model="m1", messages=[{"role": "user", "content": "go"}])
        first = await st.__anext__()
        await asyncio.sleep(1.5)  # stall
        stalled = stats["sent"]
        assert stalled < 3 * (1 << 20), f"device sent {stalled} bytes to a stalled consumer (want < 3 MiB of 8 MiB)"
        got = len((first["choices"][0]["delta"].get("content") or "") if first["choices"] else "")
        async for ch in st:
            if ch["choices"]:
                got += len(ch["choices"][0]["delta"].get("content") or "")
        assert got == n * size
        await ws.close()
        task.cancel()


async def test_close_while_producer_blocked(serve_app):
    lmbp = new()
    stats = {"sent": 0}
    async with serve_app(lmbp.asgi()) as base:
        ws, task, _ = await pair_and_connect(lmbp, base, T, "u1", flood={"n": 256, "size": 32 * 1024, "stats": stats})
        c = lmbp.client(T, "u1")
        st = c.chat.completions.stream(model="m1", messages=[{"role": "user", "content": "go"}])
        await st.__anext__()
        await asyncio.sleep(0.5)  # producer now blocked on the full buffer
        await asyncio.wait_for(st.aclose(), 2)
        for _ in range(200):
            if stats.get("cancelled"):
                break
            await asyncio.sleep(0.02)
        assert stats.get("cancelled"), "closing the stream did not cancel the task on the device"
        await ws.close()
        task.cancel()
