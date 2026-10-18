//! Request parameter validation, same rules as the Go reference and Node/Python SDKs
//! (spec/testvectors/params.json, conformance Z8).
use crate::json::{self, get, Obj};
use crate::lmbp;
use crate::obj;
use crate::schema::Schema;
use serde_json::Value;
use std::collections::HashSet;
use std::sync::OnceLock;

pub const MAX_STOP_SEQUENCES: usize = 4;
pub const MAX_STOP_LENGTH: usize = 256;
pub const MAX_TOOLS: usize = 128;
const BASE: &str = "https://localsubproxy.dev/spec/lmbp/";
const ROLES: [&str; 5] = ["system", "user", "assistant", "tool", "developer"];

#[derive(Debug, Clone)]
pub struct ParamError {
    pub param: String,
    pub message: String,
}

type R<T> = Result<T, ParamError>;

fn perr<T>(param: &str, msg: impl AsRef<str>) -> R<T> {
    Err(ParamError { param: param.into(), message: format!("{}: {}", param, msg.as_ref()) })
}

pub fn param_error_body(e: &ParamError, anthropic: bool) -> Value {
    if anthropic {
        obj! {"type" => "error", "error" => obj!{"type" => "invalid_request_error", "message" => e.message.clone(), "lmbp_code" => "BAD_REQUEST", "lmbp_param" => e.param.clone()}}
    } else {
        obj! {"error" => obj!{"message" => e.message.clone(), "type" => "invalid_request_error", "code" => "BAD_REQUEST", "param" => e.param.clone()}}
    }
}

pub fn present(m: &Obj, k: &str) -> bool {
    m.get(k).map(|v| !v.is_null()).unwrap_or(false)
}

fn fmt(v: &Value) -> String {
    json::stringify(v)
}

fn fnum(f: f64) -> String {
    json::js_num(f)
}

fn tool_name_ok(s: &str) -> bool {
    !s.is_empty() && s.len() <= 64 && s.bytes().all(|c| c.is_ascii_alphanumeric() || c == b'_' || c == b'-')
}

fn number_in(m: &Obj, k: &str, lo: f64, hi: f64) -> R<Option<Value>> {
    if !present(m, k) {
        return Ok(None);
    }
    let f = &m[k];
    let Some(d) = f.as_f64() else { return perr(k, "must be a number") };
    if d < lo || d > hi {
        return perr(k, format!("must be between {} and {}, got {}", fnum(lo), fnum(hi), fmt(f)));
    }
    Ok(Some(f.clone()))
}

fn positive_int(m: &Obj, k: &str) -> R<i64> {
    if !present(m, k) {
        return Ok(0);
    }
    let f = &m[k];
    let d = f.as_f64().unwrap_or(0.0);
    if !json::is_int(f) || d < 1.0 || d > 2147483647.0 {
        return perr(k, format!("must be a positive integer, got {}", fmt(f)));
    }
    Ok(json::int(f))
}

fn boolean(m: &Obj, k: &str) -> R<()> {
    if present(m, k) && !m[k].is_boolean() {
        return perr(k, "must be a boolean");
    }
    Ok(())
}

fn stop_list(m: &Obj, k: &str, allow_string: bool) -> R<Option<Vec<Value>>> {
    if !present(m, k) {
        return Ok(None);
    }
    let list = match &m[k] {
        Value::String(_) if allow_string => vec![m[k].clone()],
        Value::String(_) => return perr(k, "must be an array of strings"),
        Value::Array(a) => a.clone(),
        _ => return perr(k, if allow_string { "must be a string or an array of strings" } else { "must be an array of strings" }),
    };
    if list.is_empty() || list.len() > MAX_STOP_SEQUENCES {
        return perr(k, format!("must have 1 to {} sequences, got {}", MAX_STOP_SEQUENCES, list.len()));
    }
    for (i, s) in list.iter().enumerate() {
        match s.as_str() {
            Some(st) if !st.is_empty() && st.chars().count() <= MAX_STOP_LENGTH => {}
            _ => return perr(k, format!("sequence {} must be a non-empty string of at most {} characters", i, MAX_STOP_LENGTH)),
        }
    }
    Ok(Some(list))
}

/// Normalized sampling parameters (None = absent; max_tokens 0 = absent).
#[derive(Default, Clone)]
pub struct Sampling {
    pub temperature: Option<Value>,
    pub top_p: Option<Value>,
    pub max_tokens: i64,
    pub stop: Option<Vec<Value>>,
}

fn non_empty_str(v: Option<&Value>) -> bool {
    matches!(v, Some(Value::String(s)) if !s.is_empty())
}

fn undef_or(m: &Obj, k: &str) -> String {
    if m.contains_key(k) { fmt(&m[k]) } else { "undefined".into() }
}

pub fn validate_chat_params(m: &Obj) -> R<Sampling> {
    if !non_empty_str(m.get("model")) {
        return perr("model", "field required (non-empty string)");
    }
    boolean(m, "stream")?;
    let mut p = Sampling { temperature: number_in(m, "temperature", 0.0, 2.0)?, top_p: number_in(m, "top_p", 0.0, 1.0)?, ..Default::default() };
    let mt = positive_int(m, "max_tokens")?;
    let mct = positive_int(m, "max_completion_tokens")?;
    p.max_tokens = if mt != 0 { mt } else { mct };
    if present(m, "n") {
        if !json::is_int(&m["n"]) {
            return perr("n", "must be an integer");
        }
        if m["n"].as_f64() != Some(1.0) {
            return perr("n", format!("only n=1 is supported, got {}", fmt(&m["n"])));
        }
    }
    p.stop = stop_list(m, "stop", true)?;
    if !m.contains_key("lmbp_e2ee") {
        validate_openai_messages(m)?;
    }
    let names = validate_openai_tools(m)?;
    validate_openai_tool_choice(m, &names)?;
    Ok(p)
}

fn validate_openai_messages(m: &Obj) -> R<()> {
    if !present(m, "messages") {
        return perr("messages", "field required");
    }
    let Some(list) = m["messages"].as_array() else { return perr("messages", "must be an array") };
    if list.is_empty() || list.len() > lmbp::MAX_MESSAGES {
        return perr("messages", format!("1-{} messages required, got {}", lmbp::MAX_MESSAGES, list.len()));
    }
    for (i, msg) in list.iter().enumerate() {
        let Some(msg) = msg.as_object() else { return perr("messages", format!("message {} must be an object", i)) };
        let role = msg.get("role").and_then(|r| r.as_str());
        if !role.map(|r| ROLES.contains(&r)).unwrap_or(false) {
            return perr("messages", format!("message {}: unsupported role {}", i, undef_or(msg, "role")));
        }
        let c = msg.get("content").unwrap_or(&json::NULL);
        if !(c.is_null() || c.is_string() || c.is_array()) {
            return perr("messages", format!("message {}: content must be a string, an array of content parts or null", i));
        }
        if role == Some("tool") {
            if !non_empty_str(msg.get("tool_call_id")) {
                return perr("messages", format!("message {}: tool messages need tool_call_id", i));
            }
            if c.is_null() {
                return perr("messages", format!("message {}: tool messages need content", i));
            }
        }
        if let Some(tc) = msg.get("tool_calls") {
            if !tc.is_null() {
                let Some(tcs) = tc.as_array().filter(|_| role == Some("assistant")) else {
                    return perr("messages", format!("message {}: tool_calls must be an array on an assistant message", i));
                };
                for (j, t) in tcs.iter().enumerate() {
                    if !get(t, "function").is_object() {
                        return perr("messages", format!("message {}: tool_calls[{}] needs a function", i, j));
                    }
                }
            }
        }
    }
    Ok(())
}

fn validate_openai_tools(m: &Obj) -> R<HashSet<String>> {
    let mut names = HashSet::new();
    if !present(m, "tools") {
        return Ok(names);
    }
    let Some(tools) = m["tools"].as_array() else { return perr("tools", "must be an array") };
    if tools.len() > MAX_TOOLS {
        return perr("tools", format!("at most {} tools, got {}", MAX_TOOLS, tools.len()));
    }
    for (i, t) in tools.iter().enumerate() {
        if get(t, "type").as_str() != Some("function") {
            return perr("tools", format!("tool {} must have type function", i));
        }
        let Some(f) = get(t, "function").as_object() else { return perr("tools", format!("tool {} needs a function object", i)) };
        let name = f.get("name").and_then(|n| n.as_str()).filter(|n| tool_name_ok(n));
        let Some(name) = name else {
            return perr("tools", format!("tool {}: function name must match ^[A-Za-z0-9_-]{{1,64}}$, got {}", i, undef_or(f, "name")));
        };
        if present(f, "description") && !f["description"].is_string() {
            return perr("tools", format!("tool {}: description must be a string", i));
        }
        if present(f, "parameters") && !f["parameters"].is_object() {
            return perr("tools", format!("tool {}: parameters must be a JSON Schema object", i));
        }
        names.insert(name.to_string());
    }
    Ok(names)
}

fn validate_openai_tool_choice(m: &Obj, names: &HashSet<String>) -> R<()> {
    if !present(m, "tool_choice") {
        return Ok(());
    }
    match &m["tool_choice"] {
        Value::String(s) => {
            if s == "none" {
                return Ok(());
            }
            if s != "auto" && s != "required" {
                return perr("tool_choice", format!("must be none, auto, required or a function object, got {}", fmt(&m["tool_choice"])));
            }
        }
        v @ Value::Object(_) => {
            let name = get(get(v, "function"), "name");
            if get(v, "type").as_str() != Some("function") || !matches!(name.as_str(), Some(s) if !s.is_empty()) {
                return perr("tool_choice", "object form must be {\"type\":\"function\",\"function\":{\"name\":...}}");
            }
            if !names.contains(name.as_str().unwrap()) {
                return perr("tool_choice", format!("names function {} that is not in tools", fmt(name)));
            }
        }
        _ => return perr("tool_choice", "must be a string or an object"),
    }
    if names.is_empty() {
        return perr("tool_choice", "requires tools");
    }
    Ok(())
}

fn js_str(v: &Value) -> String {
    match v {
        Value::String(s) => s.clone(),
        o => fmt(o),
    }
}

pub fn validate_anthropic_params(m: &Obj) -> R<()> {
    if !non_empty_str(m.get("model")) {
        return perr("model", "field required (non-empty string)");
    }
    if !present(m, "max_tokens") {
        return perr("max_tokens", "field required (positive integer)");
    }
    positive_int(m, "max_tokens")?;
    boolean(m, "stream")?;
    number_in(m, "temperature", 0.0, 1.0)?;
    number_in(m, "top_p", 0.0, 1.0)?;
    stop_list(m, "stop_sequences", false)?;
    if !present(m, "messages") {
        return perr("messages", "field required");
    }
    let Some(msgs) = m["messages"].as_array() else { return perr("messages", "must be an array") };
    if msgs.is_empty() || msgs.len() > lmbp::MAX_MESSAGES {
        return perr("messages", format!("1-{} messages required, got {}", lmbp::MAX_MESSAGES, msgs.len()));
    }
    for (i, msg) in msgs.iter().enumerate() {
        if !msg.is_object() {
            return perr("messages", format!("message {} must be an object", i));
        }
        let r = get(msg, "role").as_str();
        if r != Some("user") && r != Some("assistant") {
            return perr("messages", format!("message {}: role must be user or assistant", i));
        }
        let c = get(msg, "content");
        if !c.is_string() && !c.is_array() {
            return perr("messages", format!("message {}: content must be a string or an array of blocks", i));
        }
    }
    let mut names = HashSet::new();
    if present(m, "tools") {
        let Some(tools) = m["tools"].as_array() else { return perr("tools", "must be an array") };
        if tools.len() > MAX_TOOLS {
            return perr("tools", format!("at most {} tools, got {}", MAX_TOOLS, tools.len()));
        }
        for (i, t) in tools.iter().enumerate() {
            let Some(to) = t.as_object() else { return perr("tools", format!("tool {} must be an object", i)) };
            if present(to, "type") && to["type"].as_str() != Some("custom") {
                return perr("tools", format!("tool {}: server tool {} is not supported", i, js_str(&to["type"])));
            }
            let Some(name) = to.get("name").and_then(|n| n.as_str()).filter(|n| tool_name_ok(n)) else {
                return perr("tools", format!("tool {}: name must match ^[A-Za-z0-9_-]{{1,64}}$, got {}", i, undef_or(to, "name")));
            };
            if !get(t, "input_schema").is_object() {
                return perr("tools", format!("tool {}: input_schema (object) is required", i));
            }
            names.insert(name.to_string());
        }
    }
    if present(m, "tool_choice") {
        let tc = &m["tool_choice"];
        let Some(tco) = tc.as_object() else { return perr("tool_choice", "must be an object") };
        let ty = get(tc, "type");
        if ty.as_str() == Some("none") {
            return Ok(());
        }
        if ty.as_str() == Some("tool") {
            let name = get(tc, "name");
            if !matches!(name.as_str(), Some(s) if !s.is_empty()) {
                return perr("tool_choice", "type tool needs a name");
            }
            if !names.contains(name.as_str().unwrap()) {
                return perr("tool_choice", format!("names tool {} that is not in tools", fmt(name)));
            }
        } else if ty.as_str() != Some("auto") && ty.as_str() != Some("any") {
            let got = if ty.is_null() && !tco.contains_key("type") { "undefined".to_string() } else { fmt(ty) };
            return perr("tool_choice", format!("type must be auto, any, tool or none, got {}", got));
        }
        if names.is_empty() {
            return perr("tool_choice", "requires tools");
        }
    }
    Ok(())
}

fn schemas() -> &'static Schema {
    static S: OnceLock<Schema> = OnceLock::new();
    S.get_or_init(|| {
        let defs: Value = serde_json::from_str(include_str!("../../../spec/schemas/defs.json")).unwrap();
        let run: Value = serde_json::from_str(include_str!("../../../spec/schemas/task.run.payload.json")).unwrap();
        let hello: Value = serde_json::from_str(include_str!("../../../spec/schemas/hello.json")).unwrap();
        let mu: Value = serde_json::from_str(include_str!("../../../spec/schemas/models.update.json")).unwrap();
        let done: Value = serde_json::from_str(include_str!("../../../spec/schemas/task.done.json")).unwrap();
        let err: Value = serde_json::from_str(include_str!("../../../spec/schemas/task.error.json")).unwrap();
        Schema::new(vec![defs, run, hello, mu, done, err])
    })
}

/// Validates hello / models.update data against its schema (spec §7.4): None when valid, else a message.
/// Validates a device data frame's envelope and, for task.done / task.error,
/// its body against task.done.json / task.error.json: None when acceptable
/// (checked before the seq is accepted, persisted or ACKed).
pub fn device_data_problem(inp: &Value) -> Option<String> {
    let ty = inp.get("type").and_then(|v| v.as_str()).unwrap_or("").to_string();
    match inp.get("task_id").and_then(|v| v.as_str()) {
        Some(t) if !t.is_empty() => {}
        _ => return Some(format!("{} without task_id", ty)),
    }
    let seq = inp.get("seq").unwrap_or(&Value::Null);
    if !crate::json::is_int(seq) || seq.as_f64().unwrap_or(0.0) < 1.0 {
        return Some(format!("{} seq must be an integer >= 1", ty));
    }
    let data = inp.get("data").unwrap_or(&Value::Null);
    if !data.is_object() {
        return Some(format!("{} data must be a JSON object", ty));
    }
    if ty != "task.done" && ty != "task.error" {
        return None;
    }
    let e = schemas().validate(&format!("{}{}.json", BASE, ty), data)?;
    Some(if e.path.is_empty() { format!("{}: {}", ty, e.message) } else { format!("{}.{}: {}", ty, e.path.join("."), e.message) })
}

pub fn announce_problem(data: &Value, hello: bool) -> Option<String> {
    let name = if hello { "hello" } else { "models.update" };
    let e = schemas().validate(&format!("{}{}.json", BASE, name), data)?;
    Some(if e.path.is_empty() { format!("{}: {}", name, e.message) } else { format!("{}.{}: {}", name, e.path.join("."), e.message) })
}

pub fn validate_task_run(payload: &Value) -> R<()> {
    if let Some(e) = schemas().validate(&format!("{}task.run.payload.json", BASE), payload) {
        return Err(ParamError {
            param: e.path.first().cloned().unwrap_or_default(),
            message: format!("request does not satisfy the task.run schema at /{}: {}", e.path.join("/"), e.message),
        });
    }
    Ok(())
}
