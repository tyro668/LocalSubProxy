// Unit tests for the pure helpers in chat.js: node --test mock/web/test
import test from 'node:test';
import assert from 'node:assert/strict';
import { createRequire } from 'node:module';
const C = createRequire(import.meta.url)('../chat.js');

test('markdown keeps HTML literal and only allows http(s) links', () => {
  const b = C.parseMarkdown('<img src=x onerror=alert(1)> [a](javascript:alert(1)) [b](https://x.dev)');
  const inl = b[0].inl;
  assert.equal(inl[0].t, 'text');
  assert.ok(inl[0].v.includes('<img src=x onerror=alert(1)>'));
  assert.ok(inl[0].v.includes('[a](javascript:alert(1))'));
  assert.deepEqual(inl.find((n) => n.t === 'a'), { t: 'a', href: 'https://x.dev', c: [{ t: 'text', v: 'b' }] });
});

test('markdown blocks: heading, ordered list with inline code, closed and streaming fences', () => {
  const b = C.parseMarkdown('# T\n\n1. a `jkt`\n2. **b**\n\n```json\n{"a":1}\n```\n\n```go\nfunc x(\n');
  assert.deepEqual(b.map((x) => x.t), ['h', 'list', 'code', 'code']);
  assert.equal(b[1].ordered, true);
  assert.deepEqual(b[1].items[0].inl[1], { t: 'code', v: 'jkt' });
  assert.equal(b[1].items[1].inl[0].t, 'b');
  assert.deepEqual([b[2].lang, b[2].text, b[2].closed], ['json', '{"a":1}', true]);
  assert.deepEqual([b[3].lang, b[3].text, b[3].closed], ['go', 'func x(', false]);
});

test('highlight: keywords, strings, comments; JSON keys vs values', () => {
  assert.deepEqual(C.highlight('func f() string // c', 'go'), [{ c: 'k', v: 'func' }, { c: '', v: ' f() ' }, { c: 'k', v: 'string' }, { c: '', v: ' ' }, { c: 'c', v: '// c' }]);
  const j = C.highlight('{"alg": "ECDH-ES"}', 'json');
  assert.deepEqual(j.filter((t) => t.c), [{ c: 'k', v: '"alg"' }, { c: 's', v: '"ECDH-ES"' }]);
  assert.equal(C.highlight('x = 1 # note', 'py').pop().c, 'c');
});

test('tool_calls fragments merge by index', () => {
  const acc = [];
  C.mergeToolCalls(acc, [{ index: 0, id: 'call_1', function: { name: 'search', arguments: '' } }]);
  C.mergeToolCalls(acc, [{ index: 0, function: { arguments: '{"q":' } }, { index: 1, id: 'call_2', function: { name: 'b', arguments: '{}' } }]);
  C.mergeToolCalls(acc, [{ index: 0, function: { arguments: '"x"}' } }]);
  assert.deepEqual(acc, [{ id: 'call_1', name: 'search', args: '{"q":"x"}' }, { id: 'call_2', name: 'b', args: '{}' }]);
  assert.equal(C.prettyArgs('{"query":"a","top_k":3}'), '{ "query": "a", "top_k": 3 }');
  assert.equal(C.prettyArgs('{"query":'), '{"query":');
});

test('steps come only from observed events; tool calls are shown as requests, not results', () => {
  const done = { state: 'done', dispatchedAt: 120, firstAt: 412, endAt: 7400, finish: 'stop', tools: [], taskIds: ['t1'] };
  assert.deepEqual(C.steps(done).map((s) => s.k), ['dispatch', 'first', 'done']);
  const tools = { ...done, finish: 'tool_calls', toolsAt: 1860, tools: [{ id: 'c', name: 'f', args: '{}' }] };
  assert.deepEqual(C.steps(tools).map((s) => s.k), ['dispatch', 'first', 'tools']);
  assert.deepEqual(C.summary(tools), ['1 次任务', '1 次工具调用', '用时 7.4 秒']);
  assert.deepEqual(C.steps({ state: 'live', tools: [], taskIds: [] }).map((s) => [s.k, s.state]), [['dispatch', 'live']]);
  assert.deepEqual(C.steps({ state: 'error', endAt: 5, error: { code: 'DEVICE_OFFLINE' }, tools: [] }).map((s) => s.k), ['error']);
  assert.deepEqual(C.metrics({ firstAt: 412, endAt: 7400, usage: { prompt_tokens: 38, completion_tokens: 286 }, finish: 'stop' }), ['首字 412 ms', '总耗时 7.4 s', '38 → 286 tokens', '40.9 tok/s']);
});

test('labels, ids, titles, history grouping, image stripping, pairing code', () => {
  assert.equal(C.shortModel({ id: 'dev_1/local/qwen3-8b' }), 'qwen3-8b');
  assert.equal(C.shortModel({ id: 'dev_1/local/q', display_name: '通义' }), '通义');
  assert.equal(C.shortModel('dev_1/local/llama3.2'), 'llama3.2');
  assert.equal(C.shortId('dev_844ecdd014750482971af079554161b9'), 'dev_844ecdd0…554161b9');
  assert.equal(C.titleOf('  a\n b '), 'a b');
  assert.equal(C.titleOf('x'.repeat(40)).length, 31);
  const now = new Date('2026-10-19T12:00:00').getTime();
  const g = C.groupByDay([{ id: 1, updated: now - 1000 }, { id: 2, updated: now - 864e5 }, { id: 3, updated: now - 5 * 864e5 }, { id: 4, updated: now }], now);
  assert.deepEqual(g.map(([k, v]) => [k, v.map((c) => c.id)]), [['今天', [4, 1]], ['昨天', [2]], ['更早', [3]]]);
  const s = C.stripImages({ messages: [{ role: 'user', content: [{ type: 'text', text: 'q' }, { type: 'image_url', image_url: { url: 'data:x' } }] }], turns: [{ img: 'data:x' }] });
  assert.equal(JSON.stringify(s).includes('data:x'), false);
  assert.equal(C.codeChars('k7qf-3h9x'), 'K7QF3H9X');
  assert.equal(C.brandOf('acme'), 'Acme');
});
