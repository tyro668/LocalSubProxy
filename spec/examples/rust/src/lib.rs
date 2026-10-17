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
pub const TYP_METADATA: &str = "lmbp-metadata+jwt";
const LEEWAY: i64 = 30;
const METADATA_MAX_AGE: i64 = 86400;

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

/// §5.2: the protected header is exactly {alg: "EdDSA", typ, kid?}; kid, when
/// present, is the RFC 7638 thumbprint of the verifying key. The payload MUST be
/// a JSON object, which is returned.
pub fn verify_jws(pub_key: &[u8], compact: &str, typ: &str) -> Result<Value> {
    let parts: Vec<&str> = compact.split('.').collect();
    if parts.len() != 3 { return Err("malformed JWS".into()); }
    let h: Value = serde_json::from_slice(&unb64u(parts[0])?).map_err(|e| e.to_string())?;
    let h = h.as_object().ok_or("bad JWS header")?;
    if let Some(k) = h.keys().find(|k| !["alg", "typ", "kid"].contains(&k.as_str())) { return Err(format!("forbidden JWS header {k}")); }
    if h.get("alg").and_then(Value::as_str) != Some("EdDSA") { return Err("JWS alg must be EdDSA".into()); }
    if h.get("typ").and_then(Value::as_str) != Some(typ) { return Err(format!("JWS typ must be {typ}")); }
    if let Some(kid) = h.get("kid") {
        if kid.as_str() != Some(thumbprint(pub_key).as_str()) { return Err("JWS kid does not match the verifying key".into()); }
    }
    let vk = VerifyingKey::from_bytes(pub_key.try_into().map_err(|_| "bad key length")?).map_err(|e| e.to_string())?;
    let sig = Signature::from_slice(&unb64u(parts[2])?).map_err(|e| e.to_string())?;
    // verify_strict rejects small-order keys / malleable signatures.
    vk.verify_strict(format!("{}.{}", parts[0], parts[1]).as_bytes(), &sig).map_err(|_| "bad signature".to_string())?;
    let p: Value = serde_json::from_slice(&unb64u(parts[1])?).map_err(|e| e.to_string())?;
    if !p.is_object() { return Err("JWS payload must be a JSON object".into()); }
    Ok(p)
}

/// A NumericDate (or blob size): a JSON integer 0..2^53-1, no fraction or exponent
/// (serde_json keeps 1.0 and 1e3 as f64, so as_u64 rejects them).
fn uint(v: &Value, k: &str) -> Result<i64> {
    match v.get(k).and_then(Value::as_u64) { Some(n) if n < 1 << 53 => Ok(n as i64), _ => Err(format!("claim {k} must be a non-negative integer")) }
}
/// Claim types (§5.2): ints are NumericDates, strs non-empty strings, opt strings.
pub fn require_claims(c: &Value, ints: &[&str], strs: &[&str], opt: &[&str]) -> Result<()> {
    for k in ints { uint(c, k)?; }
    for k in strs { if c.get(*k).and_then(Value::as_str).unwrap_or("").is_empty() { return Err(format!("claim {k} must be a non-empty string")); } }
    for k in opt { if c.get(*k).and_then(Value::as_str).is_none() { return Err(format!("claim {k} must be a string")); } }
    Ok(())
}
/// iat and exp NumericDates with exp >= iat.
pub fn time_range(c: &Value) -> Result<(i64, i64)> {
    let (iat, exp) = (uint(c, "iat")?, uint(c, "exp")?);
    if exp < iat { return Err("exp before iat".into()); }
    Ok((iat, exp))
}

/// Access tokens (at+jwt) and client assertions (JWT): iss/sub/aud/jti, iat <= exp, aud, 30 s leeway.
pub fn verify_jwt(pub_key: &[u8], tok: &str, typ: &str, aud: &str, now: i64) -> Result<Value> {
    let c = verify_jws(pub_key, tok, typ)?;
    require_claims(&c, &[], &["iss", "sub", "aud", "jti"], &[])?;
    let (iat, exp) = time_range(&c)?;
    if c["aud"] != aud { return Err("wrong audience".into()); }
    if now > exp + LEEWAY || now + LEEWAY < iat { return Err("token expired".into()); }
    Ok(c)
}
pub fn verify_access_token(gw: &[u8], tok: &str, aud: &str, now: i64) -> Result<Value> { verify_jwt(gw, tok, TYP_ACCESS_TOKEN, aud, now) }
/// The caller MUST also reject a repeated jti.
pub fn verify_client_assertion(dev: &[u8], tok: &str, aud: &str, now: i64) -> Result<Value> { verify_jwt(dev, tok, TYP_ASSERTION, aud, now) }

/// §18: iss is the gateway's https origin; aud binds the token to one tenant.
pub const EXAMPLE_ISSUER: &str = "https://gw.example.com";
pub fn token_audience(issuer: &str, tenant: &str) -> String { format!("{issuer}/tenants/{tenant}") }

pub fn issue_access_token(gw: &SigningKey, kid: &str, device_id: &str, tenant: &str, device_pub: &[u8], now: i64, ttl: i64, jti: &str) -> String {
    let c = json!({"iss":EXAMPLE_ISSUER,"sub":device_id,"aud":token_audience(EXAMPLE_ISSUER, tenant),"tenant":tenant,"iat":now,"exp":now+ttl,"jti":jti,"cnf":{"jkt":thumbprint(device_pub)}});
    sign_jws(gw, TYP_ACCESS_TOKEN, c.to_string().as_bytes(), Some(kid))
}

pub fn verify_proof(dev: &[u8], proof: &str, iss: &str, aud: &str, credential: &str, now: i64) -> Result<Value> {
    let c = verify_jws(dev, proof, TYP_PROOF)?;
    require_claims(&c, &["iat"], &["aud", "jti", "ath"], &["iss"])?;
    if c["aud"] != aud || c["iss"] != iss || c["ath"] != ath(credential).as_str() { return Err("proof claims invalid".into()); }
    if (now - uint(&c, "iat")?).abs() > 60 { return Err("proof iat outside skew".into()); }
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

/// task.run (§5.6): task_id/device_id/model/nonce, iat <= exp, strict blob
/// references, bound to the envelope task_id; expired after exp when now is given.
pub fn verify_task_run_at(gw: &[u8], frame: &Value, now: Option<i64>) -> Result<Value> {
    let p = verify_jws(gw, frame["jws"].as_str().ok_or("jws")?, TYP_TASK)?;
    require_claims(&p, &[], &["task_id", "device_id", "model", "nonce"], &[])?;
    let (_, exp) = time_range(&p)?;
    if let Some(blobs) = p.get("blobs") {
        for b in blobs.as_array().ok_or("blobs must be an array")? {
            if !b.is_object() { return Err("blob ref must be an object".into()); }
            require_claims(b, &["size"], &["id", "media_type", "sha256"], &[])?;
            if unb64u(b["sha256"].as_str().unwrap_or("")).map(|d| d.len()) != Ok(32) { return Err("blob sha256 must be 32 bytes base64url".into()); }
        }
    }
    if p["task_id"] != frame["task_id"] { return Err("task_id mismatch".into()); }
    if matches!(now, Some(n) if n > exp) { return Err("task.run expired".into()); }
    Ok(p)
}
pub fn verify_task_run(gw: &[u8], frame: &Value) -> Result<Value> { verify_task_run_at(gw, frame, None) }

/// scheme://host[:port] of an http(s) URL, lowercased, default port dropped; "" otherwise.
fn origin(u: &str) -> String {
    let Some((scheme, rest)) = u.split_once("://") else { return String::new() };
    let scheme = scheme.to_ascii_lowercase();
    let default = match scheme.as_str() { "http" => "80", "https" => "443", _ => return String::new() };
    let auth = rest.split(['/', '?', '#']).next().unwrap_or("");
    let host = auth.rsplit('@').next().unwrap_or("").to_ascii_lowercase();
    let host = host.strip_suffix(&format!(":{default}")).unwrap_or(&host);
    if host.is_empty() { String::new() } else { format!("{scheme}://{host}") }
}

/// Signed tenant metadata (§19.1): bound to issuer, tenant and token audience,
/// speaks 2.0, names the SaaS, valid at most 24 h, endpoints on the issuer origin.
pub fn verify_metadata(gw: &[u8], jws: &str, issuer: &str, tenant: &str, now: i64) -> Result<Value> {
    let m = verify_jws(gw, jws, TYP_METADATA)?;
    require_claims(&m, &[], &["issuer", "tenant", "audience"], &[])?;
    let (iat, exp) = time_range(&m)?;
    if !m["protocol_versions"].as_array().is_some_and(|v| v.iter().any(|x| x == "2.0")) { return Err("gateway does not speak LMBP 2.0".into()); }
    if m["issuer"] != issuer || m["tenant"] != tenant || m["audience"] != token_audience(issuer, tenant).as_str() {
        return Err("metadata issuer/tenant/audience mismatch".into());
    }
    if m["saas"]["name"].as_str().unwrap_or("").trim().is_empty() { return Err("metadata lacks saas.name".into()); }
    if now > exp + LEEWAY || now + LEEWAY < iat || exp - iat > METADATA_MAX_AGE { return Err("metadata expired or validity too long".into()); }
    for k in ["jwks_uri", "device_authorization_endpoint", "token_endpoint"] {
        if origin(m[k].as_str().unwrap_or("")) != origin(issuer) { return Err(format!("{k} is not on the issuer origin")); }
    }
    Ok(m)
}

pub fn sse_chunk(task_id: &str, model: &str, created: i64, text: &str) -> String {
    format!("data: {}\n\n", json!({"id":format!("chatcmpl-{task_id}"),"object":"chat.completion.chunk","created":created,"model":model,
        "choices":[{"index":0,"delta":{"content":text},"finish_reason":null}]}))
}
