// Review9 P2-10: tool_result content arrays used to keep only text blocks;
// images vanished silently. Images now follow the tool results as image_url
// parts of the next user message; unsupported blocks are a clear error.
import test from 'node:test';
import assert from 'node:assert/strict';
import { toOpenAI } from '../dist/anthropic.js';

const conv = (content) => ({ model: 'm', max_tokens: 8, messages: [
  { role: 'user', content: 'take a screenshot' },
  { role: 'assistant', content: [{ type: 'tool_use', id: 'toolu_1', name: 'screenshot', input: {} }] },
  { role: 'user', content: [{ type: 'tool_result', tool_use_id: 'toolu_1', content }] },
] });

test('tool_result image is converted, not dropped', () => {
  const out = toOpenAI(conv([{ type: 'text', text: 'here' }, { type: 'image', source: { type: 'base64', media_type: 'image/png', data: 'QUJD' } }]));
  const tool = out.messages.find((m) => m.role === 'tool');
  assert.match(tool.content, /here/);
  const last = out.messages[out.messages.length - 1];
  assert.equal(last.role, 'user');
  assert.ok(last.content.some((p) => p.type === 'image_url' && p.image_url.url === 'data:image/png;base64,QUJD'), JSON.stringify(out.messages));
});

test('unsupported tool_result content is rejected', () => {
  assert.throws(() => toOpenAI(conv([{ type: 'document', source: { type: 'text', data: 'x' } }])), /tool_result content type document/);
});
