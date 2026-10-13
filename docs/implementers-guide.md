# lsp2 实现指南：用 Java、Node.js、Python、Go、Rust 实现网关

本指南面向要用自己的语言实现 lsp2 **网关**（也适用于租户后端或设备端）的团队。规范以 [`docs/protocol-v2.md`](protocol-v2.md) 为准，本文只讲实现方法和常见陷阱。

## 0. 先看这几份材料

| 材料 | 用途 |
|---|---|
| `spec/schemas/*.json` | JSON Schema 2020-12，可直接用来校验每一帧 |
| `spec/openapi.yaml` | HTTP 接口，可用于生成服务端桩代码或客户端 |
| `spec/asyncapi.yaml` | WebSocket 通道和消息 |
| `spec/testvectors/vectors.json` | 固定密钥、期望签名和 JWT、正例与反例，以及合法和非法的帧 |
| `spec/examples/{python,node,rust}` | 关键路径的骨架代码，**都已用测试向量实际运行通过** |
| `cmd/lsp2-conformance` | 黑盒测试你的网关，共 31 项检查 |
| `spec/validate.sh` | 一键校验所有契约，并运行示例骨架 |

### 代码片段的验证状态

| 语言 | 状态 |
|---|---|
| Go | 参考实现（`internal/lsp2`）。`go test` 覆盖测试向量、Schema 和一致性测试 |
| Python | `spec/examples/python`：已在 Python 3.13 + cryptography 上运行，60 项全部通过 |
| Node.js | `spec/examples/node`：已在 Node 20 上运行，只用 `node:crypto`，Schema 校验用 ajv，59 项全部通过 |
| Rust | `spec/examples/rust`：已在 rustc 1.85 + ed25519-dalek 2 上运行，`cargo test` 通过 |
| Java | **仅为示意**，本环境没有 JDK，未编译。下文 Java 片段需要你自行用测试向量验证 |

本文中 Go、Python、Node、Rust 的片段摘自上述已运行的文件，或是它们的精简版。

---

## 1. 推荐库

| 能力 | Java | Node.js | Python | Go | Rust |
|---|---|---|---|---|---|
| HTTP 服务 | Spring Boot / Quarkus / Javalin | Fastify / Express / `node:http` | FastAPI / Starlette | `net/http` | axum |
| WebSocket（需支持子协议选择） | Jakarta WebSocket（Tyrus）、Spring WebSocket、Netty | `ws`（用 `handleProtocols` 选择 `lsp2`） | `websockets`（`subprotocols=["lsp2"]`）、Starlette `websocket.accept(subprotocol="lsp2")` | `gorilla/websocket`（`Upgrader.Subprotocols`）、`coder/websocket` | `axum::extract::ws`（`.protocols(["lsp2"])`）、`tokio-tungstenite` |
| Ed25519 | JDK 15+ `Signature.getInstance("Ed25519")`；BouncyCastle | `node:crypto`（`sign(null, …)`） | `cryptography` | `crypto/ed25519` | `ed25519-dalek` 2（用 `verify_strict`） |
| JWS/JWT（EdDSA） | Nimbus JOSE+JWT（`Ed25519Signer`/`Ed25519Verifier`，需 Tink）、jose4j | `jose`（panva），或像骨架那样直接用 `node:crypto` | `joserfc`、PyJWT[crypto]（`algorithms=["EdDSA"]`） | 自己实现（参考实现约 80 行）、`go-jose` v4、`lestrrat-go/jwx` v2 | `josekit`、`jsonwebtoken` 9（支持 EdDSA），或像骨架那样手写 |
| JSON Schema 2020-12 | `com.networknt:json-schema-validator` | `ajv`（`ajv/dist/2020`） | `jsonschema` + `referencing` | `santhosh-tekuri/jsonschema` v6 | `jsonschema` crate |
| SSE | Spring `SseEmitter`、WebFlux `Flux<ServerSentEvent>` | `res.write("data: …\n\n")`，处理 `drain` 事件 | `StreamingResponse`、`sse-starlette` | `http.Flusher` | `axum::response::sse::Sse` |
| SQLite / Postgres | `sqlite-jdbc`；PostgreSQL JDBC + HikariCP | `better-sqlite3`；`pg` | `sqlite3`/`aiosqlite`；`asyncpg`/`psycopg` 3 | `modernc.org/sqlite`；`pgx` | `sqlx`（sqlite/postgres） |
| AES-GCM（静态加密） | `Cipher.getInstance("AES/GCM/NoPadding")` | `crypto.createCipheriv('aes-256-gcm')` | `cryptography` AESGCM | `crypto/cipher` | `aes-gcm` |

**不推荐**：
- python-jose：已停止维护，曾出现算法混淆漏洞；
- 任何"自动从 JWT 头读取 alg"的验证 API：必须固定 `alg=EdDSA`；
- 依赖库默认行为的 base64 解码（见 §2.2）。

---

## 2. 跨语言陷阱

### 2.1 JSON
- **签名不依赖 JSON 序列化**。lsp2 一律对**收到的字节**验签，然后再解析 JSON。不要先解析再重新序列化后验签。不同语言的序列化结果互不相同：
  - Go 的 `json.Marshal` 默认把 `<`、`>`、`&` 转义为 `\u003c` 等；
  - Python 的 `json.dumps` 默认 `ensure_ascii=True`，会把中文转成 `\uXXXX`；
  - JavaScript 会把 `1.0` 输出为 `1`；
  - Java Jackson 的键顺序取决于字段声明顺序。
  
  测试向量里的 `task_run` 载荷故意包含中文、emoji、引号、反斜杠和 `</script>`，用来暴露这类问题。
- **重复键**：Go 和 Python 取最后一个值，Jackson 默认也是。安全敏感的实现 SHOULD 开启严格模式：Jackson 用 `StreamReadFeature.STRICT_DUPLICATE_DETECTION`，Python 用 `object_pairs_hook` 检查。
- **Go 的大小写不敏感匹配**：`encoding/json` 匹配字段名时**不区分大小写**，`{"Device_ID": …}` 也会被解析到 `DeviceID` 字段。需要严格匹配时，先解析到 `map` 再检查。
- **数字**：所有整数都 ≤ 2^53-1，因此 JavaScript 的 `number` 足够。不要输出 `1.0`、`1e3`。Python 的 `json.loads` 默认接受 `NaN`/`Infinity`，应该用 `parse_constant` 拒绝。
- **Unicode**：只接受合法 UTF-8。单独的代理项（`"\ud800"`）应该拒绝：Python 默认接受，Go 会替换成 U+FFFD。

### 2.2 base64url
- 编码时**不带填充**：
  - Java：`Base64.getUrlEncoder().withoutPadding()`
  - Node：`Buffer.toString('base64url')`
  - Python：`urlsafe_b64encode(...).rstrip(b"=")`
  - Go：`base64.RawURLEncoding`
  - Rust：`URL_SAFE_NO_PAD`
- 解码时要**严格**：
  - Node 的 `Buffer.from(s, 'base64url')` 会静默忽略非法字符；
  - Python 的 `urlsafe_b64decode` 要求补齐填充，且会忽略部分非法字符；
  - Java 的 `getUrlDecoder()` 接受带填充的输入。
  
  这三种语言都要先用正则 `^[A-Za-z0-9_-]*$` 检查，并拒绝长度模 4 余 1 的输入。Go 的 `RawURLEncoding` 和 Rust 的 `URL_SAFE_NO_PAD` 本身就是严格的。

### 2.3 Ed25519 与 JWS
- 固定 `alg == "EdDSA"`，同时检查 `typ`（四种类型，见规范 §5.2）。测试向量覆盖了 `alg_none`、`alg_hs256`、类型混淆和 `crit` 等反例。
- Ed25519 是确定性签名：同一个种子对同一个 `signing_input` 的签名必须与测试向量**逐字节相同**。可以借此检查私钥加载是否正确。
- 私钥格式：
  - Java PKCS#8 的 DER 前缀为 `302e020100300506032b657004220420`，后面跟 32 字节种子；
  - Node 可以用 JWK 导入（`{kty:'OKP',crv:'Ed25519',d,x}`）；
  - Python 用 `Ed25519PrivateKey.from_private_bytes(seed)`；
  - Go 用 `ed25519.NewKeyFromSeed`；
  - Rust 用 `SigningKey::from_bytes`。
- RFC 7638 指纹：哈希的输入是 `{"crv":"Ed25519","kty":"OKP","x":"…"}`，**成员按字典序排列，不含空白**。不要用 JSON 库序列化 JWK 对象来生成这个输入，直接拼接字符串最可靠。

### 2.4 时钟偏差
- 访问令牌允许 ±30 秒偏差，证明 JWT 允许 ±60 秒。服务器必须用 NTP 同步时钟。
- `jti` 去重表的保留时间要大于允许偏差加有效期（参考实现保留 3 分钟）。多实例部署时，去重表要放在 Redis 等共享存储里，例如 `SET jti NX EX 180`。

### 2.5 背压与并发
- 每个 WebSocket 连接只能有一个写入者。各语言都需要加写锁或使用发送队列：Go 用 `sync.Mutex`；Node 的 `ws` 自带队列，但要关注 `bufferedAmount`；Python 用 `asyncio.Lock`；Rust 用 `mpsc` 通道。
- 每个任务的缓冲区要有上限。SSE 客户端跟不上时，取消任务或断开会话，**不要无限缓冲**。Node 中 `res.write` 返回 `false` 时要等待 `drain` 事件。
- 只要 HTTP 客户端断开，就必须发送 `task.cancel`：Node 监听 `req.on('close')`，Starlette 检查 `await request.is_disconnected()`，Go 监听 `r.Context().Done()`，Spring 用 `SseEmitter.onCompletion`/`onError`。

### 2.6 反向代理
- Nginx 需要设置 `proxy_set_header Upgrade/Connection`、`proxy_read_timeout 75s`；SSE 需要 `proxy_buffering off`（或在响应头加 `X-Accel-Buffering: no`）。
- 如果代理对 `Sec-WebSocket-Protocol` 头做了改写或过滤，子协议协商会失败。

---

## 3. 最小网关实现步骤

1. **存储**：建四张表。
   - `tenants(id, api_key_sha256)`
   - `device_auth(device_code_sha256, user_code, tenant, public_key, status, expires_at, interval, last_poll, device_id, owner)`
   - `devices(id, tenant, owner, name, public_key, status, created_at)`
   - `tasks(id, tenant, owner, device_id, state, request_enc, …)`
2. **密钥**：从环境变量或 KMS 加载 32 字节的网关 Ed25519 种子，计算 `kid = RFC7638(pub)`。实现 `GET /.well-known/jwks.json`。
3. **`POST /v2/device/authorize`**：解析表单参数，检查公钥是否为 32 字节，生成 `device_code`（只存哈希）和 `user_code`，返回结果并带上 `Cache-Control: no-store`。
4. **`POST /v2/tenant/device-approvals`**：用租户 Key 的哈希查出租户。在租户内按 `user_code` 查找待确认记录。确认后创建设备记录，归属 `X-End-User`。失败要计数限速。
5. **`POST /v2/token`**：
   - device_code 授权：先验证证明，再看状态，最后标记为已使用（规范 §6.3 的顺序）；
   - jwt-bearer 授权：用设备公钥验证断言，并对 `jti` 去重。
   - 签发 `at+jwt`，内含 `cnf.jkt`。
6. **`GET /v2/connect`**：
   1. 检查子协议；
   2. 拒绝带 `Origin` 的请求；
   3. 验证访问令牌，并核对设备状态和 `cnf.jkt`；
   4. 升级连接，选择 `lsp2`；
   5. 15 秒内读取 `hello`，验证证明（`ath` 绑定本次连接的令牌）；
   6. 回复 `welcome`，`capabilities` 取交集；
   7. 记录 `sessions[device_id]`，并关闭该设备的旧会话。
7. **读循环**：
   - `task.delta`/`task.done`/`task.error`：检查 `seq` 连续后，转发到任务通道；
   - `session.token`：验证后回复；
   - `models.update`：更新模型表；
   - `ping`：回复 `pong`；
   - 其他类型：回复 `error`，但不断开。
   - 定时发送 WebSocket ping，并检查令牌是否已过期超过 30 秒。
8. **`POST /v1/chat/completions`**：
   1. 认证租户；
   2. 解析模型路由；
   3. 检查设备并发；
   4. 构造 `task.run` 载荷，签名后发送 `{v:2,type:"task.run",task_id,jws}`；
   5. 把设备回报转换成 SSE；
   6. 客户端断开或超时时发送 `task.cancel`；
   7. 设备断线时任务失败（`DEVICE_OFFLINE`）。
9. **吊销**：把设备状态改为 `revoked`，并关闭在线会话。
10. **验证**：先跑测试向量，再执行 `lsp2-conformance -gateway http://localhost:PORT`，直到 31 项全部通过。

---

## 4. 各语言关键代码片段

下面每种语言都覆盖四个关键点：**验证 hello**、**签发令牌**、**签名 task.run**、**输出 SSE 块**。

### 4.1 Python（已运行：`spec/examples/python/lsp2.py`）
```python
def verify_hello(gw_pub, device_pub_of, frame, access_token, aud="lsp2-gateway", now=None):
    claims = verify_access_token(gw_pub, access_token, aud, now)          # typ=at+jwt, aud, exp/iat ±30s
    dev_pub = device_pub_of(claims["sub"])                                # 查库，且 status == active
    if claims.get("cnf", {}).get("jkt") != thumbprint(dev_pub):
        raise Lsp2Error("token not bound to this device key")
    if frame.get("v") != 2 or frame.get("type") != "hello" or frame["data"].get("device_id") != claims["sub"]:
        raise Lsp2Error("bad hello")
    verify_proof(dev_pub, frame["data"]["proof"], claims["sub"], "lsp2:hello", access_token, now)
    return claims   # 调用方还要对 proof 的 jti 去重

def issue_access_token(gw_key, kid, device_id, tenant, device_pub, ttl=900, now=None, jti=None):
    now = int(time.time()) if now is None else now
    claims = {"iss": "lsp2-gateway", "sub": device_id, "aud": "lsp2-gateway", "tenant": tenant, "iat": now,
              "exp": now + ttl, "jti": jti or "jti_" + secrets.token_hex(16), "cnf": {"jkt": thumbprint(device_pub)}}
    return sign_jws(gw_key, "at+jwt", json_bytes(claims), kid)

def sign_task_run(gw_key, kid, payload):
    return {"v": 2, "type": "task.run", "task_id": payload["task_id"],
            "jws": sign_jws(gw_key, "lsp2-task+jws", json_bytes(payload), kid)}

def sse_chunk(task_id, model, created, text=None, finish=None):
    delta = {"content": text} if text is not None else {}
    obj = {"id": "chatcmpl-" + task_id, "object": "chat.completion.chunk", "created": created, "model": model,
           "choices": [{"index": 0, "delta": delta, "finish_reason": finish}]}
    return b"data: " + json_bytes(obj) + b"\n\n"
```
FastAPI 中接入 WebSocket（示意）：`await ws.accept(subprotocol="lsp2")`，在此之前先检查 `ws.headers["sec-websocket-protocol"]`、`Authorization` 以及是否带有 `Origin`。

### 4.2 Node.js（已运行：`spec/examples/node/lsp2.mjs`）
```js
export function verifyHello(gwPub, devicePubOf, frame, accessToken, { aud = 'lsp2-gateway', now } = {}) {
  const claims = verifyAccessToken(gwPub, accessToken, aud, now);
  const devPub = devicePubOf(claims.sub);
  if (claims.cnf?.jkt !== thumbprint(devPub)) throw new Error('token not bound to device key');
  if (frame.v !== 2 || frame.type !== 'hello' || frame.data?.device_id !== claims.sub) throw new Error('bad hello');
  verifyProof(devPub, frame.data.proof, { iss: claims.sub, aud: 'lsp2:hello', credential: accessToken }, now);
  return claims;
}
export function issueAccessToken(gwKey, kid, { deviceId, tenant, devicePub, ttl = 900, now = Math.floor(Date.now() / 1000), jti }) {
  const claims = { iss: 'lsp2-gateway', sub: deviceId, aud: 'lsp2-gateway', tenant, iat: now, exp: now + ttl,
    jti: jti ?? `jti_${randomBytes(16).toString('hex')}`, cnf: { jkt: thumbprint(devicePub) } };
  return signJws(gwKey, 'at+jwt', jsonBytes(claims), kid);
}
export const signTaskRun = (gwKey, kid, payload) =>
  ({ v: 2, type: 'task.run', task_id: payload.task_id, jws: signJws(gwKey, 'lsp2-task+jws', jsonBytes(payload), kid) });
export async function writeSseChunk(res, { taskId, model, created, text, finish = null }) {
  const obj = { id: `chatcmpl-${taskId}`, object: 'chat.completion.chunk', created, model,
    choices: [{ index: 0, delta: text === undefined ? {} : { content: text }, finish_reason: finish }] };
  if (!res.write(`data: ${JSON.stringify(obj)}\n\n`)) await new Promise((r) => res.once('drain', r));
}
```
使用 `ws` 库接入（示意）：
```js
const wss = new WebSocketServer({ noServer: true, handleProtocols: (ps) => (ps.has('lsp2') ? 'lsp2' : false) });
server.on('upgrade', (req, sock, head) => {
  if (req.headers.origin) return sock.end('HTTP/1.1 403 Forbidden\r\n\r\n');
  // 在这里验证 Authorization 中的令牌，失败时返回 401
  wss.handleUpgrade(req, sock, head, (ws) => onSession(ws, token));
});
```

### 4.3 Go（参考实现：`internal/lsp2`、`internal/lsp2/gateway`）
```go
// 验证 hello（gateway.go）
claims, err := lsp2.VerifyJWT(g.pub, tok, lsp2.TypAccessToken, Audience, time.Now())
// ……查设备，检查 status == active，且 claims.Cnf["jkt"] == lsp2.Thumbprint(dev.PublicKey)
pc, err := lsp2.VerifyProof(dev.PublicKey, hello.Proof, dev.ID, lsp2.ProofHello, tok, time.Now())
if err != nil || !g.useJTI(pc.Jti, time.Now().Add(3*time.Minute)) { /* error UNAUTHENTICATED，然后断开 */ }

// 签发令牌
c := lsp2.Claims{Iss: Issuer, Sub: d.ID, Aud: Audience, Tenant: d.TenantID, Iat: now.Unix(), Exp: now.Add(ttl).Unix(),
	Jti: lsp2.RandomID("jti_"), Cnf: map[string]string{"jkt": lsp2.Thumbprint(d.PublicKey)}}
at := lsp2.SignJWT(signingKey, lsp2.TypAccessToken, kid, c)

// 签名 task.run
jws := lsp2.SignTaskRun(signingKey, kid, lsp2.TaskRun{TaskID: id, Iat: now, Exp: now + 60, Nonce: lsp2.RandomID("n_"), DeviceID: dev, Model: m, Messages: msgs})
s.send(lsp2.Envelope{V: 2, Type: lsp2.TypeTaskRun, TaskID: id, JWS: jws})

// SSE
fmt.Fprintf(w, "data: %s\n\n", chunkJSON); w.(http.Flusher).Flush()
```

### 4.4 Rust（已运行：`spec/examples/rust/src/lib.rs`）
```rust
pub fn verify_hello(gw: &[u8], device_pub_of: impl Fn(&str) -> Vec<u8>, frame: &Value, token: &str, now: i64) -> Result<Value> {
    let c = verify_access_token(gw, token, "lsp2-gateway", now)?;
    let sub = c["sub"].as_str().ok_or("sub")?.to_string();
    let dev = device_pub_of(&sub);
    if c["cnf"]["jkt"] != thumbprint(&dev).as_str() { return Err("token not bound to device key".into()); }
    if frame["v"] != 2 || frame["type"] != "hello" || frame["data"]["device_id"] != sub.as_str() { return Err("bad hello".into()); }
    verify_proof(&dev, frame["data"]["proof"].as_str().ok_or("proof")?, &sub, "lsp2:hello", token, now)?;
    Ok(c)
}
pub fn issue_access_token(gw: &SigningKey, kid: &str, device_id: &str, tenant: &str, device_pub: &[u8], now: i64, ttl: i64, jti: &str) -> String {
    let c = json!({"iss":"lsp2-gateway","sub":device_id,"aud":"lsp2-gateway","tenant":tenant,"iat":now,"exp":now+ttl,"jti":jti,"cnf":{"jkt":thumbprint(device_pub)}});
    sign_jws(gw, TYP_ACCESS_TOKEN, c.to_string().as_bytes(), Some(kid))
}
pub fn sign_task_run(gw: &SigningKey, kid: &str, payload: &Value) -> Value {
    json!({"v":2,"type":"task.run","task_id":payload["task_id"],"jws":sign_jws(gw, TYP_TASK, payload.to_string().as_bytes(), Some(kid))})
}
pub fn sse_chunk(task_id: &str, model: &str, created: i64, text: &str) -> String {
    format!("data: {}\n\n", json!({"id":format!("chatcmpl-{task_id}"),"object":"chat.completion.chunk","created":created,"model":model,
        "choices":[{"index":0,"delta":{"content":text},"finish_reason":null}]}))
}
```
axum 接入（示意）：`ws.protocols(["lsp2"]).on_upgrade(...)`。升级前先从 `HeaderMap` 中验证令牌，并拒绝带 `Origin` 的请求。SSE 用 `Sse::new(stream).keep_alive(KeepAlive::new().interval(Duration::from_secs(15)))`。

### 4.5 Java（**示意，未编译**；JDK 17+，只用 JDK 自带的 EdDSA 与 Jackson）
```java
static final Base64.Encoder B64 = Base64.getUrlEncoder().withoutPadding();
static byte[] unb64(String s) {
  if (!s.matches("[A-Za-z0-9_-]*") || s.length() % 4 == 1) throw new IllegalArgumentException("base64url");
  return Base64.getUrlDecoder().decode(s);
}
// 由 32 字节种子得到私钥：PKCS#8 = 固定前缀 + 种子
static PrivateKey privFromSeed(byte[] seed) throws Exception {
  byte[] p = HexFormat.of().parseHex("302e020100300506032b657004220420");
  return KeyFactory.getInstance("Ed25519").generatePrivate(new PKCS8EncodedKeySpec(concat(p, seed)));
}
static PublicKey pubFromRaw(byte[] x) throws Exception {   // X.509 = 固定前缀 + 32 字节
  byte[] p = HexFormat.of().parseHex("302a300506032b6570032100");
  return KeyFactory.getInstance("Ed25519").generatePublic(new X509EncodedKeySpec(concat(p, x)));
}
static String thumbprint(byte[] x) throws Exception {
  String in = "{\"crv\":\"Ed25519\",\"kty\":\"OKP\",\"x\":\"" + B64.encodeToString(x) + "\"}";
  return B64.encodeToString(MessageDigest.getInstance("SHA-256").digest(in.getBytes(UTF_8)));
}
static String signJws(PrivateKey k, String typ, String kid, byte[] payload) throws Exception {
  String h = kid == null ? "{\"alg\":\"EdDSA\",\"typ\":\"" + typ + "\"}" : "{\"alg\":\"EdDSA\",\"kid\":\"" + kid + "\",\"typ\":\"" + typ + "\"}";
  String in = B64.encodeToString(h.getBytes(UTF_8)) + "." + B64.encodeToString(payload);
  Signature s = Signature.getInstance("Ed25519"); s.initSign(k); s.update(in.getBytes(US_ASCII));
  return in + "." + B64.encodeToString(s.sign());
}
static JsonNode verifyJws(PublicKey pub, String jws, String typ) throws Exception {
  String[] p = jws.split("\\.", -1);
  if (p.length != 3) throw new SecurityException("malformed");
  JsonNode h = STRICT_MAPPER.readTree(unb64(p[0]));   // 开启 STRICT_DUPLICATE_DETECTION
  if (!"EdDSA".equals(h.path("alg").asText()) || !typ.equals(h.path("typ").asText()) || h.has("crit")) throw new SecurityException("header");
  Signature s = Signature.getInstance("Ed25519"); s.initVerify(pub); s.update((p[0] + "." + p[1]).getBytes(US_ASCII));
  if (!s.verify(unb64(p[2]))) throw new SecurityException("signature");
  return STRICT_MAPPER.readTree(unb64(p[1]));
}
// verifyHello：verifyJws(gw, token, "at+jwt") → 检查 aud/exp/iat → 查设备并比对 cnf.jkt
//              → verifyJws(dev, proof, "lsp2-proof+jwt") → 检查 aud="lsp2:hello"、iss、ath=b64(sha256(token))、|now-iat|≤60，并对 jti 去重
// signTaskRun：signJws(gwKey, "lsp2-task+jws", kid, mapper.writeValueAsBytes(payload))
// SSE（Spring MVC）：emitter.send(SseEmitter.event().data(chunkJson, MediaType.APPLICATION_JSON))；结束时 emitter.send("[DONE]")
```
接入 Spring WebSocket 时，在 `HandshakeInterceptor` 中验证令牌和 `Origin`，并在 `DefaultHandshakeHandler` 中选择子协议 `lsp2`（`setSupportedProtocols("lsp2")`）。**用 Java 实现前，请先把上面的函数对照 `vectors.json` 跑一遍**：要求签名逐字节一致，所有反例都被拒绝。

---

## 5. 设备端（如果你也要实现）

设备端只需要以下几步：
1. 生成并保存 Ed25519 私钥（存入系统密钥库）；
2. 通过发现文档完成 RFC 8628 配对，每次轮询都用新的证明；
3. 用 jwt-bearer 断言换取令牌，建立 WSS 连接，发送 `hello`（附证明）；
4. 验证每个 `task.run`，依次检查 JWS、`task_id`、`device_id`、`exp`、`nonce`、模型允许列表和并发数；
5. 流式回报结果，`seq` 从 1 开始连续递增；
6. 在到期前通过 `session.token` 续期；
7. 断线后按指数退避重连；收到 `invalid_grant` 后停止，提示用户重新配对。

Go 设备端参考实现见 `internal/lsp2/client`。
