"""Official openai / anthropic Python libraries, unchanged, against an LMBP gateway.

Env: LMBP_BASE_URL (gateway root), LMBP_API_KEY (tenant key), LMBP_END_USER, LMBP_MODEL.
"""
import os
import sys

import anthropic
import openai

base, key, user, model = (os.environ[k] for k in ("LMBP_BASE_URL", "LMBP_API_KEY", "LMBP_END_USER", "LMBP_MODEL"))
hdr = {"X-End-User": user}
tools_oa = [{"type": "function", "function": {"name": "get_weather", "parameters": {"type": "object", "properties": {"city": {"type": "string"}}}}}]
tools_an = [{"name": "get_weather", "input_schema": {"type": "object", "properties": {"city": {"type": "string"}}}}]


def ok(name, cond, detail=""):
    print(("PASS " if cond else "FAIL ") + name + ("" if cond else f": {detail}"))
    if not cond:
        sys.exit(1)


oa = openai.OpenAI(base_url=base + "/v1", api_key=key, default_headers=hdr)
ok("py-openai models.list", any(m.id == model for m in oa.models.list().data))
r = oa.chat.completions.create(model=model, messages=[{"role": "user", "content": "ping"}])
ok("py-openai chat", "ping" in (r.choices[0].message.content or ""), r)
txt = "".join(c.choices[0].delta.content or "" for c in oa.chat.completions.create(model=model, stream=True, messages=[{"role": "user", "content": "pong"}]) if c.choices)
ok("py-openai chat stream", "pong" in txt, txt)
r = oa.chat.completions.create(model=model, tools=tools_oa, messages=[{"role": "user", "content": "weather?"}])
ok("py-openai tools", r.choices[0].finish_reason == "tool_calls" and r.choices[0].message.tool_calls[0].function.name == "get_weather", r)

an = anthropic.Anthropic(base_url=base, api_key=key, default_headers=hdr)
ok("py-anthropic models.list", any(m.id == model for m in an.models.list().data))
m = an.messages.create(model=model, max_tokens=64, system="be brief", messages=[{"role": "user", "content": "hello"}])
ok("py-anthropic messages", m.stop_reason == "end_turn" and "hello" in m.content[0].text and m.usage.output_tokens >= 0, m)
with an.messages.stream(model=model, max_tokens=64, messages=[{"role": "user", "content": "streamed"}]) as s:
    txt = "".join(s.text_stream)
    final = s.get_final_message()
ok("py-anthropic messages stream", "streamed" in txt and final.stop_reason == "end_turn", txt)
m = an.messages.create(model=model, max_tokens=64, tools=tools_an, messages=[{"role": "user", "content": "weather in Paris?"}])
tu = [b for b in m.content if b.type == "tool_use"]
ok("py-anthropic tool_use", m.stop_reason == "tool_use" and tu and tu[0].name == "get_weather" and isinstance(tu[0].input, dict), m)
m2 = an.messages.create(model=model, max_tokens=64, tools=tools_an, messages=[
    {"role": "user", "content": "weather in Paris?"},
    {"role": "assistant", "content": [b.model_dump() for b in m.content]},
    {"role": "user", "content": [{"type": "tool_result", "tool_use_id": tu[0].id, "content": "晴，25度"}]},
])
ok("py-anthropic tool_result round trip", m2.stop_reason == "end_turn" and "晴" in "".join(b.text for b in m2.content if b.type == "text"), m2)
try:
    an.messages.create(model="no-such/model", max_tokens=8, messages=[{"role": "user", "content": "x"}])
    ok("py-anthropic NotFoundError", False, "no error")
except anthropic.NotFoundError as e:
    ok("py-anthropic NotFoundError", e.status_code == 404)
try:
    anthropic.Anthropic(base_url=base, api_key="wrong", default_headers=hdr).messages.create(model=model, max_tokens=8, messages=[{"role": "user", "content": "x"}])
    ok("py-anthropic AuthenticationError", False, "no error")
except anthropic.AuthenticationError:
    ok("py-anthropic AuthenticationError", True)
