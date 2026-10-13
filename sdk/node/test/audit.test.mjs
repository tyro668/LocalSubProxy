// Regression tests for the 2026-10 audit findings (see reports/LMBP审查问题修复报告.md).
import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import http from 'node:http';
import { LMBPServer, MemoryStore, SqliteStore } from '../dist/index.js';
import { pairAndConnect } from '../test-support/simdevice.mjs';

const KEY = 'k'.repeat(40);
const T = 'acme';

export async function setup(opts = {}) {
  const lsp = new LMBPServer({ tenants: [{ id: T, apiKey: KEY, profile: { display_name: 'Acme' } }], log: () => {}, ...opts });
  const srv = http.createServer(lsp.handler);
  lsp.attach(srv);
  await new Promise((ok) => srv.listen(0, '127.0.0.1', ok));
  const base = `http://127.0.0.1:${srv.address().port}`;
  return { lsp, srv, base, close: async () => { srv.closeAllConnections(); await new Promise((ok) => srv.close(ok)); } };
}
const chat = (base, user, body, hdr = {}) => fetch(base + '/v1/chat/completions', {
  method: 'POST', headers: { Authorization: 'Bearer ' + KEY, 'X-End-User': user, 'Content-Type': 'application/json', ...hdr }, body: JSON.stringify(body) });

// Finding 1: (user a, key b|c) must not collide with (user a|b, key c).
test('idempotency store keys are unambiguous (a, "b|c") vs ("a|b", "c")', () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'idem-'));
  for (const st of [new MemoryStore(), new SqliteStore(path.join(dir, 'i.db'))]) {
    st.putIdempotency(T, 'a', 'b|c', { tenant: T, owner: 'a', taskID: 'task_a', bodyHash: 'h' });
    assert.equal(st.getIdempotency(T, 'a|b', 'c'), undefined, st.constructor.name);
    assert.equal(st.getIdempotency(T, 'a', 'b|c').taskID, 'task_a');
    st.putIdempotency(T, 'a', 'b|c', { tenant: T, owner: 'a', taskID: 'task_other', bodyHash: 'h' });
    assert.equal(st.getIdempotency(T, 'a', 'b|c').taskID, 'task_a', 'first writer wins');
    st.close?.();
  }
});

test('idempotent replay never crosses users (gateway, exact audit example)', async () => {
  const s = await setup();
  const { ws } = await pairAndConnect(s.lsp, s.base, T, 'a');
  try {
    const body = { model: 'm1', messages: [{ role: 'user', content: 'secret-of-a' }] };
    const r1 = await chat(s.base, 'a', body, { 'Idempotency-Key': 'b|c' });
    assert.equal(r1.status, 200);
    assert.match(await r1.text(), /secret-of-a/);
    const r2 = await chat(s.base, 'a|b', body, { 'Idempotency-Key': 'c' });
    const t2 = await r2.text();
    assert.notEqual(r2.status, 200, t2);
    assert.doesNotMatch(t2, /secret-of-a/);
    const r3 = await chat(s.base, 'a', body, { 'Idempotency-Key': 'b|c' });
    assert.equal(r3.status, 200);
    assert.equal(r3.headers.get('x-task-id'), r1.headers.get('x-task-id'));
  } finally { ws.close(); await s.close(); }
});
