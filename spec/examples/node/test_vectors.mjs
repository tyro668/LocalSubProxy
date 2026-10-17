// Run: npm install && npm test   (Node >= 18.4)
import { readFileSync, readdirSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { sign } from 'node:crypto';
import * as L from './lmbp.mjs';

const ROOT = join(dirname(fileURLToPath(import.meta.url)), '..', '..');
const V = JSON.parse(readFileSync(join(ROOT, 'testvectors', 'vectors.json'), 'utf8'));
const NOW = V.now;
let fails = 0;
const check = (name, ok, detail = '') => { console.log(`${ok ? 'PASS' : 'FAIL'} ${name}${!ok && detail ? ' ' + detail : ''}`); if (!ok) fails++; };
const pub = (n) => L.unb64u(V.keys[n].public_b64url);
const priv = (n) => L.privateKeyFromSeed(L.unb64u(V.keys[n].seed_b64url), pub(n));

for (const [n, k] of Object.entries(V.keys)) {
  const raw = L.unb64u(priv(n).export({ format: 'jwk' }).x);
  check(`key ${n}: seed→public, RFC 7638 thumbprint`, L.b64u(raw) === k.public_b64url && L.thumbprint(raw) === k.jkt);
}
for (const c of V.base64url) check(`base64url ${c.bytes_hex || '(empty)'}`, L.b64u(Buffer.from(c.bytes_hex, 'hex')) === c.b64url);
for (const c of V.ath) check('ath', L.ath(c.credential) === c.ath);

function doVerify(kind, compact, key, e) {
  switch (kind) {
    case 'access_token': { const c = L.verifyAccessToken(key, compact, e.aud, NOW); if (c.cnf.jkt !== e['cnf.jkt']) throw new Error('jkt'); return; }
    case 'client_assertion': L.verifyClientAssertion(key, compact, e.aud, NOW); return;
    case 'proof': L.verifyProof(key, compact, e, NOW); return;
    case 'task_run': L.verifyTaskRun(key, { task_id: e.envelope_task_id, jws: compact }); return;
    default: throw new Error('kind');
  }
}
for (const j of V.jws) {
  check(`jws ${j.name}: deterministic Ed25519 signature`, L.b64u(sign(null, Buffer.from(j.signing_input), priv(j.signed_by))) === j.signature_b64url);
  try { doVerify(j.kind, j.compact, pub(j.signed_by), j.verify_with); check(`jws ${j.name}: verifies`, true); }
  catch (err) { check(`jws ${j.name}: verifies`, false, String(err)); }
}
for (const n of V.negative) {
  let accepted = false;
  try { doVerify(n.kind, n.compact, pub(n.verify_with_key), n.verify_with); accepted = true; } catch { /* expected */ }
  check(`negative ${n.name}: rejected`, !accepted);
}
const at = V.jws.find((j) => j.name === 'access_token');
const p = at.payload;
const mine = L.issueAccessToken(priv('gateway'), at.protected_header.kid, { deviceId: p.sub, tenant: p.tenant, devicePub: pub('device'), ttl: p.exp - p.iat, now: p.iat, jti: p.jti });
check('issueAccessToken == reference compact', mine === at.compact, mine);
const hello = V.valid_frames.find((f) => f.name === 'hello').frame;
try { L.verifyHello(pub('gateway'), () => pub('device'), hello, at.compact, { now: NOW }); check('verifyHello(vector hello)', true); }
catch (err) { check('verifyHello(vector hello)', false, String(err)); }
const tr = V.jws.find((j) => j.name === 'task_run');
const env = L.signTaskRun(priv('gateway'), L.thumbprint(pub('gateway')), tr.payload);
check('signTaskRun round trip', JSON.stringify(L.verifyTaskRun(pub('gateway'), env)) === JSON.stringify(tr.payload));

try {
  const { default: Ajv2020 } = await import('ajv/dist/2020.js');
  const ajv = new Ajv2020({ strict: false, allErrors: true });
  const sdir = join(ROOT, 'schemas');
  for (const f of readdirSync(sdir).filter((n) => n.endsWith('.json'))) ajv.addSchema(JSON.parse(readFileSync(join(sdir, f), 'utf8')));
  const validate = ajv.getSchema('https://localsubproxy.dev/spec/lmbp/envelope.json');
  for (const f of V.valid_frames) check(`schema valid frame ${f.name}`, validate(f.frame), JSON.stringify(validate.errors));
  for (const f of V.invalid_frames) check(`schema rejects ${f.name}`, !validate(f.frame));
} catch (err) {
  if (err.code !== 'ERR_MODULE_NOT_FOUND') throw err;
  // In CI / LMBP_REQUIRE_EXTERNAL mode a skipped check is a failure.
  const strict = ['CI', 'LMBP_REQUIRE_EXTERNAL'].some((k) => process.env[k] && !['0', 'false'].includes(process.env[k]));
  if (strict) { console.error('FAIL schema checks need ajv (npm install); skipping is not allowed in CI mode'); process.exit(1); }
  console.log('SKIP schema checks (npm install)');
}
// ---- extensions: E2EE, flow control, resume ----
const X = await import('./lmbp_ext.mjs');
const E = V.e2ee;
try {
  const ks = E.key_statement;
  const r = X.verifyKeyStatement(pub('device'), ks.pinned_jkt, ks.device_id, ks.compact, NOW);
  check('e2ee key statement verifies against pinned jkt', L.b64u(r.x) === ks.expect_x25519_public_b64url && r.kid === ks.expect_kid);
} catch (err) { check('e2ee key statement verifies against pinned jkt', false, String(err)); }
const xpriv = L.unb64u(E.device_x25519.private_b64url);
check('e2ee X25519 public + RFC 7638 kid', L.b64u(X.xPublicOf(X.xPriv(xpriv))) === E.device_x25519.public_b64url && X.x25519Thumbprint(L.unb64u(E.device_x25519.public_b64url)) === E.device_x25519.kid);
const J = E.jwe;
check('e2ee Concat KDF reproduces cek', L.b64u(X.concatKdf(L.unb64u(J.shared_secret_b64url))) === J.cek_b64url);
const mineJwe = X.sealJwe(L.unb64u(E.device_x25519.public_b64url), J.recipient_kid, Buffer.from(J.plaintext_utf8, 'utf8'), L.unb64u(J.ephemeral_private_b64url), L.unb64u(J.iv_b64url));
check('e2ee sealJwe with vector ephemeral key/IV == reference compact', mineJwe === J.compact, mineJwe);
try {
  const o = X.openJwe(xpriv, J.compact);
  check('e2ee openJwe(reference JWE)', o.plaintext.toString('utf8') === J.plaintext_utf8 && o.header.cty === X.CTY_REQUEST);
} catch (err) { check('e2ee openJwe(reference JWE)', false, String(err)); }
for (const c of E.chunks) {
  const k = L.unb64u(c.response_key_b64url);
  check(`e2ee chunk seq ${c.seq} ${c.type}: seal matches`, X.sealChunk(k, c.rid, c.type, c.seq, Buffer.from(c.plaintext_utf8, 'utf8')) === c.sealed_b64url);
  check(`e2ee chunk seq ${c.seq}: open`, X.openChunk(k, c.rid, c.type, c.seq, c.sealed_b64url).toString('utf8') === c.plaintext_utf8);
}
const k0 = L.unb64u(E.chunks[0].response_key_b64url);
for (const n of E.negative) {
  try {
    if (n.kind === 'jwe') X.openJwe(xpriv, n.compact);
    else if (n.kind === 'chunk') X.openChunk(k0, n.rid, n.type, n.seq, n.sealed_b64url);
    else X.verifyKeyStatement(pub('device'), n.pinned_jkt, n.device_id, n.compact, NOW);
    check(`e2ee negative ${n.name}: rejected`, false, 'accepted');
  } catch { check(`e2ee negative ${n.name}: rejected`, true); }
}
// ---- spec/testvectors/jose.json: every verify path, strict §5.2 rules ----
const JV = JSON.parse(readFileSync(join(ROOT, 'testvectors', 'jose.json'), 'utf8'));
const jpub = (n) => L.unb64u(JV.keys[n].public_b64url);
function joseVerify(v) {
  const c = v.context, k = jpub(v.verify_with_key), now = JV.now;
  switch (v.kind) {
    case 'proof': return L.verifyProof(k, v.compact, c, now);
    case 'client_assertion': return L.verifyClientAssertion(k, v.compact, c.aud, now);
    case 'access_token': return L.verifyAccessToken(k, v.compact, c.aud, now);
    case 'e2ee_key': return X.verifyKeyStatement(k, '', c.device_id, v.compact, now);
    case 'task_run': return L.verifyTaskRun(k, { task_id: c.envelope_task_id, jws: v.compact }, now);
    case 'metadata': return L.verifyMetadata(k, v.compact, c.issuer, c.tenant, now);
    default: throw new Error(`unknown kind ${v.kind}`);
  }
}
for (const v of JV.valid) { try { joseVerify(v); check(`jose valid ${v.name}`, true); } catch (err) { check(`jose valid ${v.name}`, false, String(err)); } }
for (const v of JV.negative) { let ok = false; try { joseVerify(v); } catch { ok = true; } check(`jose negative ${v.name}: rejected`, ok, v.must_reject_because); }
check('jose: all cases ran', JV.valid.length > 0 && JV.negative.length > 200);
for (const f of V.flow) {
  let ok = true;
  if (f.role === 'sender') {
    const sent = new Map();
    for (const st of f.steps) {
      if (st.op === 'send') {
        const tu = (sent.get(st.task) ?? []).reduce((a, [, z]) => a + z, 0);
        const cu = [...sent.values()].flat().reduce((a, [, z]) => a + z, 0);
        const can = X.canSend(f.task_window, f.conn_window, tu, cu, st.size);
        ok &&= can === (st.expect === 'send');
        if (can) { const l = sent.get(st.task) ?? []; l.push([(l.at(-1)?.[0] ?? 0) + 1, st.size]); sent.set(st.task, l); }
      } else sent.set(st.task, (sent.get(st.task) ?? []).filter(([s]) => s > st.seq));
    }
  } else {
    const inb = new X.Inbound(f.task_window, f.conn_window);
    for (const st of f.steps) { if (st.op === 'recv') ok &&= inb.accept(st.task, st.seq, st.size) === st.expect; else inb.acked(st.task, st.seq); }
  }
  check(`flow ${f.name}`, ok);
}
for (const r of V.resume) check(`resume ${r.name}`, JSON.stringify(X.retransmit(r.sent.filter((s) => s > r.acked), r.peer_recv_seq)) === JSON.stringify(r.retransmit));
console.log(`\n${fails} failed`);
process.exit(fails ? 1 : 0);
