"""Anthropic Messages compatibility (/v1/messages, LMBP spec §6.7).

Request translation to the OpenAI chat shape and rendering of task events
as an Anthropic message or named-event SSE stream. Port of
internal/lmbp/gateway/anthropic.go; keep the implementations in step.
"""
from __future__ import annotations

import json
from typing import Any, Callable

_STATUS_TYPE = {400: "invalid_request_error", 422: "invalid_request_error", 401: "authentication_error", 403: "permission_error",
                404: "not_found_error", 413: "request_too_large", 429: "rate_limit_error", 503: "overloaded_error", 529: "overloaded_error"}


def error_type(status: int) -> str:
    """Anthropic error type for an HTTP status."""
    return _STATUS_TYPE.get(status, "api_error")


def error_body(status: int, code: str, message: str) -> dict:
    return {"type": "error", "error": {"type": error_type(status), "message": message, "lmbp_code": code}}


def stop_reason(finish: str | None) -> str:
    return {"length": "max_tokens", "tool_calls": "tool_use"}.get(finish or "", "end_turn")


def canonical_json(v: Any) -> str:
    return json.dumps(v, separators=(",", ":"), ensure_ascii=False)


def _is_int(v: Any) -> bool:
    return isinstance(v, int) and not isinstance(v, bool)


def to_openai(inp: dict) -> dict:
    """Translate a Messages request into a chat-completions request. Raises ValueError (→ 400)."""
    model = inp.get("model")
    if not isinstance(model, str) or not model:
        raise ValueError("model: field required")
    mt = inp.get("max_tokens")
    if not _is_int(mt) or mt < 1:
        raise ValueError("max_tokens: field required (positive integer)")
    if "lmbp_e2ee" in inp:
        raise ValueError("lmbp_e2ee is only supported on /v1/chat/completions")
    out: dict[str, Any] = {"model": model, "max_tokens": mt, "stream": inp.get("stream") is True}
    if isinstance(inp.get("temperature"), (int, float)) and not isinstance(inp.get("temperature"), bool):
        out["temperature"] = inp["temperature"]
    msgs: list[dict] = []
    sys_ = inp.get("system")
    if isinstance(sys_, str):
        if sys_:
            msgs.append({"role": "system", "content": sys_})
    elif isinstance(sys_, list):
        parts = []
        for b in sys_:
            if not isinstance(b, dict) or b.get("type") != "text":
                raise ValueError("system: only text blocks are supported")
            parts.append(str(b.get("text") or ""))
        msgs.append({"role": "system", "content": "\n\n".join(parts)})
    elif sys_ is not None:
        raise ValueError("system: must be a string or an array of text blocks")
    lst = inp.get("messages")
    if not isinstance(lst, list) or not lst:
        raise ValueError("messages: at least one message is required")
    for i, m in enumerate(lst):
        role = m.get("role") if isinstance(m, dict) else None
        if role not in ("user", "assistant"):
            raise ValueError(f"messages.{i}.role: must be user or assistant")
        content = m.get("content")
        if isinstance(content, str):
            msgs.append({"role": role, "content": content})
            continue
        if not isinstance(content, list):
            raise ValueError(f"messages.{i}.content: must be a string or an array of blocks")
        parts, calls, results, text = [], [], [], []
        for j, b in enumerate(content):
            typ = b.get("type") if isinstance(b, dict) else None
            if typ == "text":
                t = str(b.get("text") or "")
                parts.append({"type": "text", "text": t})
                text.append(t)
            elif typ == "image":
                src = b.get("source") or {}
                if src.get("type") == "base64":
                    url = f"data:{src.get('media_type') or ''};base64,{src.get('data') or ''}"
                elif src.get("type") == "url":
                    url = str(src.get("url") or "")
                else:
                    raise ValueError(f"messages.{i}.content.{j}.source.type: base64 or url required")
                parts.append({"type": "image_url", "image_url": {"url": url}})
            elif typ == "tool_use":
                if role != "assistant":
                    raise ValueError(f"messages.{i}: tool_use blocks belong to assistant messages")
                args = "{}" if b.get("input") is None else json.dumps(b["input"], separators=(",", ":"), ensure_ascii=False)
                calls.append({"id": str(b.get("id") or ""), "type": "function", "function": {"name": str(b.get("name") or ""), "arguments": args}})
            elif typ == "tool_result":
                if role != "user":
                    raise ValueError(f"messages.{i}: tool_result blocks belong to user messages")
                c = b.get("content")
                if isinstance(c, list):
                    c = "\n".join(str(z.get("text") or "") for z in c if isinstance(z, dict) and z.get("type") == "text")
                elif not isinstance(c, str):
                    c = ""
                if b.get("is_error") is True:
                    c = "Error: " + c
                results.append({"role": "tool", "tool_call_id": str(b.get("tool_use_id") or ""), "content": c})
            elif typ in ("thinking", "redacted_thinking"):
                pass
            else:
                raise ValueError(f"messages.{i}.content.{j}: unsupported block type {typ}")
        msgs.extend(results)
        if role == "assistant":
            if not text and not calls:
                continue
            am: dict[str, Any] = {"role": "assistant", "content": "".join(text)}
            if calls:
                am["tool_calls"] = calls
                if not text:
                    am["content"] = None
            msgs.append(am)
        elif parts:
            msgs.append({"role": "user", "content": parts})
    out["messages"] = msgs
    tools = inp.get("tools")
    if isinstance(tools, list) and tools:
        ot = []
        for i, t in enumerate(tools):
            if not isinstance(t, dict):
                raise ValueError(f"tools.{i}: must be an object")
            if "type" in t and t["type"] != "custom":
                raise ValueError(f"tools.{i}: server tool {t['type']} is not supported")
            if not t.get("name"):
                raise ValueError(f"tools.{i}.name: field required")
            fn = {"name": t["name"], "parameters": t.get("input_schema")}
            if isinstance(t.get("description"), str):
                fn["description"] = t["description"]
            ot.append({"type": "function", "function": fn})
        out["tools"] = ot
    tc = inp.get("tool_choice")
    if isinstance(tc, dict):
        if tc.get("type") == "auto":
            out["tool_choice"] = "auto"
        elif tc.get("type") == "any":
            out["tool_choice"] = "required"
        elif tc.get("type") == "none":
            out["tool_choice"] = "none"
        elif tc.get("type") == "tool":
            out["tool_choice"] = {"type": "function", "function": {"name": tc.get("name")}}
    return out


def models_list(data: list[dict]) -> dict:
    return {"data": [{"type": "model", "id": m["id"], "display_name": m["id"], "created_at": "1970-01-01T00:00:00Z"} for m in data],
            "has_more": False, "first_id": data[0]["id"] if data else None, "last_id": data[-1]["id"] if data else None}


def _tool_input(s: str) -> Any:
    try:
        v = json.loads(s) if s else {}
    except ValueError:
        return {}
    return v if isinstance(v, dict) else {}


def _usage(u: dict | None) -> dict:
    u = u or {}
    return {"input_tokens": u.get("prompt_tokens") or 0, "output_tokens": u.get("completion_tokens") or 0}


async def serve(req: Any, t: Any, after: int, stream: bool, headers: dict, json_resp: Callable, err_status: Callable):
    """Render a task as an Anthropic response (see Go serveAnthropic)."""
    from ._http import web
    msg_id = "msg_" + t.id.removeprefix("task_")
    if not stream:
        text: list[str] = []
        calls: list[dict] = []
        st: dict[str, Any] = {"done": None, "fail": None}

        async def collect(e):
            d = e.get("data") or {}
            if e["type"] == "task.delta":
                text.append(d.get("text") or "")
                for c in d.get("tool_calls") or []:
                    i = c.get("index") if _is_int(c.get("index")) else 0
                    while len(calls) <= i:
                        calls.append({"id": "", "name": "", "args": ""})
                    if c.get("id"):
                        calls[i]["id"] = c["id"]
                    fn = c.get("function") or {}
                    calls[i]["name"] += fn.get("name") or ""
                    calls[i]["args"] += fn.get("arguments") or ""
            elif e["type"] == "task.done":
                st["done"] = d
            else:
                st["fail"] = d
        await t.stream(0, None, collect)
        if st["fail"] is not None:
            f = st["fail"]
            s = err_status(f.get("code"))
            return json_resp(s, error_body(s, f.get("code"), f.get("message")))
        content: list[dict] = []
        if "".join(text):
            content.append({"type": "text", "text": "".join(text)})
        for c in calls:
            content.append({"type": "tool_use", "id": c["id"], "name": c["name"], "input": _tool_input(c["args"])})
        d = st["done"] or {}
        return json_resp(200, {"id": msg_id, "type": "message", "role": "assistant", "model": t.req_model, "content": content,
                               "stop_reason": stop_reason(d.get("finish_reason")), "stop_sequence": None, "usage": _usage(d.get("usage"))}, headers)

    resp = web.StreamResponse(status=200, headers={"Content-Type": "text/event-stream", "Cache-Control": "no-cache", "X-Accel-Buffering": "no", **headers})
    await resp.prepare(req)
    # Events derived from one task event are written as a group; only the last
    # carries that event's seq as id, so Last-Event-ID names a whole group.
    state: dict[str, Any] = {"cur": 0, "idx": -1, "open": "", "tools": {}}
    group: list[tuple[str, Any]] = []

    def send(typ, data):
        group.append((typ, data))

    async def flush():
        g = list(group)
        group.clear()
        cur = state["cur"]
        if after > 0 and cur <= after:
            return
        out = []
        for i, (typ, data) in enumerate(g):
            idl = f"id: {cur}\n" if i == len(g) - 1 and cur > 0 else ""
            out.append(f"event: {typ}\n{idl}data: {canonical_json(data)}\n\n")
        if out:
            await resp.write("".join(out).encode())

    # Content-block state (audit finding 5): Anthropic blocks are strictly
    # sequential (one open block, no delta after content_block_stop) while
    # OpenAI tool_call deltas may interleave. A live tool block stays open
    # until task.done; content for other blocks arriving meanwhile is buffered
    # and emitted, complete, at done. A text block can always be closed.
    blk = {"live": "", "ti": -1, "pending": [], "ptool": {}}

    def tool_id(ti, cid):
        return cid or f"toolu_{t.id.removeprefix('task_')}_{ti}"

    def start(cb):
        state["idx"] += 1
        send("content_block_start", {"type": "content_block_start", "index": state["idx"], "content_block": cb})

    def text_delta(x):
        send("content_block_delta", {"type": "content_block_delta", "index": state["idx"], "delta": {"type": "text_delta", "text": x}})

    def args_delta(x):
        send("content_block_delta", {"type": "content_block_delta", "index": state["idx"], "delta": {"type": "input_json_delta", "partial_json": x}})

    def stop():
        send("content_block_stop", {"type": "content_block_stop", "index": state["idx"]})

    def close_live():
        if blk["live"]:
            stop()
            blk["live"], blk["ti"] = "", -1

    def finish_blocks():
        close_live()
        for b in blk["pending"]:
            if b["tool"]:
                start({"type": "tool_use", "id": b["id"], "name": b["name"], "input": {}})
                if b["buf"]:
                    args_delta(b["buf"])
            else:
                start({"type": "text", "text": ""})
                text_delta(b["buf"])
            stop()
        blk["pending"] = []

    def on_text(x):
        if blk["live"] == "tool":
            p = blk["pending"]
            if p and not p[-1]["tool"]:
                p[-1]["buf"] += x
            else:
                p.append({"tool": False, "id": "", "name": "", "buf": x})
            return
        if not blk["live"]:
            start({"type": "text", "text": ""})
            blk["live"] = "text"
        text_delta(x)

    def on_tool(ti, c):
        fn = c.get("function") or {}
        args = fn.get("arguments") or ""
        if blk["live"] == "tool" and blk["ti"] == ti:
            if args:
                args_delta(args)
            return
        p = blk["ptool"].get(ti)
        if p is not None:
            p["name"] = p["name"] or fn.get("name") or ""
            p["buf"] += args
            return
        if blk["live"] == "tool":
            b = {"tool": True, "id": tool_id(ti, c.get("id")), "name": fn.get("name") or "", "buf": args}
            blk["pending"].append(b)
            blk["ptool"][ti] = b
            return
        close_live()
        start({"type": "tool_use", "id": tool_id(ti, c.get("id")), "name": fn.get("name") or "", "input": {}})
        blk["live"], blk["ti"] = "tool", ti
        if args:
            args_delta(args)

    send("message_start", {"type": "message_start", "message": {"id": msg_id, "type": "message", "role": "assistant", "model": t.req_model,
                                                                "content": [], "stop_reason": None, "stop_sequence": None,
                                                                "usage": {"input_tokens": 0, "output_tokens": 0}}})
    await flush()

    async def keepalive():
        await resp.write(b'event: ping\ndata: {"type":"ping"}\n\n')

    async def emit(e):
        state["cur"] = e["seq"]
        d = e.get("data") or {}
        if e["type"] == "task.delta":
            if d.get("text"):
                on_text(d["text"])
            for c in d.get("tool_calls") or []:
                on_tool(c.get("index") if _is_int(c.get("index")) else 0, c)
        elif e["type"] == "task.done":
            finish_blocks()
            u = d.get("usage")
            send("message_delta", {"type": "message_delta", "delta": {"stop_reason": stop_reason(d.get("finish_reason")), "stop_sequence": None},
                                   "usage": _usage(u) if u else {"output_tokens": 0}})
            send("message_stop", {"type": "message_stop"})
        else:
            send("error", {"type": "error", "error": {"type": error_type(err_status(d.get("code"))), "message": d.get("message"), "lmbp_code": d.get("code")}})
        await flush()
    try:
        await t.stream(0, keepalive, emit)
    except (ConnectionError, RuntimeError):
        return None
    await resp.write_eof()
    return None
