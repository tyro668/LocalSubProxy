// A4: the in-process client treats pre-call cancel, mid-call cancel, a
// producer failure and an abandoned stream the same way: the caller gets an
// error or terminator (never a hang), and the engine sees the request close.
import test from 'node:test';
import assert from 'node:assert/strict';
import { InProcessClient } from '../dist/index.js';

function fakeEngine(handle) {
  return { engine: { tenants: new Map([['t', {}]]), handle } };
}
const within = (p, ms, what) => Promise.race([p, new Promise((_, no) => setTimeout(() => no(new Error('timed out: ' + what)), ms))]);
const sseHead = (res) => { res.writeHead(200, { 'content-type': 'text/event-stream', 'x-task-id': 'task_1' }); };

// streams one chunk then waits for the request to close; resolves `closed`
function streamer() {
  let closed; const p = new Promise((ok) => { closed = ok; });
  const handle = async (req, res) => {
    sseHead(res);
    res.write('id: 0\ndata: {"choices":[{"delta":{"content":"a"}}]}\n\n');
    await new Promise((ok) => res.on('close', ok));
    closed();
  };
  return { handle, closed: p };
}

test('pre-aborted signal: the engine is never called', async () => {
  let called = false;
  const c = new InProcessClient(fakeEngine(async () => { called = true; }), 't', 'u');
  const ac = new AbortController(); ac.abort();
  const st = c.chat.completions.stream({ model: 'm', messages: [] }, { signal: ac.signal });
  await assert.rejects(within(st.next(), 2000, 'pre-abort'), (e) => e.name === 'AbortError');
  assert.equal(called, false, 'engine ran for a request aborted before the call');
});

test('mid-call abort: engine sees close and the stream ends with an error', async () => {
  const s = streamer();
  const c = new InProcessClient(fakeEngine(s.handle), 't', 'u');
  const ac = new AbortController();
  const st = c.chat.completions.stream({ model: 'm', messages: [] }, { signal: ac.signal });
  assert.equal((await st.next()).value.choices[0].delta.content, 'a');
  ac.abort();
  await within(s.closed, 2000, 'engine close');
  await assert.rejects(within(st.next(), 2000, 'consumer after abort'));
});

test('stream closed by the consumer: engine sees close', async () => {
  const s = streamer();
  const c = new InProcessClient(fakeEngine(s.handle), 't', 'u');
  const st = c.chat.completions.stream({ model: 'm', messages: [] });
  await st.next();
  await within(st.return(), 2000, 'return');
  await within(s.closed, 2000, 'engine close after return()');
});

test('producer throws after headers: consumer gets the error, not a hang or garbage', async () => {
  const c = new InProcessClient(fakeEngine(async (req, res) => {
    sseHead(res);
    res.write('id: 0\ndata: {"choices":[{"delta":{"content":"a"}}]}\n\n');
    await new Promise((ok) => setTimeout(ok, 10));
    throw new Error('producer died');
  }), 't', 'u');
  const st = c.chat.completions.stream({ model: 'm', messages: [] });
  await st.next();
  await assert.rejects(within(st.next(), 2000, 'after producer failure'), /producer died/);
});

test('producer throws before headers: the call rejects with the error', async () => {
  const c = new InProcessClient(fakeEngine(async () => { throw new Error('boom'); }), 't', 'u');
  await assert.rejects(within(c.models(), 2000, 'models'), /boom/);
});
