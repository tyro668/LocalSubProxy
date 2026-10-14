//! Minimal JSON Schema (draft 2020-12) validator implementing exactly the keywords
//! the LMBP schemas use; fails closed on any other keyword. Port of jsonschema.js.
use crate::json::{self, get};
use serde_json::Value;
use std::collections::HashMap;

const ANNOTATIONS: [&str; 7] = ["$schema", "$id", "$defs", "title", "description", "default", "$comment"];
const KEYWORDS: [&str; 22] = ["$ref", "type", "required", "properties", "additionalProperties", "items", "minItems", "maxItems", "minimum", "maximum",
    "minLength", "maxLength", "enum", "const", "pattern", "oneOf", "anyOf", "allOf", "if", "then", "else", "uniqueItems"];

pub struct Err {
    pub path: Vec<String>,
    pub message: String,
}

pub struct Schema {
    docs: HashMap<String, Value>,
    patterns: std::sync::Mutex<HashMap<String, regex::Regex>>,
}

fn check(s: &Value) {
    let Some(m) = s.as_object() else { return };
    for (k, v) in m {
        if ANNOTATIONS.contains(&k.as_str()) {
            if k == "$defs" {
                for x in v.as_object().unwrap().values() {
                    check(x);
                }
            }
            continue;
        }
        if !KEYWORDS.contains(&k.as_str()) {
            panic!("jsonschema: unsupported keyword {}", k);
        }
        match k.as_str() {
            "properties" => v.as_object().unwrap().values().for_each(check),
            "items" | "additionalProperties" | "if" | "then" | "else" => check(v),
            "oneOf" | "anyOf" | "allOf" => v.as_array().unwrap().iter().for_each(check),
            _ => {}
        }
    }
}

fn type_of(v: &Value) -> &'static str {
    match v {
        Value::Null => "null",
        Value::Array(_) => "array",
        Value::Object(_) => "object",
        Value::String(_) => "string",
        Value::Bool(_) => "boolean",
        Value::Number(_) => if json::is_int(v) { "integer" } else { "number" },
    }
}

fn is_type(v: &Value, t: &str) -> bool {
    let a = type_of(v);
    a == t || (t == "number" && a == "integer")
}

pub fn deep_equal(a: &Value, b: &Value) -> bool {
    match (a, b) {
        (Value::Number(x), Value::Number(y)) => x.as_f64() == y.as_f64(),
        (Value::Array(x), Value::Array(y)) => x.len() == y.len() && x.iter().zip(y).all(|(p, q)| deep_equal(p, q)),
        (Value::Object(x), Value::Object(y)) => x.len() == y.len() && x.iter().all(|(k, v)| y.get(k).map(|w| deep_equal(v, w)).unwrap_or(false)),
        _ => a == b,
    }
}

fn resolve_url(base: &str, file: &str) -> String {
    if file.contains("://") {
        return file.to_string();
    }
    match base.rfind('/') {
        Some(i) => format!("{}{}", &base[..=i], file.trim_start_matches("./")),
        None => file.to_string(),
    }
}

fn pct_decode(s: &str) -> String {
    let b = s.as_bytes();
    let mut out = vec![];
    let mut i = 0;
    while i < b.len() {
        if b[i] == b'%' && i + 2 < b.len() + 0 && i + 2 <= b.len() - 1 {
            if let Ok(x) = u8::from_str_radix(&s[i + 1..i + 3], 16) {
                out.push(x);
                i += 3;
                continue;
            }
        }
        out.push(b[i]);
        i += 1;
    }
    String::from_utf8_lossy(&out).into_owned().replace("~1", "/").replace("~0", "~")
}

impl Schema {
    pub fn new(list: Vec<Value>) -> Schema {
        let mut docs = HashMap::new();
        for d in list {
            check(&d);
            docs.insert(d["$id"].as_str().unwrap().to_string(), d);
        }
        Schema { docs, patterns: Default::default() }
    }

    fn resolve(&self, r: &str, doc_id: &str) -> (&Value, String) {
        let (file, frag) = r.split_once('#').unwrap_or((r, ""));
        let id = if file.is_empty() { doc_id.to_string() } else { resolve_url(doc_id, file) };
        let mut s = self.docs.get(&id).unwrap_or_else(|| panic!("jsonschema: unknown $ref {}", r));
        for p in frag.split('/') {
            if p.is_empty() {
                continue;
            }
            s = get(s, &pct_decode(p));
        }
        if s.is_null() {
            panic!("jsonschema: unresolvable $ref {}", r);
        }
        (s, id)
    }

    pub fn validate(&self, id: &str, value: &Value) -> Option<Err> {
        let d = self.docs.get(id).unwrap_or_else(|| panic!("jsonschema: unknown schema {}", id));
        self.v(d, value, &[], id)
    }

    fn re_match(&self, p: &str, s: &str) -> bool {
        let mut m = self.patterns.lock().unwrap();
        let re = m.entry(p.to_string()).or_insert_with(|| regex::Regex::new(p).expect("schema pattern"));
        re.is_match(s)
    }

    fn v(&self, s: &Value, x: &Value, path: &[String], doc_id: &str) -> Option<Err> {
        let Some(sm) = s.as_object() else { return None };
        let e = |m: String| Some(Err { path: path.to_vec(), message: m });
        let plus = |k: String| {
            let mut p = path.to_vec();
            p.push(k);
            p
        };
        if let Some(Value::String(r)) = sm.get("$ref") {
            let (rs, id) = self.resolve(r, doc_id);
            if let Some(er) = self.v(rs, x, path, &id) {
                return Some(er);
            }
        }
        if let Some(t) = sm.get("type") {
            let ts: Vec<&str> = match t {
                Value::Array(a) => a.iter().filter_map(|v| v.as_str()).collect(),
                v => vec![v.as_str().unwrap_or("")],
            };
            if !ts.iter().any(|tt| is_type(x, tt)) {
                return e(format!("must be {}", ts.join(" or ")));
            }
        }
        if let Some(c) = sm.get("const") {
            if !deep_equal(x, c) {
                return e(format!("must be {}", json::stringify(c)));
            }
        }
        if let Some(Value::Array(en)) = sm.get("enum") {
            if !en.iter().any(|v| deep_equal(v, x)) {
                return e(format!("must be one of {}", json::stringify(&Value::Array(en.clone()))));
            }
        }
        if let Value::Number(n) = x {
            let n = n.as_f64().unwrap();
            if let Some(m @ Value::Number(_)) = sm.get("minimum") {
                if n < m.as_f64().unwrap() {
                    return e(format!("must be >= {}", json::stringify(m)));
                }
            }
            if let Some(m @ Value::Number(_)) = sm.get("maximum") {
                if n > m.as_f64().unwrap() {
                    return e(format!("must be <= {}", json::stringify(m)));
                }
            }
        }
        if let Value::String(st) = x {
            let n = st.chars().count() as i64;
            if let Some(m @ Value::Number(_)) = sm.get("minLength") {
                if n < json::int(m) {
                    return e(format!("must have at least {} characters", json::stringify(m)));
                }
            }
            if let Some(m @ Value::Number(_)) = sm.get("maxLength") {
                if n > json::int(m) {
                    return e(format!("must have at most {} characters", json::stringify(m)));
                }
            }
            if let Some(Value::String(p)) = sm.get("pattern") {
                if !self.re_match(p, st) {
                    return e(format!("must match {}", p));
                }
            }
        }
        if let Value::Array(l) = x {
            if let Some(m @ Value::Number(_)) = sm.get("minItems") {
                if (l.len() as i64) < json::int(m) {
                    return e(format!("must have at least {} items", json::stringify(m)));
                }
            }
            if let Some(m @ Value::Number(_)) = sm.get("maxItems") {
                if (l.len() as i64) > json::int(m) {
                    return e(format!("must have at most {} items", json::stringify(m)));
                }
            }
            if sm.get("uniqueItems") == Some(&Value::Bool(true)) {
                for i in 0..l.len() {
                    for j in 0..i {
                        if deep_equal(&l[i], &l[j]) {
                            return e("items must be unique".into());
                        }
                    }
                }
            }
            if let Some(it) = sm.get("items") {
                if !it.is_null() {
                    for (i, v) in l.iter().enumerate() {
                        if let Some(er) = self.v(it, v, &plus(i.to_string()), doc_id) {
                            return Some(er);
                        }
                    }
                }
            }
        }
        if let Value::Object(mo) = x {
            if let Some(Value::Array(req)) = sm.get("required") {
                for k in req {
                    if !mo.contains_key(k.as_str().unwrap_or("")) {
                        return e(format!("missing required property {}", json::js_string(k)));
                    }
                }
            }
            let props = sm.get("properties").and_then(|p| p.as_object());
            let ap = sm.get("additionalProperties");
            for (k, v) in mo {
                if let Some(ps) = props.and_then(|p| p.get(k)) {
                    if let Some(er) = self.v(ps, v, &plus(k.clone()), doc_id) {
                        return Some(er);
                    }
                } else if ap == Some(&Value::Bool(false)) {
                    return Some(Err { path: plus(k.clone()), message: "is not allowed".into() });
                } else if let Some(a @ Value::Object(_)) = ap {
                    if let Some(er) = self.v(a, v, &plus(k.clone()), doc_id) {
                        return Some(er);
                    }
                }
            }
        }
        if let Some(Value::Array(all)) = sm.get("allOf") {
            for sub in all {
                if let Some(er) = self.v(sub, x, path, doc_id) {
                    return Some(er);
                }
            }
        }
        if let Some(Value::Array(any)) = sm.get("anyOf") {
            if !any.iter().any(|sub| self.v(sub, x, path, doc_id).is_none()) {
                return e("does not match any allowed form".into());
            }
        }
        if let Some(Value::Array(one)) = sm.get("oneOf") {
            if one.iter().filter(|sub| self.v(sub, x, path, doc_id).is_none()).count() != 1 {
                return e("must match exactly one allowed form".into());
            }
        }
        if let Some(cond) = sm.get("if") {
            let branch = if self.v(cond, x, path, doc_id).is_some() { sm.get("else") } else { sm.get("then") };
            if let Some(b) = branch {
                if let Some(er) = self.v(b, x, path, doc_id) {
                    return Some(er);
                }
            }
        }
        None
    }
}
