// lsp2 critical-path primitives for a Node.js gateway (reference skeleton).
// Only node:crypto is needed (Ed25519 since Node 12; JWK import since 15.12).
// Executed against spec/testvectors/vectors.json by test_vectors.mjs.
import { createHash, createPrivateKey, createPublicKey, randomBytes, sign, verify } from 'node:crypto';

export const TYP = { accessToken: 'at+jwt', assertion: 'JWT', proof: 'lsp2-proof+jwt', task: 'lsp2-task+jws' };
const LEEWAY = 30, PROOF_SKEW = 60;
const B64U = /^[A-Za-z0-9_-]*$/;

export const b64u = (buf) => Buffer.from(buf).toString('base64url'); // no padding
export function unb64u(s) {
  // Buffer.from(s,'base64url') silently ignores junk; be strict.
  if (typeof s !== 'string' || !B64U.test(s) || s.length % 4 === 1) throw new Error('invalid base64url');
  return Buffer.from(s, 'base64url');
}
export const jsonBytes = (o) => Buffer.from(JSON.stringify(o), 'utf8');
export const thumbprint = (pub) => b64u(createHash('sha256').update(`{"crv":"Ed25519","kty":"OKP","x":"${b64u(pub)}"}`).digest());
export const ath = (cred) => b64u(createHash('sha256').update(cred, 'utf8').digest());

export const privateKeyFromSeed = (seed, pub) => createPrivateKey({ format: 'jwk', key: { kty: 'OKP', crv: 'Ed25519', d: b64u(seed), x: b64u(pub) } });
export const publicKey = (raw) => createPublicKey({ format: 'jwk', key: { kty: 'OKP', crv: 'Ed25519', x: b64u(raw) } });

export function signJws(key, typ, payloadBytes, kid) {
  const header = kid ? { alg: 'EdDSA', kid, typ } : { alg: 'EdDSA', typ }; // keys sorted
  const input = `${b64u(jsonBytes(header))}.${b64u(payloadBytes)}`;
  return `${input}.${b64u(sign(null, Buffer.from(input, 'ascii'), key))}`;
}

export function verifyJws(pubRaw, compact, typ) {
  const parts = compact.split('.');
  if (parts.length !== 3) throw new Error('malformed JWS');
  const h = JSON.parse(unb64u(parts[0]).toString('utf8'));
  if (h.alg !== 'EdDSA' || h.typ !== typ || 'crit' in h) throw new Error('bad JWS header');
  const ok = verify(null, Buffer.from(`${parts[0]}.${parts[1]}`, 'ascii'), publicKey(pubRaw), unb64u(parts[2]));
  if (!ok) throw new Error('bad signature');
  return unb64u(parts[1]);
}

export function verifyAccessToken(gwPub, token, aud, now = Math.floor(Date.now() / 1000)) {
  const c = JSON.parse(verifyJws(gwPub, token, TYP.accessToken));
  if (c.aud !== aud || now > c.exp + LEEWAY || now + LEEWAY < c.iat) throw new Error('token aud/time invalid');
  return c;
}

export function issueAccessToken(gwKey, kid, { deviceId, tenant, devicePub, ttl = 900, now = Math.floor(Date.now() / 1000), jti }) {
  const claims = { iss: 'lsp2-gateway', sub: deviceId, aud: 'lsp2-gateway', tenant, iat: now, exp: now + ttl,
    jti: jti ?? `jti_${randomBytes(16).toString('hex')}`, cnf: { jkt: thumbprint(devicePub) } };
  return signJws(gwKey, TYP.accessToken, jsonBytes(claims), kid);
}

export function verifyProof(devPub, proof, { iss, aud, credential }, now = Math.floor(Date.now() / 1000)) {
  const c = JSON.parse(verifyJws(devPub, proof, TYP.proof));
  if (c.aud !== aud || c.iss !== iss || !c.jti || c.ath !== ath(credential)) throw new Error('proof claims invalid');
  if (Math.abs(now - c.iat) > PROOF_SKEW) throw new Error('proof iat outside skew');
  return c; // caller MUST reject a repeated jti
}

export function verifyHello(gwPub, devicePubOf, frame, accessToken, { aud = 'lsp2-gateway', now } = {}) {
  const claims = verifyAccessToken(gwPub, accessToken, aud, now);
  const devPub = devicePubOf(claims.sub);
  if (claims.cnf?.jkt !== thumbprint(devPub)) throw new Error('token not bound to device key');
  if (frame.v !== 2 || frame.type !== 'hello' || frame.data?.device_id !== claims.sub) throw new Error('bad hello');
  verifyProof(devPub, frame.data.proof, { iss: claims.sub, aud: 'lsp2:hello', credential: accessToken }, now);
  return claims;
}

export const signTaskRun = (gwKey, kid, payload) =>
  ({ v: 2, type: 'task.run', task_id: payload.task_id, jws: signJws(gwKey, TYP.task, jsonBytes(payload), kid) });

export function verifyTaskRun(gwPub, frame) {
  const p = JSON.parse(verifyJws(gwPub, frame.jws, TYP.task));
  if (p.task_id !== frame.task_id) throw new Error('task_id mismatch');
  return p;
}

// Write one OpenAI chunk to an http.ServerResponse; respect backpressure.
export async function writeSseChunk(res, { taskId, model, created, text, finish = null }) {
  const obj = { id: `chatcmpl-${taskId}`, object: 'chat.completion.chunk', created, model,
    choices: [{ index: 0, delta: text === undefined ? {} : { content: text }, finish_reason: finish }] };
  if (!res.write(`data: ${JSON.stringify(obj)}\n\n`)) await new Promise((r) => res.once('drain', r));
}
export const SSE_DONE = 'data: [DONE]\n\n';
