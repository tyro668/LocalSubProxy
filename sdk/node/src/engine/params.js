// Request parameter validation (third audit #2), the same rules as the Go
// reference (internal/lmbp/gateway/params.go) and the Python SDK, pinned by
// spec/testvectors/params.json and conformance Z8. Every parameter is checked
// before anything is stored, queued or signed; the payload to be signed is
// then validated against the bundled copy of spec/schemas/task.run.payload.json.
// Illegal values: 400 BAD_REQUEST with the top-level parameter in
// error.param (OpenAI shape) / error.lmbp_param (Anthropic shape).
import fs from 'node:fs';
import { SchemaSet } from './jsonschema.js';
import { MAX_MESSAGES } from './lmbp.js';

export const MAX_STOP_SEQUENCES = 4;
export const MAX_STOP_LENGTH = 256;
export const MAX_TOOLS = 128;
const TOOL_NAME = /^[A-Za-z0-9_-]{1,64}$/;
const BASE = 'https://localsubproxy.dev/spec/lmbp/';

let schemas;
function schemaSet() {
  if (!schemas) {
    const load = (n) => JSON.parse(fs.readFileSync(new URL(`../../schemas/${n}`, import.meta.url), 'utf8'));
    schemas = new SchemaSet([load('defs.json'), load('task.run.payload.json'), load('hello.json'), load('models.update.json'), load('task.done.json'), load('task.error.json')]);
  }
  return schemas;
}

export class ParamError extends Error {
  constructor(param, message) { super(message); this.name = 'ParamError'; this.param = param; }
}
const perr = (param, msg) => new ParamError(param, `${param}: ${msg}`);

/** paramErrorBody is the 400 BAD_REQUEST body in the endpoint's error shape. */
export function paramErrorBody(e, anthropic) {
  return anthropic
    ? { type: 'error', error: { type: 'invalid_request_error', message: e.message, lmbp_code: 'BAD_REQUEST', lmbp_param: e.param } }
    : { error: { message: e.message, type: 'invalid_request_error', code: 'BAD_REQUEST', param: e.param } };
}
const present = (m, k) => Object.hasOwn(m, k) && m[k] !== null && m[k] !== undefined;
const isObj = (x) => x !== null && typeof x === 'object' && !Array.isArray(x);
const fmt = (n) => String(n);

function numberIn(m, k, lo, hi) {
  if (!present(m, k)) return undefined;
  const f = m[k];
  if (typeof f !== 'number' || !Number.isFinite(f)) throw perr(k, 'must be a number');
  if (f < lo || f > hi) throw perr(k, `must be between ${fmt(lo)} and ${fmt(hi)}, got ${fmt(f)}`);
  return f;
}
function positiveInt(m, k) {
  if (!present(m, k)) return 0;
  const f = m[k];
  if (typeof f !== 'number' || !Number.isInteger(f) || f < 1 || f > 2147483647) throw perr(k, `must be a positive integer, got ${JSON.stringify(f)}`);
  return f;
}
function boolean(m, k) { if (present(m, k) && typeof m[k] !== 'boolean') throw perr(k, 'must be a boolean'); }
function stopList(m, k, allowString) {
  if (!present(m, k)) return undefined;
  let list = m[k];
  if (typeof list === 'string') {
    if (!allowString) throw perr(k, 'must be an array of strings');
    list = [list];
  } else if (!Array.isArray(list)) throw perr(k, allowString ? 'must be a string or an array of strings' : 'must be an array of strings');
  if (list.length < 1 || list.length > MAX_STOP_SEQUENCES) throw perr(k, `must have 1 to ${MAX_STOP_SEQUENCES} sequences, got ${list.length}`);
  list.forEach((s, i) => {
    if (typeof s !== 'string' || s === '' || [...s].length > MAX_STOP_LENGTH) throw perr(k, `sequence ${i} must be a non-empty string of at most ${MAX_STOP_LENGTH} characters`);
  });
  return [...list];
}

/** validateChatParams checks an OpenAI chat body; returns normalized sampling params or throws ParamError. */
export function validateChatParams(m) {
  if (typeof m.model !== 'string' || !m.model) throw perr('model', 'field required (non-empty string)');
  boolean(m, 'stream');
  const p = {};
  p.temperature = numberIn(m, 'temperature', 0, 2);
  p.top_p = numberIn(m, 'top_p', 0, 1);
  const mt = positiveInt(m, 'max_tokens');
  const mct = positiveInt(m, 'max_completion_tokens');
  p.max_tokens = mt || mct;
  if (present(m, 'n')) {
    if (typeof m.n !== 'number' || !Number.isInteger(m.n)) throw perr('n', 'must be an integer');
    if (m.n !== 1) throw perr('n', `only n=1 is supported, got ${m.n}`);
  }
  p.stop = stopList(m, 'stop', true);
  if (!Object.hasOwn(m, 'lmbp_e2ee')) validateOpenAIMessages(m);
  const names = validateOpenAITools(m);
  validateOpenAIToolChoice(m, names);
  return p;
}

function validateOpenAIMessages(m) {
  if (!present(m, 'messages')) throw perr('messages', 'field required');
  const list = m.messages;
  if (!Array.isArray(list)) throw perr('messages', 'must be an array');
  if (list.length === 0 || list.length > MAX_MESSAGES) throw perr('messages', `1-${MAX_MESSAGES} messages required, got ${list.length}`);
  list.forEach((msg, i) => {
    if (!isObj(msg)) throw perr('messages', `message ${i} must be an object`);
    if (!['system', 'user', 'assistant', 'tool', 'developer'].includes(msg.role)) throw perr('messages', `message ${i}: unsupported role ${JSON.stringify(msg.role)}`);
    const c = msg.content;
    if (!(c === undefined || c === null || typeof c === 'string' || Array.isArray(c))) throw perr('messages', `message ${i}: content must be a string, an array of content parts or null`);
    if (msg.role === 'tool') {
      if (typeof msg.tool_call_id !== 'string' || !msg.tool_call_id) throw perr('messages', `message ${i}: tool messages need tool_call_id`);
      if (c === undefined || c === null) throw perr('messages', `message ${i}: tool messages need content`);
    }
    if (msg.tool_calls !== undefined && msg.tool_calls !== null) {
      if (!Array.isArray(msg.tool_calls) || msg.role !== 'assistant') throw perr('messages', `message ${i}: tool_calls must be an array on an assistant message`);
      msg.tool_calls.forEach((tc, j) => { if (!isObj(tc?.function)) throw perr('messages', `message ${i}: tool_calls[${j}] needs a function`); });
    }
  });
}

function validateOpenAITools(m) {
  const names = new Set();
  if (!present(m, 'tools')) return names;
  if (!Array.isArray(m.tools)) throw perr('tools', 'must be an array');
  if (m.tools.length > MAX_TOOLS) throw perr('tools', `at most ${MAX_TOOLS} tools, got ${m.tools.length}`);
  m.tools.forEach((t, i) => {
    if (!isObj(t) || t.type !== 'function') throw perr('tools', `tool ${i} must have type function`);
    const fn = t.function;
    if (!isObj(fn)) throw perr('tools', `tool ${i} needs a function object`);
    if (typeof fn.name !== 'string' || !TOOL_NAME.test(fn.name)) throw perr('tools', `tool ${i}: function name must match ^[A-Za-z0-9_-]{1,64}$, got ${JSON.stringify(fn.name)}`);
    if (present(fn, 'description') && typeof fn.description !== 'string') throw perr('tools', `tool ${i}: description must be a string`);
    if (present(fn, 'parameters') && !isObj(fn.parameters)) throw perr('tools', `tool ${i}: parameters must be a JSON Schema object`);
    names.add(fn.name);
  });
  return names;
}

function validateOpenAIToolChoice(m, names) {
  if (!present(m, 'tool_choice')) return;
  const v = m.tool_choice;
  if (typeof v === 'string') {
    if (v === 'none') return;
    if (v !== 'auto' && v !== 'required') throw perr('tool_choice', `must be none, auto, required or a function object, got ${JSON.stringify(v)}`);
  } else if (isObj(v)) {
    const name = v.function?.name;
    if (v.type !== 'function' || typeof name !== 'string' || !name) throw perr('tool_choice', 'object form must be {"type":"function","function":{"name":...}}');
    if (!names.has(name)) throw perr('tool_choice', `names function ${JSON.stringify(name)} that is not in tools`);
  } else throw perr('tool_choice', 'must be a string or an object');
  if (names.size === 0) throw perr('tool_choice', 'requires tools');
}

/** validateAnthropicParams checks a Messages body with Anthropic's names and ranges; throws ParamError. */
export function validateAnthropicParams(m) {
  if (typeof m.model !== 'string' || !m.model) throw perr('model', 'field required (non-empty string)');
  if (!present(m, 'max_tokens')) throw perr('max_tokens', 'field required (positive integer)');
  positiveInt(m, 'max_tokens');
  boolean(m, 'stream');
  numberIn(m, 'temperature', 0, 1);
  numberIn(m, 'top_p', 0, 1);
  stopList(m, 'stop_sequences', false);
  if (!present(m, 'messages')) throw perr('messages', 'field required');
  if (!Array.isArray(m.messages)) throw perr('messages', 'must be an array');
  if (m.messages.length === 0 || m.messages.length > MAX_MESSAGES) throw perr('messages', `1-${MAX_MESSAGES} messages required, got ${m.messages.length}`);
  m.messages.forEach((msg, i) => {
    if (!isObj(msg)) throw perr('messages', `message ${i} must be an object`);
    if (msg.role !== 'user' && msg.role !== 'assistant') throw perr('messages', `message ${i}: role must be user or assistant`);
    if (typeof msg.content !== 'string' && !Array.isArray(msg.content)) throw perr('messages', `message ${i}: content must be a string or an array of blocks`);
  });
  const names = new Set();
  if (present(m, 'tools')) {
    if (!Array.isArray(m.tools)) throw perr('tools', 'must be an array');
    if (m.tools.length > MAX_TOOLS) throw perr('tools', `at most ${MAX_TOOLS} tools, got ${m.tools.length}`);
    m.tools.forEach((t, i) => {
      if (!isObj(t)) throw perr('tools', `tool ${i} must be an object`);
      if (present(t, 'type') && t.type !== 'custom') throw perr('tools', `tool ${i}: server tool ${t.type} is not supported`);
      if (typeof t.name !== 'string' || !TOOL_NAME.test(t.name)) throw perr('tools', `tool ${i}: name must match ^[A-Za-z0-9_-]{1,64}$, got ${JSON.stringify(t.name)}`);
      if (!isObj(t.input_schema)) throw perr('tools', `tool ${i}: input_schema (object) is required`);
      names.add(t.name);
    });
  }
  if (present(m, 'tool_choice')) {
    const tc = m.tool_choice;
    if (!isObj(tc)) throw perr('tool_choice', 'must be an object');
    if (tc.type === 'none') return;
    if (tc.type === 'tool') {
      if (typeof tc.name !== 'string' || !tc.name) throw perr('tool_choice', 'type tool needs a name');
      if (!names.has(tc.name)) throw perr('tool_choice', `names tool ${JSON.stringify(tc.name)} that is not in tools`);
    } else if (tc.type !== 'auto' && tc.type !== 'any') throw perr('tool_choice', `type must be auto, any, tool or none, got ${JSON.stringify(tc.type)}`);
    if (names.size === 0) throw perr('tool_choice', 'requires tools');
  }
}

/** validateTaskRun validates the payload about to be signed; throws ParamError. */
/** announceProblem validates hello / models.update data against its schema
 * (spec §7.4): '' when valid, else a message naming the first bad field. */
export function announceProblem(data, hello) {
  const name = hello ? 'hello' : 'models.update';
  const e = schemaSet().validate(BASE + name + '.json', JSON.parse(JSON.stringify(data ?? null)));
  return e ? `${name}${e.path.length ? '.' + e.path.join('.') : ''}: ${e.message}` : '';
}

/** Validates a device data frame's envelope and, for task.done / task.error,
 *  its body against task.done.json / task.error.json: '' when acceptable
 *  (audit3 P2-6: checked before the seq is accepted, persisted or ACKed). */
export function deviceDataProblem(inp) {
  if (typeof inp.task_id !== 'string' || inp.task_id === '') return `${inp.type} without task_id`;
  if (!Number.isSafeInteger(inp.seq) || inp.seq < 1) return `${inp.type} seq must be an integer >= 1`;
  if (inp.data === null || typeof inp.data !== 'object' || Array.isArray(inp.data)) return `${inp.type} data must be a JSON object`;
  if (inp.type !== 'task.done' && inp.type !== 'task.error') return '';
  const e = schemaSet().validate(BASE + inp.type + '.json', JSON.parse(JSON.stringify(inp.data)));
  return e ? `${inp.type}${e.path.length ? '.' + e.path.join('.') : ''}: ${e.message}` : '';
}

export function validateTaskRun(payload) {
  const e = schemaSet().validate(BASE + 'task.run.payload.json', JSON.parse(JSON.stringify(payload)));
  if (e) throw new ParamError(e.path.length ? String(e.path[0]) : '', `request does not satisfy the task.run schema at /${e.path.join('/')}: ${e.message}`);
}
