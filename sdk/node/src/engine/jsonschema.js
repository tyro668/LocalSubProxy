// Minimal JSON Schema (draft 2020-12) validator for the LMBP schemas
// (spec/schemas). It implements exactly the keywords those files use and
// FAILS CLOSED: compiling a schema with any other keyword throws, so a
// constraint is never silently ignored. Lengths count Unicode code points
// (as the specification requires). validate() returns null or the first
// error { path: [...instance location], message }.
const ANNOTATIONS = new Set(['$schema', '$id', '$defs', 'title', 'description', 'default', '$comment', 'examples']);
const KEYWORDS = new Set(['$ref', 'type', 'required', 'properties', 'additionalProperties', 'items', 'minItems', 'maxItems',
  'minimum', 'maximum', 'minLength', 'maxLength', 'enum', 'const', 'pattern', 'oneOf', 'anyOf', 'allOf', 'if', 'then', 'else', 'uniqueItems']);

const typeOf = (v) => (v === null ? 'null' : Array.isArray(v) ? 'array' : typeof v === 'number' ? (Number.isInteger(v) ? 'integer' : 'number') : typeof v);
const isType = (v, t) => { const a = typeOf(v); return a === t || (t === 'number' && a === 'integer'); };
function deepEqual(a, b) {
  if (a === b) return true;
  if (typeOf(a) !== typeOf(b) && !(typeof a === 'number' && typeof b === 'number')) return false;
  if (Array.isArray(a)) return a.length === b.length && a.every((x, i) => deepEqual(x, b[i]));
  if (a && typeof a === 'object') {
    const ka = Object.keys(a), kb = Object.keys(b);
    return ka.length === kb.length && ka.every((k) => Object.hasOwn(b, k) && deepEqual(a[k], b[k]));
  }
  return false;
}

export class SchemaSet {
  /** docs: schema documents with $id; base: the common $id prefix. */
  constructor(docs) {
    this.docs = new Map();
    for (const d of docs) { this.check(d); this.docs.set(d.$id, d); }
    this.patterns = new Map();
  }
  check(s) {
    if (typeof s !== 'object' || s === null) return;
    for (const [k, v] of Object.entries(s)) {
      if (ANNOTATIONS.has(k)) { if (k === '$defs') for (const x of Object.values(v)) this.check(x); continue; }
      if (!KEYWORDS.has(k)) throw new Error(`jsonschema: unsupported keyword ${k}`);
      if (k === 'properties') for (const x of Object.values(v)) this.check(x);
      else if (['items', 'additionalProperties', 'if', 'then', 'else'].includes(k)) this.check(v);
      else if (['oneOf', 'anyOf', 'allOf'].includes(k)) v.forEach((x) => this.check(x));
    }
  }
  resolve(ref, docID) {
    const [file, frag = ''] = ref.split('#');
    const id = file ? new URL(file, docID).href : docID;
    const doc = this.docs.get(id);
    if (!doc) throw new Error(`jsonschema: unknown $ref ${ref}`);
    let s = doc;
    for (const p of frag.split('/').filter(Boolean)) s = s[decodeURIComponent(p)];
    if (s === undefined) throw new Error(`jsonschema: unresolvable $ref ${ref}`);
    return { s, docID: id };
  }
  validate(id, value) { const d = this.docs.get(id); if (!d) throw new Error('jsonschema: unknown schema ' + id); return this.v(d, value, [], id); }
  re(p) { let r = this.patterns.get(p); if (!r) { r = new RegExp(p, 'u'); this.patterns.set(p, r); } return r; }
  v(s, x, path, docID) {
    const err = (message) => ({ path, message });
    if (s.$ref) { const r = this.resolve(s.$ref, docID); const e = this.v(r.s, x, path, r.docID); if (e) return e; }
    if (s.type !== undefined) {
      const ts = Array.isArray(s.type) ? s.type : [s.type];
      if (!ts.some((t) => isType(x, t))) return err(`must be ${ts.join(' or ')}`);
    }
    if (s.const !== undefined && !deepEqual(x, s.const)) return err(`must be ${JSON.stringify(s.const)}`);
    if (s.enum && !s.enum.some((e) => deepEqual(e, x))) return err(`must be one of ${JSON.stringify(s.enum)}`);
    if (typeof x === 'number') {
      if (s.minimum !== undefined && x < s.minimum) return err(`must be >= ${s.minimum}`);
      if (s.maximum !== undefined && x > s.maximum) return err(`must be <= ${s.maximum}`);
    }
    if (typeof x === 'string') {
      const n = [...x].length;
      if (s.minLength !== undefined && n < s.minLength) return err(`must have at least ${s.minLength} characters`);
      if (s.maxLength !== undefined && n > s.maxLength) return err(`must have at most ${s.maxLength} characters`);
      if (s.pattern !== undefined && !this.re(s.pattern).test(x)) return err(`must match ${s.pattern}`);
    }
    if (Array.isArray(x)) {
      if (s.minItems !== undefined && x.length < s.minItems) return err(`must have at least ${s.minItems} items`);
      if (s.maxItems !== undefined && x.length > s.maxItems) return err(`must have at most ${s.maxItems} items`);
      if (s.uniqueItems && x.some((a, i) => x.slice(0, i).some((b) => deepEqual(a, b)))) return err('items must be unique');
      if (s.items) for (let i = 0; i < x.length; i++) { const e = this.v(s.items, x[i], [...path, i], docID); if (e) return e; }
    }
    if (x && typeof x === 'object' && !Array.isArray(x)) {
      for (const k of s.required ?? []) if (!Object.hasOwn(x, k)) return err(`missing required property ${k}`);
      for (const [k, val] of Object.entries(x)) {
        if (s.properties && Object.hasOwn(s.properties, k)) { const e = this.v(s.properties[k], val, [...path, k], docID); if (e) return e; }
        else if (s.additionalProperties === false) return { path: [...path, k], message: 'is not allowed' };
        else if (s.additionalProperties && typeof s.additionalProperties === 'object') { const e = this.v(s.additionalProperties, val, [...path, k], docID); if (e) return e; }
      }
    }
    for (const sub of s.allOf ?? []) { const e = this.v(sub, x, path, docID); if (e) return e; }
    if (s.anyOf && !s.anyOf.some((sub) => !this.v(sub, x, path, docID))) return err('does not match any allowed form');
    if (s.oneOf && s.oneOf.filter((sub) => !this.v(sub, x, path, docID)).length !== 1) return err('must match exactly one allowed form');
    if (s.if) {
      const branch = this.v(s.if, x, path, docID) ? s.else : s.then;
      if (branch) { const e = this.v(branch, x, path, docID); if (e) return e; }
    }
    return null;
  }
}
