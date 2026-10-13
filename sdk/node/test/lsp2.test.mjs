import test from 'node:test';
import assert from 'node:assert/strict';
import crypto from 'node:crypto';
import * as L from '../dist/engine/lsp2.js';

const key = L.signingKey(Buffer.alloc(32, 7));

test('JWS round trip and rejections', () => {
  const jws = L.signJWS(key.priv, 'at+jwt', key.kid, { a: 1 });
  assert.equal(L.verifyJWS(key.pub, jws, 'at+jwt').toString(), '{"a":1}');
  assert.throws(() => L.verifyJWS(key.pub, jws, 'JWT'), /header/);
  const [h, p, s] = jws.split('.');
  assert.throws(() => L.verifyJWS(key.pub, `${h}.${L.b64(Buffer.from('{"a":2}'))}.${s}`, 'at+jwt'), /signature/);
  assert.throws(() => L.verifyJWS(key.pub, `${h}.${p}.${s}=`, 'at+jwt'), /base64url/);
  const crit = L.b64(Buffer.from('{"alg":"EdDSA","typ":"x","crit":["b64"]}')) + '.' + p;
  assert.throws(() => L.verifyJWS(key.pub, crit + '.' + L.b64(crypto.sign(null, Buffer.from(crit), key.priv)), 'x'), /crit/);
});

test('JWT audience and lifetime', () => {
  const iat = L.now();
  const tok = L.signJWS(key.priv, 'at+jwt', '', { aud: 'a', iat, exp: iat + 60 });
  assert.equal(L.verifyJWT(key.pub, tok, 'at+jwt', 'a').aud, 'a');
  assert.throws(() => L.verifyJWT(key.pub, tok, 'at+jwt', 'b'), /audience/);
  assert.throws(() => L.verifyJWT(key.pub, tok, 'at+jwt', 'a', iat + 200), /expired/);
});

test('origin normalization', () => {
  assert.equal(L.origin('HTTPS://GW.Example.com:443/'), 'https://gw.example.com');
  assert.equal(L.origin('http://127.0.0.1:8080'), 'http://127.0.0.1:8080');
  assert.throws(() => L.origin('http://gw.example.com'), /https/);
  assert.throws(() => L.origin('https://gw.example.com/path'), /origin/);
});

test('outbox: windows block, acks release, resume retransmits in order', async () => {
  const sent = [];
  const o = new L.Outbox();
  o.setFlow({ task_window: 150, conn_window: 1000 });
  o.attach((raw) => sent.push(JSON.parse(raw).seq));
  const big = 'x'.repeat(80);
  await o.send('t', () => ({ type: 'blob.chunk', data: { d: big } }));
  let second = false;
  const p = o.send('t', () => ({ type: 'blob.chunk', data: { d: big } })).then(() => { second = true; });
  await new Promise((r) => setTimeout(r, 10));
  assert.equal(second, false, 'second frame must wait for window');
  o.ack('t', 1);
  await p;
  assert.deepEqual(sent, [1, 2]);
  o.attach((raw) => sent.push('r' + JSON.parse(raw).seq)); // reconnect: paused until retransmit
  const p3 = o.send('t', () => ({ type: 'task.run', jws: 'a.b.c' }));
  o.ack('t', 2);
  await p3;
  assert.deepEqual(sent, [1, 2], 'paused until retransmit');
  o.retransmit('t', 2);
  assert.deepEqual(sent, [1, 2, 'r3']);
});

test('inbound: duplicate, gap, overflow', () => {
  const i = new L.Inbound({ task_window: 100, conn_window: 1000 });
  assert.equal(i.accept('t', 1, 60), L.ACCEPTED);
  assert.equal(i.accept('t', 1, 60), L.DUPLICATE);
  assert.equal(i.accept('t', 3, 60), L.GAP);
  assert.equal(i.accept('t', 2, 60), L.OVERFLOW);
  i.acked('t', 1);
  assert.equal(i.accept('t', 2, 60), L.ACCEPTED);
  assert.equal(i.recv('t'), 2);
});
