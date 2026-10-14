// Audit finding 4: shared strict-JOSE vectors (spec/testvectors/jose.json).
// The SDK gateway verifies proofs, client assertions, access tokens and e2ee
// key statements; task_run/metadata are device-side and not implemented here.
import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import * as L from '../dist/engine/lmbp.js';

const F = JSON.parse(fs.readFileSync(new URL('../../../spec/testvectors/jose.json', import.meta.url)));
const pub = (k) => L.unb64(F.keys[k].public_b64url);
const KINDS = {
  proof: (v) => L.verifyProof(pub(v.verify_with_key), v.compact, v.context.iss, v.context.aud, v.context.credential, F.now),
  client_assertion: (v) => L.verifyJWT(pub(v.verify_with_key), v.compact, L.TYP.assertion, v.context.aud, F.now),
  access_token: (v) => L.verifyJWT(pub(v.verify_with_key), v.compact, L.TYP.access, v.context.aud, F.now),
  e2ee_key: (v) => L.verifyE2EEKey(pub(v.verify_with_key), v.context.device_id, v.compact, F.now),
};

test('jose vectors: valid accepted, negative rejected', () => {
  const bad = [];
  for (const v of F.valid) if (KINDS[v.kind]) { try { KINDS[v.kind](v); } catch (e) { bad.push(`valid ${v.name} rejected: ${e.message}`); } }
  let n = 0;
  for (const v of F.negative) if (KINDS[v.kind]) { n++; try { KINDS[v.kind](v); bad.push(`negative ${v.name} accepted (${v.must_reject_because})`); } catch { /* expected */ } }
  console.log(`checked ${n} negative vectors; ${bad.length} wrong`);
  assert.deepEqual(bad, []);
});
