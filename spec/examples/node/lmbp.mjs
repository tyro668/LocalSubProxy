// lmbp critical-path primitives for a Node.js gateway (reference skeleton).
// Only node:crypto is needed (Ed25519 since Node 12; JWK import since 15.12).
// Executed against spec/testvectors/vectors.json by test_vectors.mjs.
import { createHash, createPrivateKey, createPublicKey, randomBytes, sign, verify } from 'node:crypto';

export const TYP = { accessToken: 'at+jwt', assertion: 'JWT', proof: 'lmbp-proof+jwt', task: 'lmbp-task+jws', metadata: 'lmbp-metadata+jwt' };
const LEEWAY = 30, PROOF_SKEW = 60, METADATA_MAX_AGE = 86400;
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

// Strict JSON for JWS payloads: same values as JSON.parse, but every object
// remembers (in a hidden NON_INT set) the members written with a fraction or
// exponent, so integer claims (NumericDate, blob size) can be checked as the
// spec requires: digits only (§5.2). JSON.parse turns 1.0 and 1e3 into integers.
const NON_INT = Symbol('lmbp.nonInt');
export function parseStrictJson(text) {
  let i = 0;
  const ws = () => { while (' \t\n\r'.includes(text[i]) && i < text.length) i++; };
  const fail = () => { throw new Error('invalid JSON'); };
  let lastFloat = false;
  const value = () => {
    ws(); lastFloat = false;
    const c = text[i];
    if (c === '{') {
      i++; const o = {}, bad = new Set(); ws();
      Object.defineProperty(o, NON_INT, { value: bad });
      if (text[i] === '}') { i++; return o; }
      for (;;) { ws(); if (text[i] !== '"') fail(); const k = str(); ws(); if (text[i++] !== ':') fail(); Object.defineProperty(o, k, { value: value(), writable: true, enumerable: true, configurable: true }); if (lastFloat) bad.add(k); else bad.delete(k); ws();
        if (text[i] === ',') { i++; continue; } if (text[i++] === '}') return o; fail(); }
    }
    if (c === '[') {
      i++; const a = []; ws();
      if (text[i] === ']') { i++; return a; }
      for (;;) { a.push(value()); ws(); if (text[i] === ',') { i++; continue; } if (text[i++] === ']') return a; fail(); }
    }
    if (c === '"') return str();
    for (const [lit, v] of [['true', true], ['false', false], ['null', null]]) if (text.startsWith(lit, i)) { i += lit.length; return v; }
    const m = /^-?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)?/.exec(text.slice(i));
    if (!m) fail();
    i += m[0].length;
    lastFloat = Boolean(m[2] || m[3]);
    return Number(m[0]);
  };
  const str = () => {
    const m = /^"(?:[^"\\\u0000-\u001f]|\\["\\/bfnrt]|\\u[0-9a-fA-F]{4})*"/.exec(text.slice(i));
    if (!m) fail();
    i += m[0].length;
    return JSON.parse(m[0]);
  };
  const v = value(); ws();
  if (i !== text.length) fail();
  return v;
}

const HEADER_MEMBERS = new Set(['alg', 'typ', 'kid']);

// §5.2: the protected header is exactly {alg: "EdDSA", typ, kid?}; kid, when
// present, is the RFC 7638 thumbprint of the verifying key. The payload MUST
// be a JSON object. Returns the payload object.
export function verifyJws(pubRaw, compact, typ) {
  const parts = typeof compact === 'string' ? compact.split('.') : [];
  if (parts.length !== 3) throw new Error('malformed JWS');
  let h;
  try { h = JSON.parse(unb64u(parts[0]).toString('utf8')); } catch { throw new Error('bad JWS header'); }
  if (!h || typeof h !== 'object' || Array.isArray(h)) throw new Error('bad JWS header');
  for (const k of Object.keys(h)) if (!HEADER_MEMBERS.has(k)) throw new Error(`forbidden JWS header ${k}`);
  if (h.alg !== 'EdDSA') throw new Error('JWS alg must be EdDSA');
  if (typeof h.typ !== 'string' || h.typ !== typ) throw new Error(`JWS typ must be ${typ}`);
  if ('kid' in h && (typeof h.kid !== 'string' || h.kid !== thumbprint(pubRaw))) throw new Error('JWS kid does not match the verifying key');
  let ok = false;
  try { ok = verify(null, Buffer.from(`${parts[0]}.${parts[1]}`, 'ascii'), publicKey(pubRaw), unb64u(parts[2])); } catch { /* bad key or signature encoding */ }
  if (!ok) throw new Error('bad signature');
  const p = parseStrictJson(unb64u(parts[1]).toString('utf8'));
  if (!p || typeof p !== 'object' || Array.isArray(p)) throw new Error('JWS payload must be a JSON object');
  return p;
}

// Claim types (§5.2): NumericDates are JSON integers 0..2^53-1, required
// identifiers non-empty strings, optional ones strings.
const isUint = (c, k) => typeof c[k] === 'number' && Number.isSafeInteger(c[k]) && c[k] >= 0 && !c[NON_INT]?.has(k);
export function requireClaims(c, ints = [], strs = [], opt = []) {
  for (const k of ints) if (!isUint(c, k)) throw new Error(`claim ${k} must be a non-negative integer`);
  for (const k of strs) if (typeof c[k] !== 'string' || c[k] === '') throw new Error(`claim ${k} must be a non-empty string`);
  for (const k of opt) if (typeof c[k] !== 'string') throw new Error(`claim ${k} must be a string`);
}
export function timeRange(c) {
  requireClaims(c, ['iat', 'exp']);
  if (c.exp < c.iat) throw new Error('exp before iat');
}

// Access tokens (at+jwt) and client assertions (JWT): iss/sub/aud/jti, iat <= exp, aud, 30 s leeway.
export function verifyJwt(pub, token, typ, aud, now = Math.floor(Date.now() / 1000)) {
  const c = verifyJws(pub, token, typ);
  requireClaims(c, [], ['iss', 'sub', 'aud', 'jti']);
  timeRange(c);
  if (c.aud !== aud) throw new Error('wrong audience');
  if (now > c.exp + LEEWAY || now + LEEWAY < c.iat) throw new Error('token expired');
  return c;
}
export const verifyAccessToken = (gwPub, token, aud, now) => verifyJwt(gwPub, token, TYP.accessToken, aud, now);
export const verifyClientAssertion = (devPub, token, aud, now) => verifyJwt(devPub, token, TYP.assertion, aud, now); // caller MUST reject a repeated jti

// §18: iss is the gateway's https origin; aud binds the token to one tenant.
export const EXAMPLE_ISSUER = 'https://gw.example.com';
export const tokenAudience = (issuer, tenant) => `${issuer}/tenants/${tenant}`;

export function issueAccessToken(gwKey, kid, { deviceId, tenant, devicePub, ttl = 900, now = Math.floor(Date.now() / 1000), jti, issuer = EXAMPLE_ISSUER }) {
  const claims = { iss: issuer, sub: deviceId, aud: tokenAudience(issuer, tenant), tenant, iat: now, exp: now + ttl,
    jti: jti ?? `jti_${randomBytes(16).toString('hex')}`, cnf: { jkt: thumbprint(devicePub) } };
  return signJws(gwKey, TYP.accessToken, jsonBytes(claims), kid);
}

export function verifyProof(devPub, proof, { iss, aud, credential }, now = Math.floor(Date.now() / 1000)) {
  const c = verifyJws(devPub, proof, TYP.proof);
  requireClaims(c, ['iat'], ['aud', 'jti', 'ath'], ['iss']);
  if (c.aud !== aud || c.iss !== iss || c.ath !== ath(credential)) throw new Error('proof claims invalid');
  if (Math.abs(now - c.iat) > PROOF_SKEW) throw new Error('proof iat outside skew');
  return c; // caller MUST reject a repeated jti
}

export function verifyHello(gwPub, devicePubOf, frame, accessToken, { aud = tokenAudience(EXAMPLE_ISSUER, 'acme'), now } = {}) {
  const claims = verifyAccessToken(gwPub, accessToken, aud, now);
  const devPub = devicePubOf(claims.sub);
  if (claims.cnf?.jkt !== thumbprint(devPub)) throw new Error('token not bound to device key');
  if (frame.v !== 2 || frame.type !== 'hello' || frame.data?.device_id !== claims.sub) throw new Error('bad hello');
  verifyProof(devPub, frame.data.proof, { iss: claims.sub, aud: 'lmbp:hello', credential: accessToken }, now);
  return claims;
}

export const signTaskRun = (gwKey, kid, payload) =>
  ({ v: 2, type: 'task.run', task_id: payload.task_id, seq: 1, jws: signJws(gwKey, TYP.task, jsonBytes(payload), kid) });

// task.run (§5.6): signed payload with task_id/device_id/model/nonce, iat <= exp,
// strict blob references, bound to the envelope task_id; expired after exp.
export function verifyTaskRun(gwPub, frame, now) {
  const p = verifyJws(gwPub, frame.jws, TYP.task);
  requireClaims(p, [], ['task_id', 'device_id', 'model', 'nonce']);
  timeRange(p);
  if (p.blobs !== undefined) {
    if (!Array.isArray(p.blobs)) throw new Error('blobs must be an array');
    for (const b of p.blobs) {
      if (!b || typeof b !== 'object' || Array.isArray(b)) throw new Error('blob ref must be an object');
      requireClaims(b, ['size'], ['id', 'media_type', 'sha256']);
      let d; try { d = unb64u(b.sha256); } catch { d = Buffer.alloc(0); }
      if (d.length !== 32) throw new Error('blob sha256 must be 32 bytes base64url');
    }
  }
  if (p.task_id !== frame.task_id) throw new Error('task_id mismatch');
  if (now !== undefined && now > p.exp) throw new Error('task.run expired');
  return p;
}

// Signed tenant metadata (§19.1): bound to issuer, tenant and token audience,
// speaks 2.0, names the SaaS, valid at most 24 h, endpoints on the issuer origin.
export function verifyMetadata(gwPub, jws, issuer, tenant, now = Math.floor(Date.now() / 1000)) {
  const m = verifyJws(gwPub, jws, TYP.metadata);
  requireClaims(m, [], ['issuer', 'tenant', 'audience']);
  timeRange(m);
  if (!Array.isArray(m.protocol_versions) || !m.protocol_versions.includes('2.0')) throw new Error('gateway does not speak LMBP 2.0');
  if (m.issuer !== issuer || m.tenant !== tenant || m.audience !== tokenAudience(issuer, tenant)) throw new Error('metadata issuer/tenant/audience mismatch');
  if (typeof m.saas?.name !== 'string' || !m.saas.name.trim()) throw new Error('metadata lacks saas.name');
  if (now > m.exp + LEEWAY || now + LEEWAY < m.iat || m.exp - m.iat > METADATA_MAX_AGE) throw new Error('metadata expired or validity too long');
  const origin = new URL(issuer).origin;
  for (const u of [m.jwks_uri, m.device_authorization_endpoint, m.token_endpoint]) {
    let o = '';
    try { o = new URL(u).origin; } catch { /* not a URL */ }
    if (o !== origin) throw new Error(`endpoint ${u} is not on the issuer origin`);
  }
  return m;
}

// Write one OpenAI chunk to an http.ServerResponse; respect backpressure.
export async function writeSseChunk(res, { taskId, model, created, text, finish = null }) {
  const obj = { id: `chatcmpl-${taskId}`, object: 'chat.completion.chunk', created, model,
    choices: [{ index: 0, delta: text === undefined ? {} : { content: text }, finish_reason: finish }] };
  if (!res.write(`data: ${JSON.stringify(obj)}\n\n`)) await new Promise((r) => res.once('drain', r));
}
export const SSE_DONE = 'data: [DONE]\n\n';
