use lmbp_skeleton::ext::*;
use lmbp_skeleton::*;
use serde_json::Value;

fn load() -> Value {
    let p = concat!(env!("CARGO_MANIFEST_DIR"), "/../../testvectors/vectors.json");
    serde_json::from_str(&std::fs::read_to_string(p).unwrap()).unwrap()
}
fn s<'a>(v: &'a Value, k: &str) -> &'a str { v[k].as_str().unwrap_or("") }
fn b(v: &Value, k: &str) -> Vec<u8> { unb64u(s(v, k)).unwrap() }
fn a32(x: Vec<u8>) -> [u8; 32] { x.try_into().unwrap() }

#[test]
fn e2ee_vectors() {
    let v = load();
    let now = v["now"].as_i64().unwrap();
    let dev = unb64u(s(&v["keys"]["device"], "public_b64url")).unwrap();
    let e = &v["e2ee"];
    let ks = &e["key_statement"];
    let (x, kid) = verify_key_statement(&dev, s(ks, "pinned_jkt"), s(ks, "device_id"), s(ks, "compact"), now).unwrap();
    assert_eq!(b64u(&x), s(ks, "expect_x25519_public_b64url"));
    assert_eq!(kid, s(ks, "expect_kid"));
    let dx = &e["device_x25519"];
    let xpriv = a32(b(dx, "private_b64url"));
    assert_eq!(b64u(&x25519_public(&xpriv)), s(dx, "public_b64url"));
    assert_eq!(x25519_thumbprint(&b(dx, "public_b64url")), s(dx, "kid"));
    let j = &e["jwe"];
    assert_eq!(b64u(&concat_kdf(&b(j, "shared_secret_b64url"), "A256GCM")), s(j, "cek_b64url"));
    let mine = seal_jwe(&a32(b(dx, "public_b64url")), s(j, "recipient_kid"), s(j, "plaintext_utf8").as_bytes(),
        &a32(b(j, "ephemeral_private_b64url")), &b(j, "iv_b64url").try_into().unwrap());
    // serde_json (preserve_order) emits the header members in the same order as the reference.
    assert_eq!(mine, s(j, "compact"));
    let (h, pt) = open_jwe(&xpriv, s(j, "compact")).unwrap();
    assert_eq!(pt, s(j, "plaintext_utf8").as_bytes());
    assert_eq!(h["cty"], CTY_REQUEST);
    for c in e["chunks"].as_array().unwrap() {
        let k = b(c, "response_key_b64url");
        let seq = c["seq"].as_u64().unwrap();
        assert_eq!(seal_chunk(&k, s(c, "rid"), s(c, "type"), seq, s(c, "plaintext_utf8").as_bytes()), s(c, "sealed_b64url"));
        assert_eq!(open_chunk(&k, s(c, "rid"), s(c, "type"), seq, s(c, "sealed_b64url")).unwrap(), s(c, "plaintext_utf8").as_bytes());
        assert_eq!(chunk_aad(s(c, "rid"), s(c, "type"), seq), s(c, "aad"));
    }
    let k0 = b(&e["chunks"][0], "response_key_b64url");
    for n in e["negative"].as_array().unwrap() {
        let r = match s(n, "kind") {
            "jwe" => open_jwe(&xpriv, s(n, "compact")).map(|_| ()),
            "chunk" => open_chunk(&k0, s(n, "rid"), s(n, "type"), n["seq"].as_u64().unwrap(), s(n, "sealed_b64url")).map(|_| ()),
            _ => verify_key_statement(&dev, s(n, "pinned_jkt"), s(n, "device_id"), s(n, "compact"), now).map(|_| ()),
        };
        assert!(r.is_err(), "accepted {}", s(n, "name"));
    }
}

#[test]
fn flow_and_resume_vectors() {
    let v = load();
    for f in v["flow"].as_array().unwrap() {
        let (tw, cw) = (f["task_window"].as_u64().unwrap() as usize, f["conn_window"].as_u64().unwrap() as usize);
        if f["role"] == "sender" {
            let mut sent: std::collections::HashMap<String, Vec<(u64, usize)>> = Default::default();
            for st in f["steps"].as_array().unwrap() {
                let task = s(st, "task").to_string();
                if st["op"] == "send" {
                    let size = st["size"].as_u64().unwrap() as usize;
                    let tu: usize = sent.get(&task).map(|l| l.iter().map(|x| x.1).sum()).unwrap_or(0);
                    let cu: usize = sent.values().flatten().map(|x| x.1).sum();
                    let can = can_send(tw, cw, tu, cu, size);
                    assert_eq!(can, st["expect"] == "send", "{}", s(f, "name"));
                    if can { let l = sent.entry(task).or_default(); let n = l.last().map(|x| x.0).unwrap_or(0) + 1; l.push((n, size)); }
                } else {
                    let seq = st["seq"].as_u64().unwrap();
                    sent.entry(task).or_default().retain(|x| x.0 > seq);
                }
            }
        } else {
            let mut inb = Inbound::new(tw, cw);
            for st in f["steps"].as_array().unwrap() {
                if st["op"] == "recv" {
                    let got = inb.accept(s(st, "task"), st["seq"].as_u64().unwrap(), st["size"].as_u64().unwrap() as usize);
                    let want = match s(st, "expect") { "accepted" => Verdict::Accepted, "duplicate" => Verdict::Duplicate, "gap" => Verdict::Gap, _ => Verdict::Overflow };
                    assert_eq!(got, want, "{}", s(f, "name"));
                } else {
                    inb.acked(s(st, "task"), st["seq"].as_u64().unwrap());
                }
            }
        }
    }
    for r in v["resume"].as_array().unwrap() {
        let acked = r["acked"].as_u64().unwrap();
        let unacked: Vec<u64> = r["sent"].as_array().unwrap().iter().map(|x| x.as_u64().unwrap()).filter(|&x| x > acked).collect();
        let want: Vec<u64> = r["retransmit"].as_array().unwrap().iter().map(|x| x.as_u64().unwrap()).collect();
        assert_eq!(retransmit(&unacked, r["peer_recv_seq"].as_u64().unwrap()), want, "{}", s(r, "name"));
    }
}
