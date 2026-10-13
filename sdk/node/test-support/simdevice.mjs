// Minimal simulated desktop device for SDK tests: pairs through the gateway's
// RFC 8628 endpoints, connects the lmbp WebSocket and answers task.run with an
// echo (or a get_weather tool call when tools are offered).
import crypto from 'node:crypto';
import WebSocket from 'ws';
import * as L from '../dist/engine/lmbp.js';
import * as E from '../dist/e2ee.js';

const proof = (priv, iss, aud, cred) => L.signJWS(priv, L.TYP.proof, '', { iss, aud, iat: L.now(), jti: L.randomID('p_'), ath: L.ath(cred) });

export async function startPairing(base, tenant, name = 'test-pc') {
  const { priv, pub } = L.signingKey(crypto.randomBytes(32));
  const r = await fetch(base + '/lmbp/v1/device/authorize', { method: 'POST', headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
    body: new URLSearchParams({ tenant, public_key: L.b64(pub), device_name: name }) });
  const a = await r.json();
  return { priv, pub, ...a };
}

export async function finishPairing(base, p) {
  const r = await fetch(base + '/lmbp/v1/token', { method: 'POST', headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
    body: new URLSearchParams({ grant_type: 'urn:ietf:params:oauth:grant-type:device_code', device_code: p.device_code, proof: proof(p.priv, '', L.PROOF.token, p.device_code) }) });
  if (r.status !== 200) throw new Error('token ' + r.status + ' ' + (await r.text()));
  return r.json();
}

export async function connect(base, p, tok, models = ['m1'], opts = {}) {
  const x = opts.e2ee ? E.x25519KeyPair() : null;
  const ws = new WebSocket(base.replace(/^http/, 'ws') + '/lmbp/v1/connect', L.SUBPROTOCOL, { headers: { Authorization: 'Bearer ' + tok.access_token } });
  await new Promise((ok, bad) => { ws.once('open', ok); ws.once('error', bad); });
  const caps = Object.fromEntries(models.map((m) => [m, ['tools']]));
  ws.send(JSON.stringify({ v: 1, type: 'hello', id: 'h1', data: { device_id: tok.device_id, client: 'sdk-test', models, model_capabilities: caps,
    max_concurrency: 4, capabilities: x ? ['tools', 'e2ee'] : ['tools'],
    ...(x ? { e2ee_key: E.signKeyStatement(p.priv, p.pub, tok.device_id, x.pub) } : {}), proof: proof(p.priv, tok.device_id, L.PROOF.hello, tok.access_token) } }));
  await new Promise((ok, bad) => ws.once('message', (m) => (JSON.parse(m).type === 'welcome' ? ok() : bad(new Error(String(m))))));
  const pending = new Map(); // task_id -> { run, chunks } while an E2EE blob arrives
  ws.on('message', (raw) => {
    const e = JSON.parse(raw);
    if (e.type === 'ping') return ws.send(JSON.stringify({ v: 1, type: 'pong', id: e.id }));
    if (e.type === 'blob.chunk' && pending.has(e.task_id)) {
      const pt = pending.get(e.task_id);
      pt.chunks.push(Buffer.from(e.data.data, 'base64url'));
      if (!e.data.last) return;
      pending.delete(e.task_id);
      return runE2EE(pt.run, Buffer.concat(pt.chunks).toString());
    }
    if (e.type !== 'task.run') return;
    const run = JSON.parse(Buffer.from(e.jws.split('.')[1], 'base64url'));
    if (run.e2ee) return void pending.set(run.task_id, { run, chunks: [] });
    let seq = 0;
    const send = (type, data) => ws.send(JSON.stringify({ v: 1, type, task_id: run.task_id, seq: ++seq, data }));
    const last = run.messages.at(-1);
    const text = typeof last.content === 'string' ? last.content : (last.content || []).filter((c) => c.type === 'text').map((c) => c.text).join('');
    if (run.tools?.length && last.role === 'user') {
      send('task.delta', { tool_calls: [{ index: 0, id: 'call_1', type: 'function', function: { name: run.tools[0].function.name, arguments: '{"city":' } }] });
      send('task.delta', { tool_calls: [{ index: 0, function: { arguments: '"Paris"}' } }] });
      return send('task.done', { finish_reason: 'tool_calls', usage: { prompt_tokens: 5, completion_tokens: 2 } });
    }
    send('task.delta', { text: 'echo: ' });
    send('task.delta', { text: last.role === 'tool' ? 'tool said ' + last.content : text });
    send('task.done', { finish_reason: 'stop', usage: { prompt_tokens: 3, completion_tokens: 4 } });
  });
  // E2EE: open the sealed request, answer "echo:<text>" in sealed frames.
  function runE2EE(run, jwe) {
    const req = JSON.parse(E.openJwe(x.priv, jwe).plaintext.toString());
    const key = Buffer.from(req.response_key, 'base64url');
    let seq = 0;
    const send = (type, data) => ws.send(JSON.stringify({ v: 1, type, task_id: run.task_id, seq: ++seq, data }));
    const last = req.messages.at(-1);
    const sealed = (type, v) => E.sealChunk(key, req.rid, type, seq + 1, Buffer.from(JSON.stringify(v)));
    send('task.delta', { e2ee: sealed('task.delta', { text: 'echo:' + last.content }) });
    send('task.done', { finish_reason: 'stop', usage: { prompt_tokens: 1, completion_tokens: 1 }, e2ee: sealed('task.done', { finish_reason: 'stop' }) });
  }
  return ws;
}

/** Pair (approving through the SDK API) and connect; returns {ws, deviceId}. */
export async function pairAndConnect(lsp, base, tenant, user, opts = {}) {
  const p = await startPairing(base, tenant);
  const ok = lsp.pairing.approve(tenant, user, p.user_code);
  const tok = await finishPairing(base, p);
  const ws = await connect(base, p, tok, ['m1'], opts);
  for (let i = 0; i < 50 && !lsp.pairing.listDevices(tenant, user).find((d) => d.online); i++) await new Promise((r) => setTimeout(r, 20));
  return { ws, deviceId: ok.deviceId, jkt: ok.jkt, pairing: p };
}
