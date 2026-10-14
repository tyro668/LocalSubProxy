// Official openai / @anthropic-ai/sdk Node libraries, unchanged, against an LMBP gateway.
// Env: LMBP_BASE_URL, LMBP_API_KEY, LMBP_END_USER, LMBP_MODEL.
import OpenAI from 'openai';
import Anthropic from '@anthropic-ai/sdk';

const { LMBP_BASE_URL: base, LMBP_API_KEY: key, LMBP_END_USER: user, LMBP_MODEL: model } = process.env;
const hdr = { 'X-End-User': user };
const ok = (name, cond, detail = '') => {
  console.log((cond ? 'PASS ' : 'FAIL ') + name + (cond ? '' : `: ${JSON.stringify(detail)}`));
  if (!cond) process.exit(1);
};

const oa = new OpenAI({ baseURL: base + '/v1', apiKey: key, defaultHeaders: hdr });
ok('js-openai models.list', (await oa.models.list()).data.some((m) => m.id === model));
let r = await oa.chat.completions.create({ model, messages: [{ role: 'user', content: 'ping' }] });
ok('js-openai chat', r.choices[0].message.content.includes('ping'), r);
let txt = '';
for await (const c of await oa.chat.completions.create({ model, stream: true, messages: [{ role: 'user', content: 'pong' }] })) txt += c.choices[0]?.delta?.content ?? '';
ok('js-openai chat stream', txt.includes('pong'), txt);

const an = new Anthropic({ baseURL: base, apiKey: key, defaultHeaders: hdr });
ok('js-anthropic models.list', (await an.models.list()).data.some((m) => m.id === model));
let m = await an.messages.create({ model, max_tokens: 64, system: 'be brief', messages: [{ role: 'user', content: 'hello' }] });
ok('js-anthropic messages', m.stop_reason === 'end_turn' && m.content[0].text.includes('hello'), m);
const s = an.messages.stream({ model, max_tokens: 64, messages: [{ role: 'user', content: 'streamed' }] });
txt = '';
s.on('text', (t) => { txt += t; });
const fin = await s.finalMessage();
ok('js-anthropic messages stream', txt.includes('streamed') && fin.stop_reason === 'end_turn', txt);
const tools = [{ name: 'get_weather', input_schema: { type: 'object', properties: { city: { type: 'string' } } } }];
m = await an.messages.create({ model, max_tokens: 64, tools, messages: [{ role: 'user', content: 'weather in Paris?' }] });
const tu = m.content.find((b) => b.type === 'tool_use');
ok('js-anthropic tool_use', m.stop_reason === 'tool_use' && tu?.name === 'get_weather', m);
const m2 = await an.messages.create({ model, max_tokens: 64, tools, messages: [
  { role: 'user', content: 'weather in Paris?' }, { role: 'assistant', content: m.content },
  { role: 'user', content: [{ type: 'tool_result', tool_use_id: tu.id, content: '晴，25度' }] }] });
ok('js-anthropic tool_result round trip', m2.stop_reason === 'end_turn' && m2.content.some((b) => b.type === 'text' && b.text.includes('晴')), m2);
try {
  await an.messages.create({ model: 'no-such/model', max_tokens: 8, messages: [{ role: 'user', content: 'x' }] });
  ok('js-anthropic NotFoundError', false);
} catch (e) { ok('js-anthropic NotFoundError', e instanceof Anthropic.NotFoundError, String(e)); }
