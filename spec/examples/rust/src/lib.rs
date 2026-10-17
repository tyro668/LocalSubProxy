//! lmbp critical-path primitives for a Rust gateway (reference skeleton).
//! Executed against spec/testvectors/vectors.json by `cargo test`.
pub mod ext;
use base64::engine::general_purpose::URL_SAFE_NO_PAD;
use base64::Engine;
use ed25519_dalek::{Signature, Signer, SigningKey, VerifyingKey};
use serde_json::{json, Value};
use sha2::{Digest, Sha256};

pub const TYP_ACCESS_TOKEN: &str = "at+jwt";
pub const TYP_ASSERTION: &str = "JWT";
pub const TYP_PROOF: &str = "lmbp-proof+jwt";
pub const TYP_TASK: &str = "lmbp-task+jws";

pub type Result<T> = std::result::Result<T, String>;

pub fn b64u(b: &[u8]) -> String { URL_SAFE_NO_PAD.encode(b) }
/// Strict decoding: the NO_PAD engine rejects '=' and non-alphabet characters.
pub fn unb64u(s: &str) -> Result<Vec<u8>> { URL_SAFE_NO_PAD.decode(s).map_err(|e| e.to_string()) }

pub fn thumbprint(pub_key: &[u8]) -> String {
    b64u(&Sha256::digest(format!(r#"{{"crv":"Ed25519","kty":"OKP","x":"{}"}}"#, b64u(pub_key))))
}
pub fn ath(credential: &str) -> String { b64u(&Sha256::digest(credential.as_bytes())) }

pub fn sign_jws(key: &SigningKey, typ: &str, payload: &[u8], kid: Option<&str>) -> String {
    let header = match kid { Some(k) => json!({"alg":"EdDSA","kid":k,"typ":typ}), None => json!({"alg":"EdDSA","typ":typ}) };
    let input = format!("{}.{}", b64u(header.to_string().as_bytes()), b64u(payload));
    let sig = key.sign(input.as_bytes());
    format!("{}.{}", input, b64u(&sig.to_bytes()))
}

pub fn verify_jws(pub_key: &[u8], compact: &str, typ: &str) -> Result<Vec<u8>> {
    let parts: Vec<&str> = compact.split('.').collect();
    if parts.len() != 3 { return Err("malformed JWS".into()); }
    let h: Value = serde_json::from_slice(&unb64u(parts[0])?).map_err(|e| e.to_string())?;
    if h["alg"] != "EdDSA" || h["typ"] != typ || h.get("crit").is_some() { return Err("bad JWS header".into()); }
    let vk = VerifyingKey::from_bytes(pub_key.try_into().map_err(|_| "bad key length")?).map_err(|e| e.to_string())?;
    let sig = Signature::from_slice(&unb64u(parts[2])?).map_err(|e| e.to_string())?;
    // verify_strict rejects small-order keys / malleable signatures.
    vk.verify_strict(format!("{}.{}", parts[0], parts[1]).as_bytes(), &sig).map_err(|_| "bad signature".to_string())?;
    unb64u(parts[1])
}

fn claims(b: &[u8]) -> Result<Value> { serde_json::from_slice(b).map_err(|e| e.to_string()) }

pub fn verify_access_token(gw: &[u8], tok: &str, aud: &str, now: i64) -> Result<Value> {
    let c = claims(&verify_jws(gw, tok, TYP_ACCESS_TOKEN)?)?;
    let (iat, exp) = (c["iat"].as_i64().ok_or("iat")?, c["exp"].as_i64().ok_or("exp")?);
    if c["aud"] != aud || now > exp + 30 || now + 30 < iat { return Err("token aud/time invalid".into()); }
    Ok(c)
}

/// §18: iss is the gateway's https origin; aud binds the token to one tenant.
pub const EXAMPLE_ISSUER: &str = "https://gw.example.com";
pub fn token_audience(issuer: &str, tenant: &str) -> String { format!("{issuer}/tenants/{tenant}") }

pub fn issue_access_token(gw: &SigningKey, kid: &str, device_id: &str, tenant: &str, device_pub: &[u8], now: i64, ttl: i64, jti: &str) -> String {
    let c = json!({"iss":EXAMPLE_ISSUER,"sub":device_id,"aud":token_audience(EXAMPLE_ISSUER, tenant),"tenant":tenant,"iat":now,"exp":now+ttl,"jti":jti,"cnf":{"jkt":thumbprint(device_pub)}});
    sign_jws(gw, TYP_ACCESS_TOKEN, c.to_string().as_bytes(), Some(kid))
}

pub fn verify_proof(dev: &[u8], proof: &str, iss: &str, aud: &str, credential: &str, now: i64) -> Result<Value> {
    let c = claims(&verify_jws(dev, proof, TYP_PROOF)?)?;
    if c["aud"] != aud || c["iss"] != iss || c["jti"].as_str().unwrap_or("").is_empty() || c["ath"] != ath(credential).as_str() {
        return Err("proof claims invalid".into());
    }
    if (now - c["iat"].as_i64().ok_or("iat")?).abs() > 60 { return Err("proof iat outside skew".into()); }
    Ok(c) // caller MUST reject repeated jti
}

pub fn verify_hello(gw: &[u8], device_pub_of: impl Fn(&str) -> Vec<u8>, frame: &Value, token: &str, now: i64) -> Result<Value> {
    let c = verify_access_token(gw, token, &token_audience(EXAMPLE_ISSUER, "acme"), now)?;
    let sub = c["sub"].as_str().ok_or("sub")?.to_string();
    let dev = device_pub_of(&sub);
    if c["cnf"]["jkt"] != thumbprint(&dev).as_str() { return Err("token not bound to device key".into()); }
    if frame["v"] != 2 || frame["type"] != "hello" || frame["data"]["device_id"] != sub.as_str() { return Err("bad hello".into()); }
    verify_proof(&dev, frame["data"]["proof"].as_str().ok_or("proof")?, &sub, "lmbp:hello", token, now)?;
    Ok(c)
}

pub fn sign_task_run(gw: &SigningKey, kid: &str, payload: &Value) -> Value {
    json!({"v":2,"type":"task.run","task_id":payload["task_id"],"seq":1,"jws":sign_jws(gw, TYP_TASK, payload.to_string().as_bytes(), Some(kid))})
}

pub fn verify_task_run(gw: &[u8], frame: &Value) -> Result<Value> {
    let p = claims(&verify_jws(gw, frame["jws"].as_str().ok_or("jws")?, TYP_TASK)?)?;
    if p["task_id"] != frame["task_id"] { return Err("task_id mismatch".into()); }
    Ok(p)
}

pub fn sse_chunk(task_id: &str, model: &str, created: i64, text: &str) -> String {
    format!("data: {}\n\n", json!({"id":format!("chatcmpl-{task_id}"),"object":"chat.completion.chunk","created":created,"model":model,
        "choices":[{"index":0,"delta":{"content":text},"finish_reason":null}]}))
}
