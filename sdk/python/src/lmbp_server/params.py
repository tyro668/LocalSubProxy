"""Request parameter validation (third audit #2), the same rules as the Go
reference (internal/lmbp/gateway/params.go) and the Node SDK, pinned by
spec/testvectors/params.json and conformance Z8. Every parameter is checked
before anything is stored, queued or signed; the payload to be signed is then
validated against the bundled copy of spec/schemas/task.run.payload.json.
Illegal values: 400 BAD_REQUEST with the top-level parameter in error.param
(OpenAI shape) / error.lmbp_param (Anthropic shape)."""
from __future__ import annotations

import json
import re
from importlib import resources
from typing import Any, Optional

from .jsonschema import SchemaSet

MAX_MESSAGES = 200
MAX_STOP_SEQUENCES = 4
MAX_STOP_LENGTH = 256
MAX_TOOLS = 128
TOOL_NAME = re.compile(r"^[A-Za-z0-9_-]{1,64}\Z")
BASE = "https://localsubproxy.dev/spec/lmbp/"
_schemas: Optional[SchemaSet] = None


def _schema_set() -> SchemaSet:
    global _schemas
    if _schemas is None:
        d = resources.files("lmbp_server") / "schemas"
        _schemas = SchemaSet([json.loads((d / n).read_text("utf-8")) for n in ("defs.json", "task.run.payload.json", "hello.json", "models.update.json", "task.done.json", "task.error.json")])
    return _schemas


class ParamError(ValueError):
    def __init__(self, param: str, message: str) -> None:
        super().__init__(message)
        self.param = param


def _perr(param, msg):
    return ParamError(param, f"{param}: {msg}")


def param_error_body(e: ParamError, anthropic: bool) -> dict:
    if anthropic:
        return {"type": "error", "error": {"type": "invalid_request_error", "message": str(e), "lmbp_code": "BAD_REQUEST", "lmbp_param": e.param}}
    return {"error": {"message": str(e), "type": "invalid_request_error", "code": "BAD_REQUEST", "param": e.param}}


def _present(m, k):
    return m.get(k) is not None


def _num(v):
    return isinstance(v, (int, float)) and not isinstance(v, bool)


def _int(v):
    return (isinstance(v, int) and not isinstance(v, bool)) or (isinstance(v, float) and v.is_integer())


def _g(f):
    return format(f, "g")


def _number_in(m, k, lo, hi):
    if not _present(m, k):
        return None
    f = m[k]
    if not _num(f) or f != f or f in (float("inf"), float("-inf")):
        raise _perr(k, "must be a number")
    if f < lo or f > hi:
        raise _perr(k, f"must be between {_g(lo)} and {_g(hi)}, got {_g(f)}")
    return f


def _positive_int(m, k):
    if not _present(m, k):
        return 0
    f = m[k]
    if not _int(f) or f < 1 or f > 2147483647:
        raise _perr(k, f"must be a positive integer, got {json.dumps(f)}")
    return int(f)


def _boolean(m, k):
    if _present(m, k) and not isinstance(m[k], bool):
        raise _perr(k, "must be a boolean")


def _stop_list(m, k, allow_string):
    if not _present(m, k):
        return None
    v = m[k]
    if isinstance(v, str):
        if not allow_string:
            raise _perr(k, "must be an array of strings")
        v = [v]
    elif not isinstance(v, list):
        raise _perr(k, "must be a string or an array of strings" if allow_string else "must be an array of strings")
    if not 1 <= len(v) <= MAX_STOP_SEQUENCES:
        raise _perr(k, f"must have 1 to {MAX_STOP_SEQUENCES} sequences, got {len(v)}")
    for i, s in enumerate(v):
        if not isinstance(s, str) or s == "" or len(s) > MAX_STOP_LENGTH:
            raise _perr(k, f"sequence {i} must be a non-empty string of at most {MAX_STOP_LENGTH} characters")
    return list(v)


def validate_chat_params(m: dict) -> dict:
    """Check an OpenAI chat body; return normalized sampling params or raise ParamError."""
    if not isinstance(m.get("model"), str) or not m["model"]:
        raise _perr("model", "field required (non-empty string)")
    _boolean(m, "stream")
    p: dict[str, Any] = {}
    p["temperature"] = _number_in(m, "temperature", 0, 2)
    p["top_p"] = _number_in(m, "top_p", 0, 1)
    mt = _positive_int(m, "max_tokens")
    mct = _positive_int(m, "max_completion_tokens")
    p["max_tokens"] = mt or mct
    if _present(m, "n"):
        if not _int(m["n"]):
            raise _perr("n", "must be an integer")
        if m["n"] != 1:
            raise _perr("n", f"only n=1 is supported, got {_g(m['n'])}")
    p["stop"] = _stop_list(m, "stop", True)
    if "lmbp_e2ee" not in m:
        _openai_messages(m)
    names = _openai_tools(m)
    _openai_tool_choice(m, names)
    return p


def _openai_messages(m):
    if not _present(m, "messages"):
        raise _perr("messages", "field required")
    lst = m["messages"]
    if not isinstance(lst, list):
        raise _perr("messages", "must be an array")
    if not 1 <= len(lst) <= MAX_MESSAGES:
        raise _perr("messages", f"1-{MAX_MESSAGES} messages required, got {len(lst)}")
    for i, msg in enumerate(lst):
        if not isinstance(msg, dict):
            raise _perr("messages", f"message {i} must be an object")
        role = msg.get("role")
        if role not in ("system", "user", "assistant", "tool", "developer"):
            raise _perr("messages", f"message {i}: unsupported role {json.dumps(role)}")
        c = msg.get("content")
        if not (c is None or isinstance(c, (str, list))):
            raise _perr("messages", f"message {i}: content must be a string, an array of content parts or null")
        if role == "tool":
            if not isinstance(msg.get("tool_call_id"), str) or not msg["tool_call_id"]:
                raise _perr("messages", f"message {i}: tool messages need tool_call_id")
            if c is None:
                raise _perr("messages", f"message {i}: tool messages need content")
        if msg.get("tool_calls") is not None:
            tcs = msg["tool_calls"]
            if not isinstance(tcs, list) or role != "assistant":
                raise _perr("messages", f"message {i}: tool_calls must be an array on an assistant message")
            for j, tc in enumerate(tcs):
                if not isinstance(tc, dict) or not isinstance(tc.get("function"), dict):
                    raise _perr("messages", f"message {i}: tool_calls[{j}] needs a function")


def _openai_tools(m):
    names: set[str] = set()
    if not _present(m, "tools"):
        return names
    tl = m["tools"]
    if not isinstance(tl, list):
        raise _perr("tools", "must be an array")
    if len(tl) > MAX_TOOLS:
        raise _perr("tools", f"at most {MAX_TOOLS} tools, got {len(tl)}")
    for i, t in enumerate(tl):
        if not isinstance(t, dict) or t.get("type") != "function":
            raise _perr("tools", f"tool {i} must have type function")
        fn = t.get("function")
        if not isinstance(fn, dict):
            raise _perr("tools", f"tool {i} needs a function object")
        name = fn.get("name")
        if not isinstance(name, str) or not TOOL_NAME.match(name):
            raise _perr("tools", f"tool {i}: function name must match ^[A-Za-z0-9_-]{{1,64}}$, got {json.dumps(name)}")
        if _present(fn, "description") and not isinstance(fn["description"], str):
            raise _perr("tools", f"tool {i}: description must be a string")
        if _present(fn, "parameters") and not isinstance(fn["parameters"], dict):
            raise _perr("tools", f"tool {i}: parameters must be a JSON Schema object")
        names.add(name)
    return names


def _openai_tool_choice(m, names):
    if not _present(m, "tool_choice"):
        return
    v = m["tool_choice"]
    if isinstance(v, str):
        if v == "none":
            return
        if v not in ("auto", "required"):
            raise _perr("tool_choice", f"must be none, auto, required or a function object, got {json.dumps(v)}")
    elif isinstance(v, dict):
        fn = v.get("function") if isinstance(v.get("function"), dict) else {}
        name = fn.get("name")
        if v.get("type") != "function" or not isinstance(name, str) or not name:
            raise _perr("tool_choice", 'object form must be {"type":"function","function":{"name":...}}')
        if name not in names:
            raise _perr("tool_choice", f"names function {json.dumps(name)} that is not in tools")
    else:
        raise _perr("tool_choice", "must be a string or an object")
    if not names:
        raise _perr("tool_choice", "requires tools")


def validate_anthropic_params(m: dict) -> None:
    """Check a Messages body with Anthropic's names and ranges; raise ParamError."""
    if not isinstance(m.get("model"), str) or not m["model"]:
        raise _perr("model", "field required (non-empty string)")
    if not _present(m, "max_tokens"):
        raise _perr("max_tokens", "field required (positive integer)")
    _positive_int(m, "max_tokens")
    _boolean(m, "stream")
    _number_in(m, "temperature", 0, 1)
    _number_in(m, "top_p", 0, 1)
    _stop_list(m, "stop_sequences", False)
    if not _present(m, "messages"):
        raise _perr("messages", "field required")
    lst = m["messages"]
    if not isinstance(lst, list):
        raise _perr("messages", "must be an array")
    if not 1 <= len(lst) <= MAX_MESSAGES:
        raise _perr("messages", f"1-{MAX_MESSAGES} messages required, got {len(lst)}")
    for i, msg in enumerate(lst):
        if not isinstance(msg, dict):
            raise _perr("messages", f"message {i} must be an object")
        if msg.get("role") not in ("user", "assistant"):
            raise _perr("messages", f"message {i}: role must be user or assistant")
        if not isinstance(msg.get("content"), (str, list)):
            raise _perr("messages", f"message {i}: content must be a string or an array of blocks")
    names: set[str] = set()
    if _present(m, "tools"):
        tl = m["tools"]
        if not isinstance(tl, list):
            raise _perr("tools", "must be an array")
        if len(tl) > MAX_TOOLS:
            raise _perr("tools", f"at most {MAX_TOOLS} tools, got {len(tl)}")
        for i, t in enumerate(tl):
            if not isinstance(t, dict):
                raise _perr("tools", f"tool {i} must be an object")
            if _present(t, "type") and t["type"] != "custom":
                raise _perr("tools", f"tool {i}: server tool {t['type']} is not supported")
            name = t.get("name")
            if not isinstance(name, str) or not TOOL_NAME.match(name):
                raise _perr("tools", f"tool {i}: name must match ^[A-Za-z0-9_-]{{1,64}}$, got {json.dumps(name)}")
            if not isinstance(t.get("input_schema"), dict):
                raise _perr("tools", f"tool {i}: input_schema (object) is required")
            names.add(name)
    if _present(m, "tool_choice"):
        tc = m["tool_choice"]
        if not isinstance(tc, dict):
            raise _perr("tool_choice", "must be an object")
        ty = tc.get("type")
        if ty == "none":
            return
        if ty == "tool":
            if not isinstance(tc.get("name"), str) or not tc["name"]:
                raise _perr("tool_choice", "type tool needs a name")
            if tc["name"] not in names:
                raise _perr("tool_choice", f"names tool {json.dumps(tc['name'])} that is not in tools")
        elif ty not in ("auto", "any"):
            raise _perr("tool_choice", f"type must be auto, any, tool or none, got {json.dumps(ty)}")
        if not names:
            raise _perr("tool_choice", "requires tools")


def device_data_problem(inp) -> str:
    """Validate a device data frame's envelope and, for task.done /
    task.error, its body against task.done.json / task.error.json: "" when
    acceptable (audit3 P2-6: checked before the seq is accepted, persisted or
    ACKed)."""
    typ = inp.get("type")
    tid, seq, data = inp.get("task_id"), inp.get("seq"), inp.get("data")
    if not isinstance(tid, str) or not tid:
        return f"{typ} without task_id"
    if not isinstance(seq, int) or isinstance(seq, bool) or seq < 1:
        return f"{typ} seq must be an integer >= 1"
    if not isinstance(data, dict):
        return f"{typ} data must be a JSON object"
    if typ not in ("task.done", "task.error"):
        return ""
    e = _schema_set().validate(BASE + typ + ".json", json.loads(json.dumps(data)))
    if not e:
        return ""
    path, msg = e
    return typ + ("." + ".".join(map(str, path)) if path else "") + ": " + msg


def announce_problem(data, hello: bool) -> str:
    """Validate hello / models.update data against its schema (spec §7.4):
    "" when valid, else a message naming the first bad field."""
    name = "hello" if hello else "models.update"
    e = _schema_set().validate(BASE + name + ".json", json.loads(json.dumps(data)))
    if not e:
        return ""
    path, msg = e
    return name + ("." + ".".join(map(str, path)) if path else "") + ": " + msg


def validate_task_run(payload: dict) -> None:
    """Validate the payload about to be signed; raise ParamError."""
    e = _schema_set().validate(BASE + "task.run.payload.json", json.loads(json.dumps(payload)))
    if e:
        path, msg = e
        raise ParamError(str(path[0]) if path else "", "request does not satisfy the task.run schema at /" + "/".join(map(str, path)) + ": " + msg)
