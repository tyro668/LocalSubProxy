use ed25519_dalek::{Signer, SigningKey};
use lmbp_skeleton::*;
use serde_json::Value;

fn load() -> Value {
    let p = concat!(env!("CARGO_MANIFEST_DIR"), "/../../testvectors/vectors.json");
    serde_json::from_str(&std::fs::read_to_string(p).unwrap()).unwrap()
}
fn key(v: &Value, n: &str) -> SigningKey {
    SigningKey::from_bytes(&unb64u(v["keys"][n]["seed_b64url"].as_str().unwrap()).unwrap().try_into().unwrap())
}
fn pubk(v: &Value, n: &str) -> Vec<u8> { unb64u(v["keys"][n]["public_b64url"].as_str().unwrap()).unwrap() }
fn s<'a>(v: &'a Value, k: &str) -> &'a str { v[k].as_str().unwrap_or("") }

fn check(v: &Value, kind: &str, compact: &str, k: &[u8], e: &Value) -> Result<()> {
    let now = v["now"].as_i64().unwrap();
    match kind {
        "access_token" => { let c = verify_access_token(k, compact, s(e, "aud"), now)?; if c["cnf"]["jkt"] != e["cnf.jkt"] { return Err("jkt".into()) } }
        "client_assertion" => { verify_client_assertion(k, compact, s(e, "aud"), now)?; }
        "proof" => { verify_proof(k, compact, s(e, "iss"), s(e, "aud"), s(e, "credential"), now)?; }
        "task_run" => { verify_task_run(k, &serde_json::json!({"task_id": e["envelope_task_id"], "jws": compact}))?; }
        _ => return Err("kind".into()),
    }
    Ok(())
}

#[test]
fn conformance_vectors() {
    let v = load();
    for (n, k) in v["keys"].as_object().unwrap() {
        let p = key(&v, n).verifying_key().to_bytes();
        assert_eq!(b64u(&p), s(k, "public_b64url"));
        assert_eq!(thumbprint(&p), s(k, "jkt"));
    }
    for c in v["base64url"].as_array().unwrap() {
        let bytes: Vec<u8> = (0..s(c, "bytes_hex").len()).step_by(2).map(|i| u8::from_str_radix(&s(c, "bytes_hex")[i..i + 2], 16).unwrap()).collect();
        assert_eq!(b64u(&bytes), s(c, "b64url"));
    }
    for j in v["jws"].as_array().unwrap() {
        let by = s(j, "signed_by");
        assert_eq!(b64u(&key(&v, by).sign(s(j, "signing_input").as_bytes()).to_bytes()), s(j, "signature_b64url"), "{}", s(j, "name"));
        check(&v, s(j, "kind"), s(j, "compact"), &pubk(&v, by), &j["verify_with"]).unwrap_or_else(|e| panic!("{}: {e}", s(j, "name")));
    }
    for n in v["negative"].as_array().unwrap() {
        assert!(check(&v, s(n, "kind"), s(n, "compact"), &pubk(&v, s(n, "verify_with_key")), &n["verify_with"]).is_err(), "accepted {}", s(n, "name"));
    }
    let at = v["jws"].as_array().unwrap().iter().find(|j| j["name"] == "access_token").unwrap();
    let p = &at["payload"];
    let mine = issue_access_token(&key(&v, "gateway"), s(&at["protected_header"], "kid"), s(p, "sub"), s(p, "tenant"), &pubk(&v, "device"),
        p["iat"].as_i64().unwrap(), p["exp"].as_i64().unwrap() - p["iat"].as_i64().unwrap(), s(p, "jti"));
    assert_eq!(mine, s(at, "compact"));
    let hello = &v["valid_frames"].as_array().unwrap().iter().find(|f| f["name"] == "hello").unwrap()["frame"];
    let dev = pubk(&v, "device");
    verify_hello(&pubk(&v, "gateway"), |_| dev.clone(), hello, s(at, "compact"), v["now"].as_i64().unwrap()).unwrap();
    let tr = v["jws"].as_array().unwrap().iter().find(|j| j["name"] == "task_run").unwrap();
    let env = sign_task_run(&key(&v, "gateway"), &thumbprint(&pubk(&v, "gateway")), &tr["payload"]);
    assert_eq!(verify_task_run(&pubk(&v, "gateway"), &env).unwrap(), tr["payload"]);
    assert!(sse_chunk("t", "m", 1, "hi").starts_with("data: {\"id\":\"chatcmpl-t\""));
}

/// spec/testvectors/jose.json: every valid and negative case through the strict verifiers.
#[test]
fn jose_vectors() {
    let j: Value = serde_json::from_str(&std::fs::read_to_string(concat!(env!("CARGO_MANIFEST_DIR"), "/../../testvectors/jose.json")).unwrap()).unwrap();
    let now = j["now"].as_i64().unwrap();
    let run = |v: &Value| -> Result<Value> {
        let k = unb64u(s(&j["keys"][s(v, "verify_with_key")], "public_b64url")).unwrap();
        let (c, t) = (&v["context"], s(v, "compact"));
        match s(v, "kind") {
            "proof" => verify_proof(&k, t, s(c, "iss"), s(c, "aud"), s(c, "credential"), now),
            "client_assertion" => verify_client_assertion(&k, t, s(c, "aud"), now),
            "access_token" => verify_access_token(&k, t, s(c, "aud"), now),
            "e2ee_key" => ext::verify_key_statement(&k, "", s(c, "device_id"), t, now).map(|_| Value::Null),
            "task_run" => verify_task_run_at(&k, &serde_json::json!({"task_id": c["envelope_task_id"], "jws": t}), Some(now)),
            "metadata" => verify_metadata(&k, t, s(c, "issuer"), s(c, "tenant"), now),
            other => panic!("unknown kind {other}"),
        }
    };
    let (valid, negative) = (j["valid"].as_array().unwrap(), j["negative"].as_array().unwrap());
    assert!(!valid.is_empty() && negative.len() > 200);
    for v in valid { run(v).unwrap_or_else(|e| panic!("jose valid {}: {e}", s(v, "name"))); }
    let accepted: Vec<&str> = negative.iter().filter(|v| run(v).is_ok()).map(|v| s(v, "name")).collect();
    assert!(accepted.is_empty(), "jose negatives accepted: {accepted:?}");
    println!("jose: {} valid, {} negative cases", valid.len(), negative.len());
}
