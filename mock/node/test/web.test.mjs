import test from 'node:test';
import assert from 'node:assert/strict';
import { Gateway } from '../gateway.mjs';

test('chat UI is served at / and /chat with CSP; disabled without webDir', async () => {
  const g = new Gateway({ webDir: new URL('../../web', import.meta.url).pathname, log: () => {} });
  const srv = await g.listen(0, '127.0.0.1');
  const base = `http://127.0.0.1:${srv.port}`;
  try {
    for (const [p, ct] of [['/', 'text/html'], ['/chat', 'text/html'], ['/chat/chat.js', 'text/javascript'], ['/chat/qrcode.js', 'text/javascript']]) {
      const r = await fetch(base + p);
      assert.equal(r.status, 200, p);
      assert.ok(r.headers.get('content-type').startsWith(ct), p);
      assert.match(r.headers.get('content-security-policy'), /script-src 'self';/);
    }
    assert.equal((await fetch(base + '/chat', { method: 'POST' })).status, 404);
  } finally { g.server?.close(); }
  const g2 = new Gateway({ log: () => {} });
  const s2 = await g2.listen(0, '127.0.0.1');
  try { assert.equal((await fetch(`http://127.0.0.1:${s2.port}/chat`)).status, 404); } finally { g2.server?.close(); }
});
