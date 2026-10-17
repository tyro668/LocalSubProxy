//! lmbp extensions: E2EE (JWE ECDH-ES/X25519/A256GCM, sealed result frames,
//! key statements) and flow-control / resume bookkeeping.
//! Crates: x25519-dalek (static_secrets), aes-gcm, sha2 — all RustCrypto/dalek.
//! (josekit can also open the JWE; pin alg/enc and reject `zip` yourself.)
use crate::{b64u, require_claims, thumbprint, time_range, unb64u, verify_jws, Result};
use aes_gcm::aead::{Aead, KeyInit, Payload};
use aes_gcm::{Aes256Gcm, Nonce};
use serde_json::{json, Value};
use sha2::{Digest, Sha256};
use std::collections::HashMap;
use x25519_dalek::{PublicKey, StaticSecret};

pub const TYP_E2EE_KEY: &str = "lmbp-e2ee-key+jwt";
pub const CTY_REQUEST: &str = "lmbp-request+json";

pub fn x25519_thumbprint(pub_key: &[u8]) -> String {
    b64u(&Sha256::digest(format!(r#"{{"crv":"X25519","kty":"OKP","x":"{}"}}"#, b64u(pub_key))))
}

pub fn x25519_public(private: &[u8; 32]) -> [u8; 32] { PublicKey::from(&StaticSecret::from(*private)).to_bytes() }

/// Verifies a device key statement against the identity-key thumbprint pinned at pairing.
pub fn verify_key_statement(device_pub: &[u8], pinned_jkt: &str, device_id: &str, stmt: &str, now: i64) -> Result<(Vec<u8>, String)> {
    if !pinned_jkt.is_empty() && thumbprint(device_pub) != pinned_jkt { return Err("device key does not match pinned thumbprint".into()); }
    let c = verify_jws(device_pub, stmt, TYP_E2EE_KEY)?;
    require_claims(&c, &[], &["iss", "sub"], &[])?;
    let (iat, exp) = time_range(&c)?;
    let j = &c["jwk"];
    if c["iss"] != device_id || c["sub"] != device_id || j["kty"] != "OKP" || j["crv"] != "X25519" { return Err("bad key statement".into()); }
    if now > exp || now + 60 < iat { return Err("key statement expired".into()); }
    let x = unb64u(j["x"].as_str().ok_or("bad e2ee key")?)?;
    let kid = j["kid"].as_str().unwrap_or("").to_string();
    if x.len() != 32 || kid != x25519_thumbprint(&x) { return Err("bad e2ee key".into()); }
    Ok((x, kid))
}

/// RFC 7518 §4.6.2 Concat KDF (SHA-256), empty apu/apv, 256-bit output.
pub fn concat_kdf(z: &[u8], enc: &str) -> [u8; 32] {
    let mut h = Sha256::new();
    h.update(1u32.to_be_bytes());
    h.update(z);
    h.update((enc.len() as u32).to_be_bytes());
    h.update(enc.as_bytes());
    h.update(0u32.to_be_bytes());
    h.update(0u32.to_be_bytes());
    h.update(256u32.to_be_bytes());
    h.finalize().into()
}

pub fn seal_jwe(recipient: &[u8; 32], kid: &str, plaintext: &[u8], eph_priv: &[u8; 32], iv: &[u8; 12]) -> String {
    let eph = StaticSecret::from(*eph_priv);
    let z = eph.diffie_hellman(&PublicKey::from(*recipient));
    let hdr = json!({"alg":"ECDH-ES","enc":"A256GCM","epk":{"kty":"OKP","crv":"X25519","x":b64u(PublicKey::from(&eph).as_bytes())},"kid":kid,"cty":CTY_REQUEST});
    let prot = b64u(hdr.to_string().as_bytes());
    let ct = Aes256Gcm::new_from_slice(&concat_kdf(z.as_bytes(), "A256GCM")).unwrap()
        .encrypt(Nonce::from_slice(iv), Payload { msg: plaintext, aad: prot.as_bytes() }).unwrap();
    let n = ct.len() - 16;
    format!("{}..{}.{}.{}", prot, b64u(iv), b64u(&ct[..n]), b64u(&ct[n..]))
}

pub fn open_jwe(private: &[u8; 32], jwe: &str) -> Result<(Value, Vec<u8>)> {
    let p: Vec<&str> = jwe.split('.').collect();
    if p.len() != 5 || !p[1].is_empty() { return Err("malformed JWE".into()); }
    let h: Value = serde_json::from_slice(&unb64u(p[0])?).map_err(|e| e.to_string())?;
    if h["alg"] != "ECDH-ES" || h["enc"] != "A256GCM" || h.get("zip").is_some() || h.get("crit").is_some() { return Err("unsupported JWE algorithm".into()); }
    if h["epk"]["kty"] != "OKP" || h["epk"]["crv"] != "X25519" { return Err("bad epk".into()); }
    let epk: [u8; 32] = unb64u(h["epk"]["x"].as_str().ok_or("epk")?)?.try_into().map_err(|_| "epk length")?;
    let (iv, mut ct, tag) = (unb64u(p[2])?, unb64u(p[3])?, unb64u(p[4])?);
    if iv.len() != 12 || tag.len() != 16 { return Err("bad JWE encoding".into()); }
    ct.extend_from_slice(&tag);
    let z = StaticSecret::from(*private).diffie_hellman(&PublicKey::from(epk));
    let pt = Aes256Gcm::new_from_slice(&concat_kdf(z.as_bytes(), "A256GCM")).unwrap()
        .decrypt(Nonce::from_slice(&iv), Payload { msg: &ct, aad: p[0].as_bytes() }).map_err(|_| "JWE decryption failed")?;
    Ok((h, pt))
}

pub fn chunk_nonce(seq: u64) -> [u8; 12] { let mut n = [0u8; 12]; n[4..].copy_from_slice(&seq.to_be_bytes()); n }
pub fn chunk_aad(rid: &str, typ: &str, seq: u64) -> String { format!("lmbp-e2ee/v1|{rid}|{typ}|{seq}") }

pub fn seal_chunk(key: &[u8], rid: &str, typ: &str, seq: u64, plaintext: &[u8]) -> String {
    let aad = chunk_aad(rid, typ, seq);
    b64u(&Aes256Gcm::new_from_slice(key).unwrap().encrypt(Nonce::from_slice(&chunk_nonce(seq)), Payload { msg: plaintext, aad: aad.as_bytes() }).unwrap())
}

pub fn open_chunk(key: &[u8], rid: &str, typ: &str, seq: u64, sealed: &str) -> Result<Vec<u8>> {
    let aad = chunk_aad(rid, typ, seq);
    Aes256Gcm::new_from_slice(key).map_err(|_| "bad key")?
        .decrypt(Nonce::from_slice(&chunk_nonce(seq)), Payload { msg: &unb64u(sealed)?, aad: aad.as_bytes() })
        .map_err(|_| "chunk authentication failed".into())
}

// ---- flow control (§15) ----
pub fn can_send(tw: usize, cw: usize, task_used: usize, conn_used: usize, n: usize) -> bool {
    (task_used + n <= tw || task_used == 0) && (conn_used + n <= cw || conn_used == 0)
}

#[derive(Debug, PartialEq, Eq)]
pub enum Verdict { Accepted, Duplicate, Gap, Overflow }

#[derive(Default)]
struct InTask { recv: u64, sizes: Vec<(u64, usize)>, used: usize }

pub struct Inbound { tw: usize, cw: usize, conn_used: usize, tasks: HashMap<String, InTask> }

impl Inbound {
    pub fn new(tw: usize, cw: usize) -> Self { Inbound { tw, cw, conn_used: 0, tasks: HashMap::new() } }
    pub fn accept(&mut self, task: &str, seq: u64, size: usize) -> Verdict {
        let t = self.tasks.entry(task.to_string()).or_default();
        if seq <= t.recv { return Verdict::Duplicate; }
        if seq != t.recv + 1 { return Verdict::Gap; }
        if (t.used + size > self.tw && t.used > 0) || (self.conn_used + size > self.cw && self.conn_used > 0) { return Verdict::Overflow; }
        t.recv = seq; t.sizes.push((seq, size)); t.used += size; self.conn_used += size;
        Verdict::Accepted
    }
    pub fn acked(&mut self, task: &str, seq: u64) {
        if let Some(t) = self.tasks.get_mut(task) {
            while let Some(&(s, n)) = t.sizes.first() {
                if s > seq { break; }
                t.sizes.remove(0); t.used -= n; self.conn_used -= n;
            }
        }
    }
}

pub fn retransmit(unacked: &[u64], peer_recv: u64) -> Vec<u64> { unacked.iter().copied().filter(|&s| s > peer_recv).collect() }
