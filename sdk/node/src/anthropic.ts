/**
 * Anthropic Messages compatibility (/v1/messages, LMBP spec §6.7): request
 * translation to the OpenAI chat shape and rendering of task events as an
 * Anthropic message or named-event SSE stream. Port of
 * internal/lmbp/gateway/anthropic.go; keep the three implementations in step.
 */
import type { ServerResponse } from 'node:http';

type Obj = Record<string, any>;

const STATUS_TYPE: Record<number, string> = {
  400: 'invalid_request_error', 422: 'invalid_request_error', 401: 'authentication_error', 403: 'permission_error',
  404: 'not_found_error', 413: 'request_too_large', 429: 'rate_limit_error', 503: 'overloaded_error', 529: 'overloaded_error',
};

/** Anthropic error type for an HTTP status. */
export const errorType = (status: number): string => STATUS_TYPE[status] ?? 'api_error';

/** Anthropic error object; lmbp_code carries the protocol error code. */
export const errorBody = (status: number, code: string, message: string) =>
  ({ type: 'error', error: { type: errorType(status), message, lmbp_code: code } });

/** OpenAI finish_reason → Anthropic stop_reason. */
export const stopReason = (finish?: string) => (finish === 'length' ? 'max_tokens' : finish === 'tool_calls' ? 'tool_use' : 'end_turn');

/** Deterministic serialisation (used for the Idempotency-Key body hash). */
export const canonicalJSON = (v: unknown) => JSON.stringify(v);

/** Translate a Messages request into a chat-completions request. Throws Error (→ 400). */
export function toOpenAI(input: Obj): Obj {
  if (typeof input.model !== 'string' || !input.model) throw new Error('model: field required');
  const mt = input.max_tokens;
  if (!Number.isInteger(mt) || mt < 1) throw new Error('max_tokens: field required (positive integer)');
  if ('lmbp_e2ee' in input) throw new Error('lmbp_e2ee is only supported on /v1/chat/completions');
  const out: Obj = { model: input.model, max_tokens: mt, stream: input.stream === true };
  if (typeof input.temperature === 'number') out.temperature = input.temperature;
  const msgs: Obj[] = [];
  const sys = input.system;
  if (typeof sys === 'string') { if (sys) msgs.push({ role: 'system', content: sys }); }
  else if (Array.isArray(sys)) {
    const parts = sys.map((b) => { if (b?.type !== 'text') throw new Error('system: only text blocks are supported'); return String(b.text ?? ''); });
    msgs.push({ role: 'system', content: parts.join('\n\n') });
  } else if (sys !== undefined && sys !== null) throw new Error('system: must be a string or an array of text blocks');
  const list = input.messages;
  if (!Array.isArray(list) || !list.length) throw new Error('messages: at least one message is required');
  list.forEach((m: Obj, i: number) => {
    const role = m?.role;
    if (role !== 'user' && role !== 'assistant') throw new Error(`messages.${i}.role: must be user or assistant`);
    if (typeof m.content === 'string') { msgs.push({ role, content: m.content }); return; }
    if (!Array.isArray(m.content)) throw new Error(`messages.${i}.content: must be a string or an array of blocks`);
    const parts: Obj[] = [], calls: Obj[] = [], results: Obj[] = [], text: string[] = [];
    m.content.forEach((b: Obj, j: number) => {
      switch (b?.type) {
        case 'text': parts.push({ type: 'text', text: String(b.text ?? '') }); text.push(String(b.text ?? '')); break;
        case 'image': {
          const src = b.source || {};
          let url: string;
          if (src.type === 'base64') url = `data:${src.media_type ?? ''};base64,${src.data ?? ''}`;
          else if (src.type === 'url') url = String(src.url ?? '');
          else throw new Error(`messages.${i}.content.${j}.source.type: base64 or url required`);
          parts.push({ type: 'image_url', image_url: { url } });
          break;
        }
        case 'tool_use':
          if (role !== 'assistant') throw new Error(`messages.${i}: tool_use blocks belong to assistant messages`);
          calls.push({ id: String(b.id ?? ''), type: 'function', function: { name: String(b.name ?? ''), arguments: b.input == null ? '{}' : JSON.stringify(b.input) } });
          break;
        case 'tool_result': {
          if (role !== 'user') throw new Error(`messages.${i}: tool_result blocks belong to user messages`);
          let c = '';
          if (typeof b.content === 'string') c = b.content;
          else if (Array.isArray(b.content)) c = b.content.filter((z: Obj) => z?.type === 'text').map((z: Obj) => String(z.text ?? '')).join('\n');
          if (b.is_error === true) c = 'Error: ' + c;
          results.push({ role: 'tool', tool_call_id: String(b.tool_use_id ?? ''), content: c });
          break;
        }
        case 'thinking': case 'redacted_thinking': break;
        default: throw new Error(`messages.${i}.content.${j}: unsupported block type ${b?.type}`);
      }
    });
    msgs.push(...results);
    if (role === 'assistant') {
      if (!text.length && !calls.length) return;
      const am: Obj = { role: 'assistant', content: text.join('') };
      if (calls.length) { am.tool_calls = calls; if (!text.length) am.content = null; }
      msgs.push(am);
    } else if (parts.length) msgs.push({ role: 'user', content: parts });
  });
  out.messages = msgs;
  if (Array.isArray(input.tools) && input.tools.length) {
    out.tools = input.tools.map((t: Obj, i: number) => {
      if (t?.type !== undefined && t.type !== 'custom') throw new Error(`tools.${i}: server tool ${t.type} is not supported`);
      if (!t?.name) throw new Error(`tools.${i}.name: field required`);
      const fn: Obj = { name: t.name, parameters: t.input_schema ?? null };
      if (typeof t.description === 'string') fn.description = t.description;
      return { type: 'function', function: fn };
    });
  }
  const tc = input.tool_choice;
  if (tc && typeof tc === 'object') {
    if (tc.type === 'auto') out.tool_choice = 'auto';
    else if (tc.type === 'any') out.tool_choice = 'required';
    else if (tc.type === 'none') out.tool_choice = 'none';
    else if (tc.type === 'tool') out.tool_choice = { type: 'function', function: { name: tc.name } };
  }
  return out;
}

/** /v1/models in the Anthropic list shape. */
export function modelsList(data: Obj[]) {
  return {
    data: data.map((m) => ({ type: 'model', id: m.id, display_name: m.id, created_at: '1970-01-01T00:00:00Z' })),
    has_more: false, first_id: data[0]?.id ?? null, last_id: data.at(-1)?.id ?? null,
  };
}

const toolInput = (s: string) => { try { const v = s ? JSON.parse(s) : {}; return v && typeof v === 'object' ? v : {}; } catch { return {}; } };

interface TaskLike {
  id: string;
  reqModel: string;
  stream(after: number, signal: AbortSignal, keepalive: (() => void) | null, fn: (e: { seq: number; type: string; data: Obj }) => void): Promise<boolean>;
}

/** Render a task as an Anthropic response (see Go serveAnthropic). */
export async function serve(res: ServerResponse, t: TaskLike, after: number, stream: boolean, signal: AbortSignal,
  json: (res: ServerResponse, status: number, body: unknown) => void): Promise<void> {
  const msgID = 'msg_' + t.id.replace(/^task_/, '');
  const usageOf = (u?: Obj) => ({ input_tokens: u?.prompt_tokens ?? 0, output_tokens: u?.completion_tokens ?? 0 });
  if (!stream) {
    let text = '';
    const calls: { id?: string; name: string; args: string }[] = [];
    let done: Obj | null = null, fail: Obj | null = null;
    const ok = await t.stream(0, signal, null, (e) => {
      if (e.type === 'task.delta') {
        text += e.data?.text || '';
        for (const c of e.data?.tool_calls || []) {
          const i = Number.isInteger(c.index) ? c.index : 0;
          while (calls.length <= i) calls.push({ name: '', args: '' });
          if (c.id) calls[i].id = c.id;
          calls[i].name += c.function?.name || '';
          calls[i].args += c.function?.arguments || '';
        }
      } else if (e.type === 'task.done') done = e.data || {};
      else fail = e.data || {};
    });
    if (!ok) return;
    if (fail) { const f = fail as Obj; const st = statusOf(f.code); return json(res, st, errorBody(st, f.code, f.message)); }
    const content: Obj[] = [];
    if (text) content.push({ type: 'text', text });
    for (const c of calls) content.push({ type: 'tool_use', id: c.id ?? '', name: c.name, input: toolInput(c.args) });
    const d = done as unknown as Obj;
    return json(res, 200, { id: msgID, type: 'message', role: 'assistant', model: t.reqModel, content,
      stop_reason: stopReason(d.finish_reason), stop_sequence: null, usage: usageOf(d.usage) });
  }
  res.writeHead(200, { 'Content-Type': 'text/event-stream', 'Cache-Control': 'no-cache', 'X-Accel-Buffering': 'no' });
  // Events derived from one task event are written as a group; only the last
  // carries that event's seq as id, so Last-Event-ID names a whole group.
  let cur = 0;
  let group: [string, unknown][] = [];
  const send = (type: string, data: unknown) => { group.push([type, data]); };
  const flush = () => {
    const g = group; group = [];
    if (after > 0 && cur <= after) return;
    let s = '';
    g.forEach(([type, data], i) => { s += `event: ${type}\n${i === g.length - 1 && cur > 0 ? `id: ${cur}\n` : ''}data: ${JSON.stringify(data)}\n\n`; });
    if (s) res.write(s);
  };
  send('message_start', { type: 'message_start', message: { id: msgID, type: 'message', role: 'assistant', model: t.reqModel, content: [],
    stop_reason: null, stop_sequence: null, usage: { input_tokens: 0, output_tokens: 0 } } });
  flush();
  let idx = -1, open = '';
  const tools = new Map<number, number>();
  const closeOpen = () => { if (open) { send('content_block_stop', { type: 'content_block_stop', index: idx }); open = ''; } };
  await t.stream(0, signal, () => res.write('event: ping\ndata: {"type":"ping"}\n\n'), (e) => {
    cur = e.seq;
    if (e.type === 'task.delta') {
      const d = e.data || {};
      if (d.text) {
        if (open !== 'text') { closeOpen(); idx++; open = 'text'; send('content_block_start', { type: 'content_block_start', index: idx, content_block: { type: 'text', text: '' } }); }
        send('content_block_delta', { type: 'content_block_delta', index: idx, delta: { type: 'text_delta', text: d.text } });
      }
      for (const c of d.tool_calls || []) {
        const ti = Number.isInteger(c.index) ? c.index : 0;
        let bi = tools.get(ti);
        if (bi === undefined) {
          closeOpen(); idx++; bi = idx; tools.set(ti, bi); open = 'tool' + ti;
          const id = c.id || `toolu_${t.id.replace(/^task_/, '')}_${ti}`;
          send('content_block_start', { type: 'content_block_start', index: bi, content_block: { type: 'tool_use', id, name: c.function?.name || '', input: {} } });
        }
        if (c.function?.arguments) send('content_block_delta', { type: 'content_block_delta', index: bi, delta: { type: 'input_json_delta', partial_json: c.function.arguments } });
      }
    } else if (e.type === 'task.done') {
      closeOpen();
      const u = e.data?.usage;
      send('message_delta', { type: 'message_delta', delta: { stop_reason: stopReason(e.data?.finish_reason), stop_sequence: null },
        usage: u ? usageOf(u) : { output_tokens: 0 } });
      send('message_stop', { type: 'message_stop' });
    } else {
      const eb = e.data || {};
      send('error', { type: 'error', error: { type: errorType(statusOf(eb.code)), message: eb.message, lmbp_code: eb.code } });
    }
    flush();
  });
  res.end();
}

const STATUS_OF: Record<string, number> = {
  BAD_REQUEST: 400, CAPABILITY_UNSUPPORTED: 400, PAYLOAD_TOO_LARGE: 413, IDEMPOTENCY_KEY_REUSED: 422, UNAUTHENTICATED: 401, FORBIDDEN: 403,
  CONSENT_DENIED: 403, CANCELLED: 499, MODEL_NOT_FOUND: 404, DEVICE_BUSY: 429, RATE_LIMITED: 429, DEVICE_OFFLINE: 503, TIMEOUT: 504, OUTCOME_UNKNOWN: 502,
};
const statusOf = (code: string) => STATUS_OF[code] ?? 502;
