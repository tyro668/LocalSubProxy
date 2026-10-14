//! Anthropic Messages compatibility (/v1/messages, LMBP spec §6.7): request translation
//! to the OpenAI chat shape and rendering of task events as an Anthropic message or
//! named-event SSE stream. Port of sdk/node/src/anthropic.ts.
use crate::gateway::{self, Guard, SE};
use crate::http::{Abort, Res};
use crate::json::{self, get, truthy, Obj};
use crate::obj;
use serde_json::Value;
use std::collections::HashMap;

pub fn error_type(status: u16) -> &'static str {
    match status {
        400 | 422 => "invalid_request_error",
        401 => "authentication_error",
        403 => "permission_error",
        404 => "not_found_error",
        413 => "request_too_large",
        429 => "rate_limit_error",
        503 | 529 => "overloaded_error",
        _ => "api_error",
    }
}

pub fn error_body(status: u16, code: &str, message: &str) -> Value {
    obj! {"type" => "error", "error" => obj!{"type" => error_type(status), "message" => message, "lmbp_code" => code}}
}

fn stop_reason(f: &Value) -> &'static str {
    match f.as_str() {
        Some("length") => "max_tokens",
        Some("tool_calls") => "tool_use",
        _ => "end_turn",
    }
}

fn s(v: &Value) -> String {
    match v {
        Value::Null => String::new(),
        Value::String(x) => x.clone(),
        o => json::stringify(o),
    }
}

/// Translate a Messages request into a chat-completions request; Err → 400.
pub fn to_openai(inp: &Obj) -> Result<Value, String> {
    let n = &Value::Null;
    if !matches!(inp.get("model"), Some(Value::String(m)) if !m.is_empty()) {
        return Err("model: field required".into());
    }
    let mt = inp.get("max_tokens").unwrap_or(n);
    if !json::is_int(mt) || mt.as_f64().unwrap() < 1.0 {
        return Err("max_tokens: field required (positive integer)".into());
    }
    if inp.contains_key("lmbp_e2ee") {
        return Err("lmbp_e2ee is only supported on /v1/chat/completions".into());
    }
    let mut out = obj! {"model" => inp["model"].clone(), "max_tokens" => mt.clone(), "stream" => inp.get("stream") == Some(&Value::Bool(true))};
    let om = out.as_object_mut().unwrap();
    for k in ["temperature", "top_p"] {
        if let Some(v) = inp.get(k).filter(|v| !v.is_null()) {
            om.insert(k.into(), v.clone());
        }
    }
    if let Some(v) = inp.get("stop_sequences").filter(|v| !v.is_null()) {
        om.insert("stop".into(), v.clone());
    }
    let mut msgs = vec![];
    match inp.get("system").unwrap_or(n) {
        Value::String(ss) => {
            if !ss.is_empty() {
                msgs.push(obj! {"role" => "system", "content" => ss.clone()});
            }
        }
        Value::Array(l) => {
            let mut parts = vec![];
            for b in l {
                if get(b, "type").as_str() != Some("text") {
                    return Err("system: only text blocks are supported".into());
                }
                parts.push(s(get(b, "text")));
            }
            msgs.push(obj! {"role" => "system", "content" => parts.join("\n\n")});
        }
        Value::Null => {}
        _ => return Err("system: must be a string or an array of text blocks".into()),
    }
    let list = inp.get("messages").and_then(|m| m.as_array()).filter(|l| !l.is_empty()).ok_or("messages: at least one message is required")?;
    for (i, mo) in list.iter().enumerate() {
        let role = get(mo, "role").as_str().unwrap_or("");
        if role != "user" && role != "assistant" {
            return Err(format!("messages.{}.role: must be user or assistant", i));
        }
        let content = get(mo, "content");
        if let Value::String(cs) = content {
            msgs.push(obj! {"role" => role, "content" => cs.clone()});
            continue;
        }
        let Some(blocks) = content.as_array() else { return Err(format!("messages.{}.content: must be a string or an array of blocks", i)) };
        let (mut parts, mut calls, mut results, mut text) = (vec![], vec![], vec![], vec![]);
        for (j, b) in blocks.iter().enumerate() {
            let ty = get(b, "type");
            match ty.as_str().unwrap_or("") {
                "text" => {
                    let t = s(get(b, "text"));
                    parts.push(obj! {"type" => "text", "text" => t.clone()});
                    text.push(t);
                }
                "image" => {
                    let src = get(b, "source");
                    let url = match get(src, "type").as_str() {
                        Some("base64") => format!("data:{};base64,{}", s(get(src, "media_type")), s(get(src, "data"))),
                        Some("url") => s(get(src, "url")),
                        _ => return Err(format!("messages.{}.content.{}.source.type: base64 or url required", i, j)),
                    };
                    parts.push(obj! {"type" => "image_url", "image_url" => obj!{"url" => url}});
                }
                "tool_use" => {
                    if role != "assistant" {
                        return Err(format!("messages.{}: tool_use blocks belong to assistant messages", i));
                    }
                    let input = get(b, "input");
                    calls.push(obj! {"id" => s(get(b, "id")), "type" => "function", "function" => obj!{"name" => s(get(b, "name")),
                        "arguments" => if input.is_null() { "{}".to_string() } else { json::stringify(input) }}});
                }
                "tool_result" => {
                    if role != "user" {
                        return Err(format!("messages.{}: tool_result blocks belong to user messages", i));
                    }
                    let mut c = match get(b, "content") {
                        Value::String(bs) => bs.clone(),
                        Value::Array(zl) => zl.iter().filter(|z| get(z, "type").as_str() == Some("text")).map(|z| s(get(z, "text"))).collect::<Vec<_>>().join("\n"),
                        _ => String::new(),
                    };
                    if get(b, "is_error") == &Value::Bool(true) {
                        c = format!("Error: {}", c);
                    }
                    results.push(obj! {"role" => "tool", "tool_call_id" => s(get(b, "tool_use_id")), "content" => c});
                }
                "thinking" | "redacted_thinking" => {}
                _ => {
                    let d = if ty.is_null() { "undefined".to_string() } else { s(ty) };
                    return Err(format!("messages.{}.content.{}: unsupported block type {}", i, j, d));
                }
            }
        }
        msgs.extend(results);
        if role == "assistant" {
            if text.is_empty() && calls.is_empty() {
                continue;
            }
            let mut am = obj! {"role" => "assistant", "content" => text.join("")};
            if !calls.is_empty() {
                let empty = text.is_empty();
                let m = am.as_object_mut().unwrap();
                m.insert("tool_calls".into(), Value::Array(calls));
                if empty {
                    m.insert("content".into(), Value::Null);
                }
            }
            msgs.push(am);
        } else if !parts.is_empty() {
            msgs.push(obj! {"role" => "user", "content" => parts});
        }
    }
    om.insert("messages".into(), Value::Array(msgs));
    if let Some(tools) = inp.get("tools").and_then(|t| t.as_array()).filter(|t| !t.is_empty()) {
        let mut ot = vec![];
        for (i, t) in tools.iter().enumerate() {
            let ty = get(t, "type");
            if json::has(t, "type") && ty.as_str() != Some("custom") {
                return Err(format!("tools.{}: server tool {} is not supported", i, s(ty)));
            }
            if !truthy(get(t, "name")) {
                return Err(format!("tools.{}.name: field required", i));
            }
            let mut f = obj! {"name" => get(t, "name").clone(), "parameters" => get(t, "input_schema").clone()};
            if let Some(d) = get(t, "description").as_str() {
                f.as_object_mut().unwrap().insert("description".into(), d.into());
            }
            ot.push(obj! {"type" => "function", "function" => f});
        }
        om.insert("tools".into(), Value::Array(ot));
    }
    if let Some(tc) = inp.get("tool_choice").filter(|t| t.is_object()) {
        let v = match get(tc, "type").as_str() {
            Some("auto") => Some(Value::from("auto")),
            Some("any") => Some("required".into()),
            Some("none") => Some("none".into()),
            Some("tool") => Some(obj! {"type" => "function", "function" => obj!{"name" => get(tc, "name").clone()}}),
            _ => None,
        };
        if let Some(v) = v {
            om.insert("tool_choice".into(), v);
        }
    }
    Ok(out)
}

pub fn models_list(data: &[Value]) -> Value {
    let out: Vec<Value> = data.iter().map(|m| obj! {"type" => "model", "id" => get(m, "id").clone(), "display_name" => get(m, "id").clone(), "created_at" => "1970-01-01T00:00:00Z"}).collect();
    obj! {"data" => out, "has_more" => false, "first_id" => data.first().map(|m| get(m, "id").clone()).unwrap_or(Value::Null),
        "last_id" => data.last().map(|m| get(m, "id").clone()).unwrap_or(Value::Null)}
}

fn tool_input(st: &str) -> Value {
    if st.is_empty() {
        return obj! {};
    }
    match json::parse(st) {
        Some(v @ (Value::Object(_) | Value::Array(_))) => v,
        _ => obj! {},
    }
}

fn status_of(code: &Value) -> u16 {
    match code.as_str() {
        Some("OUTCOME_UNKNOWN") => 502,
        Some(c) => gateway::err_status(c),
        None => 502,
    }
}

fn usage_of(u: &Value) -> Value {
    let i = get(u, "prompt_tokens");
    let o = get(u, "completion_tokens");
    obj! {"input_tokens" => if i.is_null() { Value::from(0) } else { i.clone() }, "output_tokens" => if o.is_null() { Value::from(0) } else { o.clone() }}
}

/// Render a task as an Anthropic response (see Go serveAnthropic).
pub fn serve(g: &mut Guard, res: &mut Res, tk: u64, after: i64, stream: bool, signal: &Abort) {
    let t = gateway::tinfo(g, tk);
    let msg_id = format!("msg_{}", t.id.strip_prefix("task_").unwrap_or(&t.id));
    if !stream {
        let mut text = String::new();
        let mut calls = vec![];
        let (mut done, mut fail) = (None, None);
        let ok = gateway::task_stream(g, tk, 0, signal, false, &mut |_g, se| {
            let SE::Ev(e) = se else { return true };
            if e.ty == "task.delta" {
                let tx = get(&e.data, "text");
                if truthy(tx) {
                    text.push_str(&s(tx));
                }
                gateway::merge_calls(&mut calls, get(&e.data, "tool_calls"));
            } else if e.ty == "task.done" {
                done = Some(e.data.clone());
            } else {
                fail = Some(e.data.clone());
            }
            true
        });
        if !ok {
            return;
        }
        if let Some(f) = fail {
            let st = status_of(get(&f, "code"));
            let b = obj! {"type" => "error", "error" => obj!{"type" => error_type(st), "message" => get(&f, "message").as_str().map(Value::from).unwrap_or(Value::Null), "lmbp_code" => s(get(&f, "code"))}};
            return gateway::json_res(g, res, st, &b);
        }
        let done = done.unwrap_or(Value::Null);
        let mut content = vec![];
        if !text.is_empty() {
            content.push(obj! {"type" => "text", "text" => text});
        }
        for c in &calls {
            let f = &c["function"];
            content.push(obj! {"type" => "tool_use", "id" => if get(c, "id").is_null() { Value::from("") } else { c["id"].clone() },
                "name" => f["name"].clone(), "input" => tool_input(f["arguments"].as_str().unwrap_or(""))});
        }
        let body = obj! {"id" => msg_id, "type" => "message", "role" => "assistant", "model" => t.req_model.clone(), "content" => content,
            "stop_reason" => stop_reason(get(&done, "finish_reason")), "stop_sequence" => Value::Null, "usage" => usage_of(get(&done, "usage"))};
        return gateway::json_res(g, res, 200, &body);
    }
    res.write_head(g, 200, &gateway::SSE_HEADERS);
    let mut st = Streamer { task_id: t.id.clone(), after, cur: 0, wrote: false, group: vec![], idx: -1, live_ti: -1, live_kind: "", blocks: vec![], pending: vec![], pending_tool: HashMap::new() };
    st.send("message_start", obj! {"type" => "message_start", "message" => obj!{"id" => msg_id, "type" => "message", "role" => "assistant", "model" => t.req_model.clone(),
        "content" => Value::Array(vec![]), "stop_reason" => Value::Null, "stop_sequence" => Value::Null, "usage" => obj!{"input_tokens" => 0, "output_tokens" => 0}}});
    st.flush(g, res);
    let ok = gateway::task_stream(g, tk, 0, signal, true, &mut |g, se| {
        let e = match se {
            SE::Keepalive => return res.write(g, "event: ping\ndata: {\"type\":\"ping\"}\n\n"),
            SE::Ev(e) => e,
        };
        st.cur = e.seq;
        if e.ty == "task.delta" {
            let d = &e.data;
            if truthy(get(d, "text")) {
                st.on_text(&s(get(d, "text")));
            }
            if let Some(tcs) = get(d, "tool_calls").as_array() {
                for c in tcs {
                    let ix = get(c, "index");
                    st.on_tool(if json::is_int(ix) { json::int(ix) } else { 0 }, c);
                }
            }
        } else if e.ty == "task.done" {
            st.finish_blocks();
            let u = get(&e.data, "usage");
            st.send("message_delta", obj! {"type" => "message_delta", "delta" => obj!{"stop_reason" => stop_reason(get(&e.data, "finish_reason")), "stop_sequence" => Value::Null},
                "usage" => if truthy(u) { usage_of(u) } else { obj!{"output_tokens" => 0} }});
            st.send("message_stop", obj! {"type" => "message_stop"});
        } else {
            let code = get(&e.data, "code").clone();
            st.send("error", obj! {"type" => "error", "error" => obj!{"type" => error_type(status_of(&code)), "message" => get(&e.data, "message").clone(), "lmbp_code" => code}});
        }
        st.flush(g, res)
    });
    if ok && !st.wrote {
        res.write(g, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n");
    }
    res.end(g, None);
}

struct Block {
    tool: bool,
    id: String,
    name: String,
    buf: String,
}

/// Named-event SSE renderer with the strictly sequential content-block state machine (audit finding 5).
struct Streamer {
    task_id: String,
    after: i64,
    cur: i64,
    wrote: bool,
    group: Vec<(String, Value)>,
    idx: i64,
    live_ti: i64,
    live_kind: &'static str,
    blocks: Vec<Block>,
    pending: Vec<usize>,
    pending_tool: HashMap<i64, usize>,
}

impl Streamer {
    fn send(&mut self, ty: &str, data: Value) {
        self.group.push((ty.into(), data));
    }
    fn flush(&mut self, g: &mut Guard, res: &mut Res) -> bool {
        let grp = std::mem::take(&mut self.group);
        if self.after > 0 && self.cur <= self.after {
            return true;
        }
        self.wrote = true;
        let mut b = String::new();
        let n = grp.len();
        for (i, (ty, d)) in grp.iter().enumerate() {
            b.push_str(&format!("event: {}\n", ty));
            if i == n - 1 && self.cur > 0 {
                b.push_str(&format!("id: {}\n", self.cur));
            }
            b.push_str(&format!("data: {}\n\n", json::stringify(d)));
        }
        b.is_empty() || res.write(g, &b)
    }
    fn tool_id(&self, ti: i64, id: &Value) -> String {
        if truthy(id) { s(id) } else { format!("toolu_{}_{}", self.task_id.strip_prefix("task_").unwrap_or(&self.task_id), ti) }
    }
    fn start(&mut self, cb: Value) {
        self.idx += 1;
        let i = self.idx;
        self.send("content_block_start", obj! {"type" => "content_block_start", "index" => i, "content_block" => cb});
    }
    fn text_delta(&mut self, st: &str) {
        let i = self.idx;
        self.send("content_block_delta", obj! {"type" => "content_block_delta", "index" => i, "delta" => obj!{"type" => "text_delta", "text" => st}});
    }
    fn args_delta(&mut self, st: &str) {
        let i = self.idx;
        self.send("content_block_delta", obj! {"type" => "content_block_delta", "index" => i, "delta" => obj!{"type" => "input_json_delta", "partial_json" => st}});
    }
    fn stop(&mut self) {
        let i = self.idx;
        self.send("content_block_stop", obj! {"type" => "content_block_stop", "index" => i});
    }
    fn close_live(&mut self) {
        if !self.live_kind.is_empty() {
            self.stop();
            self.live_kind = "";
            self.live_ti = -1;
        }
    }
    fn finish_blocks(&mut self) {
        self.close_live();
        let pend = std::mem::take(&mut self.pending);
        for i in pend {
            let (tool, id, name, buf) = { let b = &self.blocks[i]; (b.tool, b.id.clone(), b.name.clone(), b.buf.clone()) };
            if tool {
                self.start(obj! {"type" => "tool_use", "id" => id, "name" => name, "input" => obj!{}});
                if !buf.is_empty() {
                    self.args_delta(&buf);
                }
            } else {
                self.start(obj! {"type" => "text", "text" => ""});
                self.text_delta(&buf);
            }
            self.stop();
        }
    }
    fn new_block(&mut self, b: Block) -> usize {
        self.blocks.push(b);
        self.blocks.len() - 1
    }
    fn on_text(&mut self, st: &str) {
        if self.live_kind == "tool" {
            match self.pending.last().copied() {
                Some(i) if !self.blocks[i].tool => self.blocks[i].buf.push_str(st),
                _ => {
                    let i = self.new_block(Block { tool: false, id: String::new(), name: String::new(), buf: st.into() });
                    self.pending.push(i);
                }
            }
            return;
        }
        if self.live_kind.is_empty() {
            self.start(obj! {"type" => "text", "text" => ""});
            self.live_kind = "text";
        }
        self.text_delta(st);
    }
    fn on_tool(&mut self, ti: i64, c: &Value) {
        let f = get(c, "function");
        let args = if truthy(get(f, "arguments")) { s(get(f, "arguments")) } else { String::new() };
        let name = if truthy(get(f, "name")) { s(get(f, "name")) } else { String::new() };
        if self.live_kind == "tool" && self.live_ti == ti {
            if !args.is_empty() {
                self.args_delta(&args);
            }
            return;
        }
        if let Some(&p) = self.pending_tool.get(&ti) {
            if self.blocks[p].name.is_empty() {
                self.blocks[p].name = name;
            }
            self.blocks[p].buf.push_str(&args);
            return;
        }
        if self.live_kind == "tool" {
            let id = self.tool_id(ti, get(c, "id"));
            let i = self.new_block(Block { tool: true, id, name, buf: args });
            self.pending.push(i);
            self.pending_tool.insert(ti, i);
            return;
        }
        self.close_live();
        let id = self.tool_id(ti, get(c, "id"));
        self.start(obj! {"type" => "tool_use", "id" => id, "name" => name, "input" => obj!{}});
        self.live_kind = "tool";
        self.live_ti = ti;
        if !args.is_empty() {
            self.args_delta(&args);
        }
    }
}
