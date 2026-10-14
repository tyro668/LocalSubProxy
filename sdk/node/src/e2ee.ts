/**
 * End-to-end encryption from business code (spec §17). The gateway relays the
 * sealed request and the sealed response frames but cannot read them; only
 * your backend and the user's device can.
 *
 *   const dev = server.pairing.listDevices(tenant, user)[0];
 *   const sealed = sealE2EERequest(dev, pinnedJkt, { model, messages });
 *   const rd = sealed.reader(); // enforces seq order and the sealed task.done
 *   for await (const c of client.chat.completions.stream({ model, lmbp_e2ee: sealed.jwe })) {
 *     if (!c.lmbp_e2ee) continue;
 *     const p = rd.next(c.lmbp_e2ee);
 *     if (c.lmbp_e2ee.type === 'task.delta') text += decodeE2EEDelta(p).text ?? '';
 *   }
 *   rd.finish(); // throws if the gateway truncated the response
 *
 * pinnedJkt is the device key thumbprint your backend recorded at pairing
 * (ApproveResult.jkt). Pinning stops a compromised gateway from substituting
 * its own encryption key.
 */
import crypto from 'node:crypto';
// @ts-ignore engine is plain JS
import * as L from './engine/lmbp.js';

export const TYP_E2EE_KEY = 'lmbp-e2ee-key+jwt';
export const CTY_REQUEST = 'lmbp-request+json';

export interface E2EEFrame { seq: number; type: 'task.delta' | 'task.done'; data: string }
export interface E2EERequest {
  model: string; messages: unknown[]; tools?: unknown[]; tool_choice?: unknown; max_tokens?: number; temperature?: number;
  rid?: string; iat?: number; response_key?: string;
}
export interface E2EESealed {
  jwe: string; rid: string; responseKey: Buffer;
  /** Open one frame. Stateless: on its own it does NOT detect reordered, duplicated, dropped or truncated frames; use reader(). */
  open(f: E2EEFrame): Buffer;
  /** Stateful reader for this response (after = last seq already read, for a resumed stream). */
  reader(after?: number): E2EEReader;
}

/** Opens one response's sealed frames in order (spec §17.5): next() accepts only
 * seq last+1 of type task.delta/task.done and nothing after task.done;
 * finish() throws unless the sealed task.done was read. */
export interface E2EEReader { next(f: E2EEFrame): Buffer; finish(): void; readonly last: number; readonly done: boolean }

export function e2eeReader(sealed: Pick<E2EESealed, 'open'>, after = 0): E2EEReader {
  let last = after, done = false, err: Error | null = null;
  const fail = (m: string) => { err = new Error('lmbp e2ee: ' + m); throw err; };
  return {
    get last() { return last; },
    get done() { return done; },
    next(f) {
      if (err) throw err;
      if (done) fail(`frame seq ${f.seq} after task.done`);
      if (f.type !== 'task.delta' && f.type !== 'task.done') fail(`unexpected frame type ${JSON.stringify(f.type)}`);
      if (f.seq !== last + 1) fail(`frame seq ${f.seq}, want ${last + 1} (reordered, duplicated or dropped)`);
      let p: Buffer;
      try { p = sealed.open(f); } catch (e) { return fail(`frame seq ${f.seq}: ${(e as Error).message}`); }
      last = f.seq; done = f.type === 'task.done';
      return p;
    },
    finish() {
      if (err) throw err;
      if (!done) fail(`response ended after seq ${last} without the sealed task.done (truncated)`);
    },
  };
}
export interface E2EEDevice { deviceId: string; publicKey: Buffer; e2eeKey: string }

const b64 = (b: Uint8Array) => Buffer.from(b).toString('base64url');
const unb64 = (s: string) => Buffer.from(String(s), 'base64url');
export const x25519Thumbprint = (pub: Uint8Array) => b64(crypto.createHash('sha256').update(`{"crv":"X25519","kty":"OKP","x":"${b64(pub)}"}`).digest());
const xPub = (raw: Uint8Array) => crypto.createPublicKey({ format: 'jwk', key: { kty: 'OKP', crv: 'X25519', x: b64(raw) } });
const xPriv = (raw: Uint8Array) => crypto.createPrivateKey({ key: Buffer.concat([Buffer.from('302e020100300506032b656e04220420', 'hex'), raw]), format: 'der', type: 'pkcs8' });
const xPublicOf = (priv: crypto.KeyObject) => unb64((crypto.createPublicKey(priv).export({ format: 'jwk' }) as any).x);
function concatKdf(z: Buffer, enc = 'A256GCM') {
  const u32 = (n: number) => { const b = Buffer.alloc(4); b.writeUInt32BE(n); return b; };
  return crypto.createHash('sha256').update(Buffer.concat([u32(1), z, u32(enc.length), Buffer.from(enc), u32(0), u32(0), u32(256)])).digest();
}
const nonce = (seq: number) => { const n = Buffer.alloc(12); n.writeBigUInt64BE(BigInt(seq), 4); return n; };
const aad = (rid: string, type: string, seq: number) => Buffer.from(`lmbp-e2ee/v1|${rid}|${type}|${seq}`, 'utf8');

/** Verify a device key statement against the pinned identity thumbprint; returns the X25519 key and kid. */
export function verifyKeyStatement(devicePub: Uint8Array, pinnedJkt: string, deviceId: string, stmt: string, t = L.now()): { x: Buffer; kid: string } {
  if (!pinnedJkt) throw new Error('pinned device thumbprint required');
  if (L.thumbprint(devicePub) !== pinnedJkt) throw new Error('device key does not match pinned thumbprint');
  L.verifyE2EEKey(devicePub, deviceId, stmt, t);
  const k = JSON.parse(unb64(stmt.split('.')[1]).toString()).jwk;
  return { x: unb64(k.x), kid: k.kid };
}

export function sealJwe(recipient: Uint8Array, kid: string, plaintext: Uint8Array): string {
  const eph = crypto.generateKeyPairSync('x25519').privateKey;
  const z = crypto.diffieHellman({ privateKey: eph, publicKey: xPub(recipient) });
  const prot = b64(Buffer.from(JSON.stringify({ alg: 'ECDH-ES', enc: 'A256GCM', epk: { kty: 'OKP', crv: 'X25519', x: b64(xPublicOf(eph)) }, kid, cty: CTY_REQUEST })));
  const iv = crypto.randomBytes(12);
  const c = crypto.createCipheriv('aes-256-gcm', concatKdf(z), iv);
  c.setAAD(Buffer.from(prot, 'ascii'));
  const ct = Buffer.concat([c.update(plaintext), c.final()]);
  return `${prot}..${b64(iv)}.${b64(ct)}.${b64(c.getAuthTag())}`;
}

/** Decrypt one sealed response frame; fails if it was altered, reordered or belongs to another request. */
export function openChunk(key: Uint8Array, rid: string, type: string, seq: number, sealed: string): Buffer {
  const b = unb64(sealed);
  if (b.length < 16) throw new Error('short chunk');
  const d = crypto.createDecipheriv('aes-256-gcm', key, nonce(seq));
  d.setAAD(aad(rid, type, seq));
  d.setAuthTag(b.subarray(b.length - 16));
  return Buffer.concat([d.update(b.subarray(0, b.length - 16)), d.final()]);
}

/** Seal req to the device's X25519 key (verified against pinnedJkt). */
export function sealE2EERequest(dev: E2EEDevice, pinnedJkt: string, req: E2EERequest): E2EESealed {
  if (!dev.e2eeKey) throw new Error('device has no e2ee key');
  const { x, kid } = verifyKeyStatement(dev.publicKey, pinnedJkt, dev.deviceId, dev.e2eeKey);
  const responseKey = req.response_key && unb64(req.response_key).length === 32 ? unb64(req.response_key) : crypto.randomBytes(32);
  const r = { ...req, rid: req.rid || L.randomID('rid_'), iat: req.iat || L.now(), response_key: b64(responseKey) };
  const jwe = sealJwe(x, kid, Buffer.from(JSON.stringify(r)));
  const sealed: E2EESealed = {
    jwe, rid: r.rid, responseKey, open: (f) => openChunk(responseKey, r.rid, f.type, f.seq, f.data),
    reader: (after = 0) => e2eeReader(sealed, after),
  };
  return sealed;
}

/** Parse an opened task.delta frame ({ text?, tool_calls?, ... }). */
export const decodeE2EEDelta = (p: Uint8Array): { text?: string; tool_calls?: unknown[]; [k: string]: unknown } => JSON.parse(Buffer.from(p).toString());

// ---- device side (desktop apps / test devices) ----

/** Device: sign a key statement for its X25519 key (send as hello.e2ee_key). */
export function signKeyStatement(identityPriv: unknown, identityPub: Uint8Array, deviceId: string, xpub: Uint8Array, lifetime = 30 * 86400): string {
  const iat = L.now();
  return L.signJWS(identityPriv, TYP_E2EE_KEY, L.thumbprint(identityPub), { iss: deviceId, sub: deviceId, iat, exp: iat + lifetime,
    jwk: { kty: 'OKP', crv: 'X25519', x: b64(xpub), kid: x25519Thumbprint(xpub), use: 'enc', alg: 'ECDH-ES' } });
}
export function x25519KeyPair(): { priv: Buffer; pub: Buffer } {
  const priv = crypto.randomBytes(32);
  return { priv, pub: xPublicOf(xPriv(priv)) };
}
/** Device: open a sealed request. */
export function openJwe(privRaw: Uint8Array, jwe: string): { header: any; plaintext: Buffer } {
  const p = jwe.split('.');
  if (p.length !== 5 || p[1] !== '') throw new Error('malformed JWE');
  const h = JSON.parse(unb64(p[0]).toString('utf8'));
  if (h.alg !== 'ECDH-ES' || h.enc !== 'A256GCM' || 'zip' in h || 'crit' in h) throw new Error('unsupported JWE algorithm');
  if (h.epk?.kty !== 'OKP' || h.epk?.crv !== 'X25519') throw new Error('bad epk');
  const [iv, ct, tag] = [unb64(p[2]), unb64(p[3]), unb64(p[4])];
  if (iv.length !== 12 || tag.length !== 16) throw new Error('bad JWE encoding');
  const z = crypto.diffieHellman({ privateKey: xPriv(privRaw), publicKey: xPub(unb64(h.epk.x)) });
  const d = crypto.createDecipheriv('aes-256-gcm', concatKdf(z), iv);
  d.setAAD(Buffer.from(p[0], 'ascii'));
  d.setAuthTag(tag);
  return { header: h, plaintext: Buffer.concat([d.update(ct), d.final()]) };
}
/** Device: seal one response frame. */
export function sealChunk(key: Uint8Array, rid: string, type: string, seq: number, plaintext: Uint8Array): string {
  const c = crypto.createCipheriv('aes-256-gcm', key, nonce(seq));
  c.setAAD(aad(rid, type, seq));
  return b64(Buffer.concat([c.update(plaintext), c.final(), c.getAuthTag()]));
}
