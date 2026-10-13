// Minimal simulated desktop device for SDK tests: pairs through the gateway's
// RFC 8628 endpoints, connects the lsp2 WebSocket and answers task.run with an
// echo (or a get_weather tool call when tools are offered).
import crypto from 'node:crypto';
import WebSocket from 'ws';
import * as L from '../dist/engine/lsp2.js';

const proof = (priv, iss, aud, cred) => L.signJWS(priv, L.TYP.proof, '', { iss, aud, iat: L.now(), jti: L.randomID('p_'), ath: L.ath(cred) });

export async function startPairing(base, tenant, name = 'test-pc') {
  const { priv, pub } = L.signingKey(crypto.randomBytes(32));
  const r = await fetch(base + '/v2/device/authorize', { method: 'POST', headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
    body: new URLSearchParams({ tenant, public_key: L.b64(pub), device_name: name }) });
  const a = await r.json();
  return { priv, pub, ...a };
}

export async function finishPairing(base, p) {
  const r = await fetch(base + '/v2/token', { method: 'POST', headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
    body: new URLSearchParams({ grant_type: 'urn:ietf:params:oauth:grant-type:device_code', device_code: p.device_code, proof: proof(p.priv, '', L.PROOF.token, p.device_code) }) });
  if (r.status !== 200) throw new Error('token ' + r.status + ' ' + (await r.text()));
  return r.json();
}

export async function connect(base, p, tok, models = ['m1']) {
  const ws = new WebSocket(base.replace(/^http/, 'ws') + '/v2/connect', L.SUBPROTOCOL, { headers: { Authorization: 'Bearer ' + tok.access_token } });
  await new Promise((ok, bad) => { ws.once('open', ok); ws.once('error', bad); });
  const caps = Object.fromEntries(models.map((m) => [m, ['tools']]));
  ws.send(JSON.stringify({ v: 2, type: 'hello', id: 'h1', data: { device_id: tok.device_id, client: 'sdk-test', models, model_capabilities: caps,
    max_concurrency: 4, capabilities: ['tools'], proof: proof(p.priv, tok.device_id, L.PROOF.hello, tok.access_token) } }));
  await new Promise((ok, bad) => ws.once('message', (m) => (JSON.parse(m).type === 'welcome' ? ok() : bad(new Error(String(m))))));
  ws.on('message', (raw) => {
    const e = JSON.parse(raw);
    if (e.type === 'ping') return ws.send(JSON.stringify({ v: 2, type: 'pong', id: e.id }));
    if (e.type !== 'task.run') return;
    const run = JSON.parse(Buffer.from(e.jws.split('.')[1], 'base64url'));
    let seq = 0;
    const send = (type, data) => ws.send(JSON.stringify({ v: 2, type, task_id: run.task_id, seq: ++seq, data }));
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
  return ws;
}

/** Pair (approving through the SDK API) and connect; returns {ws, deviceId}. */
export async function pairAndConnect(lsp, base, tenant, user) {
  const p = await startPairing(base, tenant);
  const ok = lsp.pairing.approve(tenant, user, p.user_code);
  const tok = await finishPairing(base, p);
  const ws = await connect(base, p, tok);
  for (let i = 0; i < 50 && !lsp.pairing.listDevices(tenant, user).find((d) => d.online); i++) await new Promise((r) => setTimeout(r, 20));
  return { ws, deviceId: ok.deviceId, pairing: p };
}
