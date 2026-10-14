//! JSON helpers with JavaScript semantics (truthiness, Number.isInteger, String(x))
//! over serde_json values (key order preserved).
use serde_json::{Map, Value};

pub type Obj = Map<String, Value>;

pub static NULL: Value = Value::Null;

/// `o[k]`, or null when o is not an object or k is absent.
pub fn get<'a>(o: &'a Value, k: &str) -> &'a Value {
    match o {
        Value::Object(m) => m.get(k).unwrap_or(&NULL),
        _ => &NULL,
    }
}

pub fn has(o: &Value, k: &str) -> bool {
    matches!(o, Value::Object(m) if m.contains_key(k))
}

pub fn truthy(v: &Value) -> bool {
    match v {
        Value::Null => false,
        Value::Bool(b) => *b,
        Value::String(s) => !s.is_empty(),
        Value::Number(n) => n.as_f64().map(|f| f != 0.0 && !f.is_nan()).unwrap_or(true),
        _ => true,
    }
}

/// Number.isInteger
pub fn is_int(v: &Value) -> bool {
    match v {
        Value::Number(n) => n.is_i64() || n.is_u64() || n.as_f64().map(|f| f.is_finite() && f.fract() == 0.0).unwrap_or(false),
        _ => false,
    }
}

/// Number.isSafeInteger
pub fn is_safe_int(v: &Value) -> bool {
    is_int(v) && v.as_f64().map(|f| f.abs() <= 9007199254740991.0).unwrap_or(false)
}

/// Integer value of a JSON number (truncated); 0 otherwise.
pub fn int(v: &Value) -> i64 {
    match v {
        Value::Number(n) => n.as_i64().unwrap_or_else(|| n.as_f64().map(|f| f as i64).unwrap_or(0)),
        _ => 0,
    }
}

/// JavaScript String(x) for JSON values (undefined/null → "null").
pub fn js_string(v: &Value) -> String {
    match v {
        Value::String(s) => s.clone(),
        other => stringify(other),
    }
}

/// JSON.stringify
pub fn stringify(v: &Value) -> String {
    let mut b = String::new();
    write(&mut b, v);
    b
}

pub fn js_num(f: f64) -> String {
    if !f.is_finite() {
        return "null".into();
    }
    if f == f.trunc() && f.abs() < 1e21 {
        return format!("{:.0}", f).replace("-0", if f == 0.0 { "0" } else { "-0" });
    }
    let a = f.abs();
    if a >= 1e21 || a < 1e-6 {
        let s = format!("{:e}", f);
        let (m, e) = s.split_once('e').unwrap();
        let e: i32 = e.parse().unwrap();
        return format!("{}e{}{}", m, if e > 0 { "+" } else { "" }, e);
    }
    format!("{}", f)
}

fn write(b: &mut String, v: &Value) {
    match v {
        Value::Null => b.push_str("null"),
        Value::Bool(x) => b.push_str(if *x { "true" } else { "false" }),
        Value::Number(n) => {
            if n.is_i64() || n.is_u64() {
                b.push_str(&n.to_string())
            } else {
                b.push_str(&js_num(n.as_f64().unwrap_or(f64::NAN)))
            }
        }
        Value::String(s) => b.push_str(&serde_json::to_string(s).unwrap()),
        Value::Array(a) => {
            b.push('[');
            for (i, x) in a.iter().enumerate() {
                if i > 0 {
                    b.push(',');
                }
                write(b, x);
            }
            b.push(']');
        }
        Value::Object(m) => {
            b.push('{');
            for (i, (k, x)) in m.iter().enumerate() {
                if i > 0 {
                    b.push(',');
                }
                b.push_str(&serde_json::to_string(k).unwrap());
                b.push(':');
                write(b, x);
            }
            b.push('}');
        }
    }
}

pub fn parse(s: &str) -> Option<Value> {
    serde_json::from_str(s).ok()
}

/// Build an object from key/value pairs.
#[macro_export]
macro_rules! obj {
    () => { serde_json::Value::Object(serde_json::Map::new()) };
    ($($k:expr => $v:expr),+ $(,)?) => {{
        let mut m = serde_json::Map::new();
        $( m.insert(($k).to_string(), serde_json::Value::from($v)); )+
        serde_json::Value::Object(m)
    }};
}
