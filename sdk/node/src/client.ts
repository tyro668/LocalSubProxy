/**
 * In-process client: calls the gateway's OpenAI/Anthropic surface without an
 * HTTP hop (the request runs through the same engine code as HTTP requests).
 */
import { PassThrough } from 'node:stream';
import { EventEmitter } from 'node:events';
import type {
  ChatRequest, ChatCompletion, ChatCompletionChunk, Model, MessagesRequest, Message, MessageStreamEvent, APIErrorShape,
} from './types.js';

export class LMBPAPIError extends Error implements APIErrorShape {
  constructor(readonly status: number, readonly code: string, message: string, readonly body?: unknown) { super(message); }
}

interface Resp { status: number; headers: Record<string, string>; body: AsyncIterable<Buffer> }

class FakeRes extends EventEmitter {
  statusCode = 200;
  headersSent = false;
  writableEnded = false;
  private hdrs: Record<string, string> = {};
  readonly out = new PassThrough();
  readonly started: Promise<void>;
  private start!: () => void;
  constructor() { super(); this.started = new Promise((ok) => { this.start = ok; }); }
  setHeader(k: string, v: string | number) { this.hdrs[k.toLowerCase()] = String(v); return this; }
  getHeader(k: string) { return this.hdrs[k.toLowerCase()]; }
  removeHeader(k: string) { delete this.hdrs[k.toLowerCase()]; }
  get headers() { return this.hdrs; }
  writeHead(status: number, h?: Record<string, string | number>) {
    this.statusCode = status;
    for (const [k, v] of Object.entries(h || {})) this.setHeader(k, v);
    this.headersSent = true; this.start();
    return this;
  }
  write(chunk: string | Buffer) { if (!this.headersSent) this.writeHead(this.statusCode); this.out.write(chunk); return true; }
  end(chunk?: string | Buffer) {
    if (this.writableEnded) return this;
    if (chunk) this.write(chunk); else if (!this.headersSent) this.writeHead(this.statusCode);
    this.writableEnded = true; this.out.end();
    this.emit('finish'); this.emit('close');
    return this;
  }
  flushHeaders() {}
}

export class InProcessClient {
  constructor(private readonly server: { engine: any }, readonly tenant: string, readonly endUser: string) {}

  /** Low-level request into the engine. */
  async request(method: string, path: string, body?: unknown, headers: Record<string, string> = {}, signal?: AbortSignal): Promise<Resp> {
    const g = this.server.engine;
    const t = g.tenants.get(this.tenant);
    if (!t) throw new LMBPAPIError(401, 'UNAUTHENTICATED', 'unknown tenant ' + this.tenant);
    const raw = body === undefined ? Buffer.alloc(0) : Buffer.from(JSON.stringify(body));
    const req: any = new EventEmitter();
    Object.assign(req, {
      method, url: path, socket: { encrypted: false, remoteAddress: '127.0.0.1' }, body: raw, readableEnded: true,
      headers: { host: 'in-process', 'content-type': 'application/json', 'x-end-user': this.endUser, ...lower(headers) },
      [IN_PROCESS]: t,
    });
    const res = new FakeRes();
    signal?.addEventListener('abort', () => { res.emit('close'); res.end(); }, { once: true });
    const done = g.handle(req, res);
    done.catch((e: unknown) => { if (!res.headersSent) { res.writeHead(500); } res.end(String(e)); });
    await Promise.race([res.started, done]);
    await res.started;
    return { status: res.statusCode, headers: res.headers, body: res.out };
  }

  private async json<T>(method: string, path: string, body?: unknown, headers?: Record<string, string>): Promise<T> {
    const r = await this.request(method, path, body, headers);
    const chunks: Buffer[] = [];
    for await (const c of r.body) chunks.push(Buffer.from(c));
    const text = Buffer.concat(chunks).toString();
    const v = text ? JSON.parse(text) : null;
    if (r.status >= 400) throw toError(r.status, v);
    return v as T;
  }

  private async *sse(method: string, path: string, body: unknown, headers?: Record<string, string>, signal?: AbortSignal, st?: LMBPStream<unknown>) {
    const r = await this.request(method, path, body, headers, signal);
    if (st) st.taskId = String(r.headers['x-task-id'] || '');
    if (r.status >= 400 || !String(r.headers['content-type'] || '').startsWith('text/event-stream')) {
      const chunks: Buffer[] = [];
      for await (const c of r.body) chunks.push(Buffer.from(c));
      const t = Buffer.concat(chunks).toString();
      throw toError(r.status, t ? JSON.parse(t) : null);
    }
    let buf = '';
    for await (const c of r.body) {
      buf += c.toString();
      let i;
      while ((i = buf.indexOf('\n\n')) >= 0) {
        const block = buf.slice(0, i); buf = buf.slice(i + 2);
        let event = '', data = '', id = '';
        for (const l of block.split('\n')) {
          if (l.startsWith('event: ')) event = l.slice(7);
          else if (l.startsWith('data: ')) data += l.slice(6);
          else if (l.startsWith('id: ')) id = l.slice(4);
        }
        if (data) yield { event, data, id };
      }
    }
  }

  /** GET /v1/models (OpenAI shape). */
  async models(): Promise<Model[]> { return (await this.json<{ data: Model[] }>('GET', '/v1/models')).data; }

  readonly chat = {
    completions: {
      /** Non-streaming chat completion. */
      create: (req: ChatRequest & { stream?: false }, opts: { idempotencyKey?: string } = {}): Promise<ChatCompletion> =>
        this.json('POST', '/v1/chat/completions', { ...req, stream: false }, idem(opts.idempotencyKey)),
      /** Streaming chat completion: async iterable of chunks. */
      stream: (req: ChatRequest, opts: StreamOpts = {}): LMBPStream<ChatCompletionChunk> =>
        this.chatStream('POST', '/v1/chat/completions', { ...req, stream: true }, opts),
      /** Re-attach to a task's stream after lastEventId (stream.lastEventId; '' replays all). Ends at once past the terminal event. */
      resume: (taskId: string, lastEventId = '', opts: { signal?: AbortSignal } = {}): LMBPStream<ChatCompletionChunk> =>
        this.chatStream('GET', `/lmbp/v1/tenant/tasks/${encodeURIComponent(taskId)}/events`, undefined, { ...opts, lastEventId }),
    },
  };

  private chatStream(method: string, path: string, body: unknown, opts: StreamOpts): LMBPStream<ChatCompletionChunk> {
    const st: LMBPStream<ChatCompletionChunk> = new LMBPStream(async function* (this: InProcessClient) {
      for await (const e of this.sse(method, path, body, hdrs(opts), opts.signal, st)) {
        if (e.data === '[DONE]') return;
        const v = JSON.parse(e.data);
        if (v.error) throw new LMBPAPIError(502, v.error.code, v.error.message, v);
        if (e.id) st.lastEventId = e.id;
        yield v;
      }
    }.bind(this));
    return st;
  }

  readonly messages = {
    /** Anthropic Messages, non-streaming. */
    create: (req: MessagesRequest & { stream?: false }, opts: { idempotencyKey?: string } = {}): Promise<Message> =>
      this.json('POST', '/v1/messages', { ...req, stream: false }, idem(opts.idempotencyKey)),
    /** Anthropic Messages, streaming: async iterable of named events. */
    stream: (req: MessagesRequest, opts: StreamOpts = {}): LMBPStream<MessageStreamEvent> => {
      const st: LMBPStream<MessageStreamEvent> = new LMBPStream(async function* (this: InProcessClient) {
        for await (const e of this.sse('POST', '/v1/messages', { ...req, stream: true }, hdrs(opts), opts.signal, st)) {
          const v = JSON.parse(e.data);
          if (v.type === 'error') throw new LMBPAPIError(502, v.error?.lmbp_code ?? 'ERROR', v.error?.message ?? 'error', v);
          if (e.id) st.lastEventId = e.id;
          if (v.type !== 'ping') yield v;
        }
      }.bind(this));
      return st;
    },
  };
}

export interface StreamOpts { idempotencyKey?: string; lastEventId?: string; signal?: AbortSignal }

/**
 * A streaming response: iterate it with for await. lastEventId is the SSE id
 * of the last event yielded (the resume cursor); taskId is the gateway task.
 * Resume with the same request + idempotencyKey + lastEventId, or with
 * chat.completions.resume(taskId, lastEventId).
 */
export class LMBPStream<T> implements AsyncIterableIterator<T> {
  lastEventId = '';
  taskId = '';
  private it?: AsyncGenerator<T>;
  constructor(private readonly make: () => AsyncGenerator<T>) {}
  private get g() { return (this.it ??= this.make()); }
  next() { return this.g.next(); }
  return(v?: any) { return this.g.return(v); }
  throw(e?: any) { return this.g.throw(e); }
  [Symbol.asyncIterator]() { return this; }
}

/** Marks a request created by the in-process client; carries the tenant. */
export const IN_PROCESS = Symbol.for('lmbp.inProcessTenant');

const lower = (h: Record<string, string>) => Object.fromEntries(Object.entries(h).map(([k, v]) => [k.toLowerCase(), v]));
const idem = (k?: string): Record<string, string> => (k ? { 'idempotency-key': k } : {});
const hdrs = (o: StreamOpts): Record<string, string> => ({ ...idem(o.idempotencyKey), ...(o.lastEventId ? { 'last-event-id': o.lastEventId } : {}) });
function toError(status: number, v: any): LMBPAPIError {
  const e = v?.error ?? {};
  return new LMBPAPIError(status, e.code ?? e.lmbp_code ?? 'ERROR', e.message ?? v?.detail ?? 'request failed', v);
}
