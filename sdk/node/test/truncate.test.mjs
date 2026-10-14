// Third audit item 3: a stream that ends (EOF) before its terminator
// ([DONE] / message_stop / an error event) is not a normal end. The
// in-process client must throw IncompleteStreamError carrying the cursor.
import test from 'node:test';
import assert from 'node:assert/strict';
import * as sdk from '../dist/index.js';
import { InProcessClient } from '../dist/client.js';

// A truncating fake server: writes `body` and ends the response.
const fake = (body) => new InProcessClient({
  engine: {
    tenants: new Map([['t', {}]]),
    async handle(_req, res) {
      res.writeHead(200, { 'content-type': 'text/event-stream', 'x-task-id': 'task_fake' });
      res.write(body); res.end();
    },
  },
}, 't', 'u');

const chunkA = '{"object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"A"},"finish_reason":null}]}';

async function drain(st) {
  let text = '';
  try {
    for await (const c of st) text += c.choices?.[0]?.delta?.content ?? c.delta?.text ?? '';
    return { text, err: null };
  } catch (err) { return { text, err }; }
}

function incomplete(err, cursor) {
  assert.ok(sdk.IncompleteStreamError, 'IncompleteStreamError is exported');
  assert.ok(err instanceof sdk.IncompleteStreamError, `want IncompleteStreamError, got ${err}`);
  assert.equal(err.code, 'INCOMPLETE_STREAM');
  assert.equal(err.lastEventId, cursor);
  assert.equal(err.taskId, 'task_fake');
}

for (const [name, body, cursor, text] of [
  ['eof after two chunks', `id: 1\ndata: ${chunkA}\n\nid: 2\ndata: ${chunkA}\n\n`, '2', 'AA'],
  ['eof mid-event is not delivered', `id: 1\ndata: ${chunkA}\n\nid: 2\ndata: ${chunkA}`, '1', 'A'],
  ['eof before any event', '', '', ''],
  ['eof after the finish chunk but before [DONE]', `id: 1\ndata: ${chunkA}\n\nid: 2\ndata: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}\n\n`, '2', 'A'],
]) {
  test(`chat stream: ${name} -> IncompleteStreamError`, async () => {
    const st = fake(body).chat.completions.stream({ model: 'm', messages: [] });
    const r = await drain(st);
    assert.equal(r.text, text);
    incomplete(r.err, cursor);
    assert.equal(st.lastEventId, cursor);
  });
}

test('chat stream: [DONE] is a normal end', async () => {
  const r = await drain(fake(`id: 1\ndata: ${chunkA}\n\ndata: [DONE]\n\n`).chat.completions.stream({ model: 'm', messages: [] }));
  assert.equal(r.err, null); assert.equal(r.text, 'A');
});

test('chat stream: an error event is an API error, not incomplete', async () => {
  const r = await drain(fake('id: 1\ndata: {"error":{"code":"DEVICE_OFFLINE","message":"x"}}\n\n').chat.completions.stream({ model: 'm', messages: [] }));
  assert.equal(r.err?.code, 'DEVICE_OFFLINE');
});

const head = 'event: message_start\ndata: {"type":"message_start","message":{}}\n\n'
  + 'event: content_block_delta\nid: 1\ndata: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"A"}}\n\n';

test('messages stream: eof before message_stop -> IncompleteStreamError', async () => {
  const r = await drain(fake(head + 'event: ping\ndata: {"type":"ping"}\n\n').messages.stream({ model: 'm', max_tokens: 8, messages: [] }));
  incomplete(r.err, '1');
});

test('messages stream: message_stop is a normal end', async () => {
  const r = await drain(fake(head + 'event: message_stop\nid: 2\ndata: {"type":"message_stop"}\n\n').messages.stream({ model: 'm', max_tokens: 8, messages: [] }));
  assert.equal(r.err, null);
});
