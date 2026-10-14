// B3: per-frame open() is stateless; the response reader enforces seq order
// (last+1) and the sealed task.done terminator.
import test from 'node:test';
import assert from 'node:assert/strict';
import { sealChunk, openChunk } from '../dist/e2ee.js';
import { e2eeReader } from '../dist/index.js';

const key = Buffer.alloc(32, 7), rid = 'rid_test';
const pts = [['task.delta', '{"text":"a"}'], ['task.delta', '{"text":"b"}'], ['task.done', '{"finish_reason":"stop"}']];
const f = pts.map(([type, pt], i) => ({ seq: i + 1, type, data: sealChunk(key, rid, type, i + 1, Buffer.from(pt)) }));
const sealed = { jwe: '', rid, responseKey: key, open: (x) => openChunk(key, rid, x.type, x.seq, x.data) };

function readAll(frames, after = 0) {
  const r = e2eeReader(sealed, after);
  let out = '';
  for (const x of frames) { const p = r.next(x); if (x.type === 'task.delta') out += JSON.parse(p).text; }
  r.finish();
  return out;
}

test('stateless open() alone accepts reordered and duplicated frames', () => {
  for (const i of [1, 0, 0, 2]) sealed.open(f[i]);
});

test('reader enforces order and the terminator', () => {
  assert.equal(readAll(f), 'ab');
  assert.equal(readAll(f.slice(1), 1), 'b');
  const cases = {
    reordered: [f[1], f[0], f[2]], duplicated: [f[0], f[0], f[1], f[2]], 'dropped middle': [f[0], f[2]],
    truncated: [f[0], f[1]], 'after done': [f[0], f[1], f[2], f[2]], 'type swapped': [f[0], f[1], { ...f[2], type: 'task.delta' }],
  };
  for (const [name, frames] of Object.entries(cases)) assert.throws(() => readAll(frames), /e2ee/, name);
});
