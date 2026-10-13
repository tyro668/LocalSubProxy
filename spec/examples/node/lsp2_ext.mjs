// lsp2 extensions for a Node.js implementation: E2EE (JWE ECDH-ES/X25519/A256GCM,
// sealed result frames, key statements) and flow-control / resume bookkeeping.
// Only node:crypto is needed (X25519 + AES-GCM since Node 12/15). The `jose`
// package (panva) can also open the JWE: compactDecrypt(jwe, key,
// { keyManagementAlgorithms: ['ECDH-ES'], contentEncryptionAlgorithms: ['A256GCM'] }).
import { createCipheriv, createDecipheriv, createHash, createPrivateKey, createPublicKey, diffieHellman, generateKeyPairSync, randomBytes } from 'node:crypto';
import { b64u, unb64u, jsonBytes, thumbprint, verifyJws } from './lsp2.mjs';

export const TYP_E2EE_KEY = 'lsp2-e2ee-key+jwt';
export const CTY_REQUEST = 'lsp2-request+json';

export const x25519Thumbprint = (pub) => b64u(createHash('sha256').update(`{"crv":"X25519","kty":"OKP","x":"${b64u(pub)}"}`).digest());
const xPub = (raw) => createPublicKey({ format: 'jwk', key: { kty: 'OKP', crv: 'X25519', x: b64u(raw) } });
export function xPriv(raw) {
  // Derive x from d by importing via PKCS#8 (RFC 8410 prefix for X25519).
  const der = Buffer.concat([Buffer.from('302e020100300506032b656e04220420', 'hex'), raw]);
  return createPrivateKey({ key: der, format: 'der', type: 'pkcs8' });
}
export const xPublicOf = (priv) => unb64u(createPublicKey(priv).export({ format: 'jwk' }).x);

export function verifyKeyStatement(devicePub, pinnedJkt, deviceId, stmt, now) {
  if (thumbprint(devicePub) !== pinnedJkt) throw new Error('device key does not match pinned thumbprint');
  const c = JSON.parse(verifyJws(devicePub, stmt, TYP_E2EE_KEY).toString('utf8'));
  const j = c.jwk ?? {};
  if (c.iss !== deviceId || c.sub !== deviceId || j.kty !== 'OKP' || j.crv !== 'X25519') throw new Error('bad key statement');
  if (now > c.exp || now + 60 < c.iat) throw new Error('key statement expired');
  const x = unb64u(j.x);
  if (x.length !== 32 || j.kid !== x25519Thumbprint(x)) throw new Error('bad e2ee key');
  return { x, kid: j.kid };
}

export function concatKdf(z, enc = 'A256GCM') {
  const u32 = (n) => { const b = Buffer.alloc(4); b.writeUInt32BE(n); return b; };
  return createHash('sha256').update(Buffer.concat([u32(1), z, u32(enc.length), Buffer.from(enc), u32(0), u32(0), u32(256)])).digest();
}

export function sealJwe(recipient, kid, plaintext, ephPrivRaw, iv = randomBytes(12)) {
  const eph = ephPrivRaw ? xPriv(ephPrivRaw) : generateKeyPairSync('x25519').privateKey;
  const z = diffieHellman({ privateKey: eph, publicKey: xPub(recipient) });
  const hdr = { alg: 'ECDH-ES', enc: 'A256GCM', epk: { kty: 'OKP', crv: 'X25519', x: b64u(xPublicOf(eph)) }, kid, cty: CTY_REQUEST };
  const prot = b64u(jsonBytes(hdr));
  const c = createCipheriv('aes-256-gcm', concatKdf(z), iv);
  c.setAAD(Buffer.from(prot, 'ascii'));
  const ct = Buffer.concat([c.update(plaintext), c.final()]);
  return `${prot}..${b64u(iv)}.${b64u(ct)}.${b64u(c.getAuthTag())}`;
}

export function openJwe(privRaw, jwe) {
  const p = jwe.split('.');
  if (p.length !== 5 || p[1] !== '') throw new Error('malformed JWE');
  const h = JSON.parse(unb64u(p[0]).toString('utf8'));
  if (h.alg !== 'ECDH-ES' || h.enc !== 'A256GCM' || 'zip' in h || 'crit' in h) throw new Error('unsupported JWE algorithm');
  if (h.epk?.kty !== 'OKP' || h.epk?.crv !== 'X25519') throw new Error('bad epk');
  const [iv, ct, tag] = [unb64u(p[2]), unb64u(p[3]), unb64u(p[4])];
  if (iv.length !== 12 || tag.length !== 16) throw new Error('bad JWE encoding');
  const z = diffieHellman({ privateKey: xPriv(privRaw), publicKey: xPub(unb64u(h.epk.x)) });
  const d = createDecipheriv('aes-256-gcm', concatKdf(z), iv);
  d.setAAD(Buffer.from(p[0], 'ascii'));
  d.setAuthTag(tag);
  return { header: h, plaintext: Buffer.concat([d.update(ct), d.final()]) };
}

export function chunkNonce(seq) { const n = Buffer.alloc(12); n.writeBigUInt64BE(BigInt(seq), 4); return n; }
export const chunkAad = (rid, type, seq) => Buffer.from(`lsp2-e2ee/v1|${rid}|${type}|${seq}`, 'utf8');

export function sealChunk(key, rid, type, seq, plaintext) {
  const c = createCipheriv('aes-256-gcm', key, chunkNonce(seq));
  c.setAAD(chunkAad(rid, type, seq));
  return b64u(Buffer.concat([c.update(plaintext), c.final(), c.getAuthTag()]));
}

export function openChunk(key, rid, type, seq, sealed) {
  const b = unb64u(sealed);
  if (b.length < 16) throw new Error('short chunk');
  const d = createDecipheriv('aes-256-gcm', key, chunkNonce(seq));
  d.setAAD(chunkAad(rid, type, seq));
  d.setAuthTag(b.subarray(b.length - 16));
  return Buffer.concat([d.update(b.subarray(0, b.length - 16)), d.final()]);
}

// ---- flow control (§15) ----
export const canSend = (tw, cw, taskUsed, connUsed, n) => (taskUsed + n <= tw || taskUsed === 0) && (connUsed + n <= cw || connUsed === 0);

export class Inbound {
  constructor(tw, cw) { Object.assign(this, { tw, cw, connUsed: 0, tasks: new Map() }); }
  accept(task, seq, size) {
    let t = this.tasks.get(task);
    if (!t) this.tasks.set(task, (t = { recv: 0, sizes: [], used: 0 }));
    if (seq <= t.recv) return 'duplicate';
    if (seq !== t.recv + 1) return 'gap';
    if ((t.used + size > this.tw && t.used > 0) || (this.connUsed + size > this.cw && this.connUsed > 0)) return 'overflow';
    t.recv = seq; t.sizes.push([seq, size]); t.used += size; this.connUsed += size;
    return 'accepted';
  }
  acked(task, seq) {
    const t = this.tasks.get(task);
    while (t && t.sizes.length && t.sizes[0][0] <= seq) { const [, n] = t.sizes.shift(); t.used -= n; this.connUsed -= n; }
  }
}

export const retransmit = (unacked, peerRecv) => unacked.filter((s) => s > peerRecv);
