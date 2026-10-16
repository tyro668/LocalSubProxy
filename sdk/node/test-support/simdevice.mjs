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
  if (opts.beforeHello) await opts.beforeHello(); // e.g. revoke while the gateway awaits the hello
  const hello = { device_id: tok.device_id, client: 'sdk-test', models, model_capabilities: caps,
    max_concurrency: opts.maxConcurrency ?? 4, capabilities: [...(x ? ['tools', 'e2ee'] : ['tools']), ...(opts.flood ? ['flow'] : []), ...(opts.extraCaps || [])],
    ...(x ? { e2ee_key: E.signKeyStatement(p.priv, p.pub, tok.device_id, x.pub) } : {}), proof: proof(p.priv, tok.device_id, L.PROOF.hello, tok.access_token), ...(opts.helloPatch || {}) };
  ws.send(JSON.stringify({ v: 1, type: 'hello', id: 'h1', data: hello }));
  const welcome = await new Promise((ok, bad) => ws.once('message', (m) => { const w = JSON.parse(m); w.type === 'welcome' ? ok(w) : bad(new Error(String(m))); }));
  // opts.flood {n, size, stats}: answer every task with n deltas of size
  // bytes, honouring the gateway's flow window (§15.3); stats.sent counts
  // bytes the window let through.
  const fl = opts.flood && { win: welcome.data?.flow?.task_window ?? Infinity, acked: 0, frames: [], wake: null };
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
    if (fl && e.type === 'task.ack') { fl.acked = Math.max(fl.acked, e.data.seq); fl.wake?.(); return; }
    if (e.type !== 'task.run') return;
    const run = JSON.parse(Buffer.from(e.jws.split('.')[1], 'base64url'));
    opts.onRun?.(run); // tests inspect the signed task.run payload
    if (run.e2ee) return void pending.set(run.task_id, { run, chunks: [] });
    if (fl) return void flood(run);
    let seq = 0;
    const send0 = (type, data) => ws.send(JSON.stringify({ v: 1, type, task_id: run.task_id, seq: ++seq, data }));
    // opts.delayMs: hold the task (and its concurrency slot) before answering
    const send = opts.delayMs ? (type, data) => setTimeout(() => send0(type, data), opts.delayMs) : send0;
    const last = run.messages.at(-1);
    const text = typeof last.content === 'string' ? last.content : (Array.isArray(last.content) ? last.content : []).filter((c) => c.type === 'text').map((c) => c.text).join('');
    if (run.tools?.length && last.role === 'user') {
      send('task.delta', { tool_calls: [{ index: 0, id: 'call_1', type: 'function', function: { name: run.tools[0].function.name, arguments: '{"city":' } }] });
      send('task.delta', { tool_calls: [{ index: 0, function: { arguments: '"Paris"}' } }] });
      return send('task.done', { finish_reason: 'tool_calls', usage: { prompt_tokens: 5, completion_tokens: 2 } });
    }
    send('task.delta', { text: 'echo: ' });
    send('task.delta', { text: last.role === 'tool' ? 'tool said ' + last.content : text });
    send('task.done', { finish_reason: 'stop', usage: { prompt_tokens: 3, completion_tokens: 4 } });
  });
  async function flood(run) {
    const { n, size, stats } = opts.flood;
    let seq = 0;
    const unacked = () => fl.frames.filter((f) => f.seq > fl.acked).reduce((a, f) => a + f.len, 0);
    for (let i = 0; i <= n; i++) {
      const last = i === n;
      const m = JSON.stringify({ v: 1, type: last ? 'task.done' : 'task.delta', task_id: run.task_id, seq: ++seq,
        data: last ? { finish_reason: 'stop', usage: { prompt_tokens: 1, completion_tokens: n } } : { text: 'x'.repeat(size) } });
      while (unacked() > 0 && unacked() + m.length > fl.win) {
        if (ws.readyState !== ws.OPEN) return;
        await new Promise((ok) => { fl.wake = ok; setTimeout(ok, 50); });
      }
      if (ws.readyState !== ws.OPEN) return;
      fl.frames.push({ seq, len: m.length });
      ws.send(m);
      stats.sent += m.length;
    }
  }
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
  return { ws, deviceId: ok.deviceId, jkt: ok.jkt, pairing: p, tok };
}
