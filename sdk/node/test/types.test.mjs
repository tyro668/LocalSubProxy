// B1: public request types are driven by the spec: every property of
// ChatRequest / MessagesRequest in spec/openapi.yaml is a member of the
// exported TypeScript interface (checked with the TypeScript compiler API).
import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import ts from 'typescript';

const here = path.dirname(fileURLToPath(import.meta.url));
export function openapiProps(schema) {
  const lines = fs.readFileSync(path.join(here, '../../../spec/openapi.yaml'), 'utf8').split('\n');
  const out = []; let inS = false, props = false;
  for (const l of lines) {
    const ind = l.length - l.trimStart().length;
    if (l === `    ${schema}:`) { inS = true; continue; }
    if (!inS) continue;
    if (l.trim() && ind <= 4) break;
    if (l === '      properties:') { props = true; continue; }
    if (props && l.trim() && ind <= 6) props = false;
    else if (props && ind === 8 && l.includes(':')) out.push(l.trim().split(':')[0]);
  }
  assert.ok(out.length, 'no properties for ' + schema);
  return out;
}

function members(name) {
  const file = path.join(here, '../src/types.ts');
  const prog = ts.createProgram([file], { strict: true });
  const tc = prog.getTypeChecker();
  const sf = prog.getSourceFile(file);
  const sym = tc.getExportsOfModule(tc.getSymbolAtLocation(sf)).find((s) => s.name === name);
  assert.ok(sym, 'no export ' + name);
  return new Set(tc.getDeclaredTypeOfSymbol(sym).getProperties().map((p) => p.name));
}

for (const schema of ['ChatRequest', 'MessagesRequest']) {
  test(`${schema} covers every spec property`, () => {
    const have = members(schema);
    const missing = openapiProps(schema).filter((p) => !have.has(p));
    assert.deepEqual(missing, [], `${schema} lacks spec properties`);
  });
}
