// Audit finding 2: concurrent senders must never jointly exceed the windows.
import test from 'node:test';
import assert from 'node:assert/strict';
import { Outbox } from '../dist/engine/lmbp.js';

test('outbox never exceeds the window under concurrency', async () => {
  const W = 262144;
  const o = new Outbox();
  o.setFlow({ task_window: W, conn_window: W });
  let maxTask = 0, maxConn = 0;
  const pending = new Map(); // task → highest written seq
  o.attach((raw) => { const e = JSON.parse(raw); pending.set(e.task_id, Math.max(pending.get(e.task_id) ?? 0, e.seq)); });
  const sample = (id) => { maxTask = Math.max(maxTask, o.task(id).used); maxConn = Math.max(maxConn, o.connUsed); };
  const timer = setInterval(() => { for (const [id, s] of pending) o.ack(id, s); pending.clear(); }, 3);
  const text = 'x'.repeat(20000);
  const senders = [];
  for (let t = 0; t < 4; t++) for (let g = 0; g < 8; g++) senders.push((async () => {
    const id = `task_${t}`;
    for (let i = 0; i < 20; i++) { await o.send(id, () => ({ type: 'task.delta', data: { text } })); sample(id); }
  })());
  await Promise.all(senders);
  clearInterval(timer);
  console.log(`max unacked per task ${maxTask}, per connection ${maxConn} (window ${W})`);
  assert.ok(maxTask <= W && maxConn <= W, `window exceeded: task ${maxTask} conn ${maxConn} > ${W}`);
});
