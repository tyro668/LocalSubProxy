"""Audit finding 2: concurrent senders must never jointly exceed the windows."""
import asyncio
import json

from lmbp_server.protocol import Outbox

W = 262144


def test_outbox_never_exceeds_window_under_concurrency():
    async def run():
        o = Outbox()
        o.set_flow({"task_window": W, "conn_window": W})
        pending: dict[str, int] = {}
        peak = {"task": 0, "conn": 0}

        def write(raw):
            e = json.loads(raw)
            pending[e["task_id"]] = max(pending.get(e["task_id"], 0), e["seq"])
        o.attach(write)

        async def acker():
            while True:
                await asyncio.sleep(0.003)
                for tid, s in list(pending.items()):
                    o.ack(tid, s)
                pending.clear()

        text = "x" * 20000

        async def sender(tid):
            for _ in range(20):
                await o.send(tid, lambda seq: {"type": "task.delta", "data": {"text": text}})
                peak["task"] = max(peak["task"], o._task(tid)["used"])
                peak["conn"] = max(peak["conn"], o.conn_used)

        a = asyncio.create_task(acker())
        await asyncio.gather(*(sender(f"task_{t}") for t in range(4) for _ in range(8)))
        a.cancel()
        return peak

    peak = asyncio.run(run())
    print(f"max unacked per task {peak['task']}, per connection {peak['conn']} (window {W})")
    assert peak["task"] <= W and peak["conn"] <= W, f"window exceeded: {peak} > {W}"
