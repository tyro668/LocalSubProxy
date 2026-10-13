// Run: npm install && npm test   (Node >= 18.4)
import { readFileSync, readdirSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { sign } from 'node:crypto';
import * as L from './lsp2.mjs';

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
    case 'client_assertion': { const c = JSON.parse(L.verifyJws(key, compact, L.TYP.assertion)); if (c.aud !== e.aud) throw new Error('aud'); return; }
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
const env = L.signTaskRun(priv('gateway'), 'k', tr.payload);
check('signTaskRun round trip', JSON.stringify(L.verifyTaskRun(pub('gateway'), env)) === JSON.stringify(tr.payload));

try {
  const { default: Ajv2020 } = await import('ajv/dist/2020.js');
  const ajv = new Ajv2020({ strict: false, allErrors: true });
  const sdir = join(ROOT, 'schemas');
  for (const f of readdirSync(sdir)) ajv.addSchema(JSON.parse(readFileSync(join(sdir, f), 'utf8')));
  const validate = ajv.getSchema('https://localsubproxy.dev/spec/lsp2/envelope.json');
  for (const f of V.valid_frames) check(`schema valid frame ${f.name}`, validate(f.frame), JSON.stringify(validate.errors));
  for (const f of V.invalid_frames) check(`schema rejects ${f.name}`, !validate(f.frame));
} catch (err) {
  if (err.code === 'ERR_MODULE_NOT_FOUND') console.log('SKIP schema checks (npm install)'); else throw err;
}
console.log(`\n${fails} failed`);
process.exit(fails ? 1 : 0);
