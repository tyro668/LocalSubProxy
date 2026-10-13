# LocalSubProxy 协议 v2（lsp2）规范

| 项目 | 内容 |
|---|---|
| 状态 | 草案（Draft），版本 2.0 |
| 线上标识 | WebSocket 子协议 `lsp2`；信封字段 `v: 2` |
| 机器可读契约 | [`spec/schemas/`](../spec/schemas)（JSON Schema 2020-12）、[`spec/asyncapi.yaml`](../spec/asyncapi.yaml)（AsyncAPI 3.0）、[`spec/openapi.yaml`](../spec/openapi.yaml)（OpenAPI 3.1） |
| 测试向量 | [`spec/testvectors/vectors.json`](../spec/testvectors/vectors.json) |
| 一致性测试 | `go run ./cmd/lsp2-conformance`（黑盒测试任意语言实现的网关） |
| 参考实现 | Go：`internal/lsp2`（协议与 JOSE）、`internal/lsp2/gateway`（网关）、`internal/lsp2/client`（设备端） |
| 实现指南 | [`docs/implementers-guide.md`](implementers-guide.md)：Java、Node.js、Python、Go、Rust |

本文规定 lsp2 的线上行为。任何语言实现的网关或设备端，只要满足本文的 MUST 要求，就能与其他实现互通。本文刻意不引用任何语言或库的行为：所有编码、签名和时间格式都由下文列出的 IETF/W3C 标准定义。

**目录**
1. 约定与术语
2. 引用的标准与设计决策
3. 角色与总体流程
4. 编码规则
5. 密钥、令牌与证明（JOSE 配置）
6. HTTP 接口
7. WebSocket 通道
8. 状态机
9. 错误
10. 超时与限制
11. 版本与能力协商
12. 安全要求
13. 一致性等级与测试
14. 断线续传（能力 `resume`）
15. 流量控制（能力 `flow`）
16. tools 与多模态（能力 `tools`、`multimodal`）
17. 端到端加密（能力 `e2ee`）
18. 网关身份绑定
19. 配对同意（签名元数据、`lsp2://pair`、`CONSENT_DENIED`）
20. 本机审计日志与用量统计
21. 尚未定义的内容

---

## 1. 约定与术语

本文中的 **MUST**、**MUST NOT**、**REQUIRED**、**SHALL**、**SHALL NOT**、**SHOULD**、**SHOULD NOT**、**RECOMMENDED**、**NOT RECOMMENDED**、**MAY**、**OPTIONAL**，仅在全部大写时按 [BCP 14](https://www.rfc-editor.org/info/bcp14)（RFC 2119、RFC 8174）解释。

| 术语 | 含义 |
|---|---|
| 设备（device / connector） | 运行在用户电脑上的程序。持有设备私钥，负责调用本机模型。 |
| 网关（gateway） | 公网服务。负责配对、签发令牌、维持设备的 WebSocket 会话、下发任务，并对外提供 OpenAI 兼容接口。 |
| 租户后端（tenant back end / SaaS） | 业务方服务器。持有租户 API Key，代表已登录的终端用户调用网关。 |
| 终端用户（end user） | 租户里的一个人，由租户后端通过 `X-End-User` 告知网关。一台设备只属于一个终端用户。 |
| 任务（task） | 一次模型调用。网关以 `task.run` 下发，设备以 `task.delta`/`task.done`/`task.error` 回报。 |
| 帧（frame） | 一条 WebSocket 文本消息，正好包含一个 JSON 对象。 |

---

## 2. 引用的标准与设计决策

| 需求 | 采用的标准 | 理由 |
|---|---|---|
| 数据格式 | JSON，RFC 8259（I-JSON，RFC 7493 子集） | 所有语言都有可靠实现 |
| 二进制转文本 | base64url，**不带填充**，RFC 4648 §5 | 与 JOSE 一致 |
| 签名算法 | Ed25519，RFC 8032；在 JOSE 中写作 `alg: "EdDSA"`，RFC 8037 | 确定性签名，密钥短，无参数选择问题 |
| 签名封装 | **JWS 紧凑序列化**，RFC 7515，载荷内嵌 | 见下面的决策 D1 |
| 令牌 | JWT，RFC 7519；访问令牌采用 RFC 9068 配置（`typ: at+jwt`） | 标准声明 iss/sub/aud/iat/exp/jti |
| 令牌与密钥绑定 | `cnf.jkt`，RFC 7800 / RFC 9449；指纹按 RFC 7638 计算 | 被盗的令牌离开设备私钥就无法使用 |
| 持钥证明 | DPoP 风格的证明 JWT（借鉴 RFC 9449 的 `ath` 和 `jti`） | 见决策 D2 |
| 显式类型 | 每种 JWS 都有不同的 `typ`，遵循 RFC 8725 §3.11 | 防止把一种令牌冒充成另一种 |
| 配对 | OAuth 2.0 设备授权流程，RFC 8628 | 设备上不需要浏览器，也不需要输入密码 |
| 令牌刷新 | JWT Bearer 授权，RFC 7523 §2.1 | 设备私钥是唯一的长期凭据，不需要 refresh token |
| 公钥发布 | JWK Set，RFC 7517，路径 `/.well-known/jwks.json` | 支持密钥轮换 |
| HTTP 错误 | OAuth 接口按 RFC 6749 §5.2；管理接口按 RFC 9457 Problem Details；OpenAI 接口使用 OpenAI 错误对象 | 每个接口沿用其生态的惯例 |
| 实时通道 | WebSocket，RFC 6455，子协议 `lsp2`，强制 TLS | 双向、可穿透 NAT |
| 流式输出 | Server-Sent Events（WHATWG HTML §9.2），OpenAI `chat.completion.chunk` 格式 | 现有 OpenAI SDK 可以直接使用 |
| 时间 | JWT 和帧内统一用 NumericDate（整数秒，RFC 7519 §2）；HTTP 管理接口的时间字段用 RFC 3339 UTC 字符串 | 整数没有时区和格式问题 |
| 标识符 | 不透明字符串，见 §4.4；RECOMMENDED 使用 UUIDv7（RFC 9562）或 ULID | 按时间排序，便于建立索引 |

**D1：签名封装采用 JWS 紧凑序列化，不使用 JSON 规范化（JCS）。**
`task.run` 的载荷直接放在 JWS 里：先把载荷序列化成 UTF-8 JSON 字节，再签名；接收方按收到的字节验证，然后才解析 JSON。这样，各语言的 JSON 序列化差异（键顺序、数字格式、Unicode 转义、`<` 是否转义成 `\u003c`）都不会影响验签。也就不需要 RFC 8785（JCS）：JCS 实现少，对数字的处理也容易出错。分离式载荷（RFC 7797，`b64: false`）同样**不使用**。

**D2：持钥证明采用证明 JWT。**
本协议中，访问令牌不止用于 HTTP 请求，还要在 WebSocket 握手和会话期间使用。RFC 9449 的 `htm`/`htu` 是为 HTTP 请求设计的，不适用于这些场景。因此本协议定义了自己的 `typ: lsp2-proof+jwt`，用 `aud` 区分用途，用 `ath` 绑定具体凭据。它的结构和校验方法与 DPoP 相同，主流 JOSE 库都能直接处理。

---

## 3. 角色与总体流程

```
 终端用户浏览器 ── 登录 ──▶ 租户后端（持租户 API Key）
                               │  HTTPS：/v2/tenant/*、/v1/*（Bearer 租户 Key + X-End-User）
                               ▼
 设备 ── HTTPS：/v2/device/authorize、/v2/token ──▶ 网关
 设备 ══ WSS：/v2/connect（子协议 lsp2）══════════▶ 网关
```

1. **配对（RFC 8628）**：设备生成 Ed25519 密钥对，把公钥发到 `/v2/device/authorize`，拿到 `user_code`。用户在已登录的租户页面输入 `user_code`，租户后端调用 `/v2/tenant/device-approvals` 确认。设备带上证明 JWT 轮询 `/v2/token`，得到 `device_id` 和第一个访问令牌。
2. **连接**：设备用访问令牌建立 WebSocket 连接，第一帧发送 `hello`，其中包含证明 JWT。网关回复 `welcome`。
3. **执行**：租户后端调用 `/v1/chat/completions`，网关向设备下发 `task.run`（JWS 签名），设备流式回报结果，网关转换成 SSE 返回。
4. **续期**：令牌到期前，设备用 jwt-bearer 断言换取新令牌，并通过 `session.token` 交给网关，连接不中断。

---

## 4. 编码规则

### 4.1 JSON
- 所有 JSON 文本 MUST 是 UTF-8 编码的 RFC 8259 JSON，SHOULD 符合 I-JSON（RFC 7493）。
- 发送方 MUST NOT 产生重复的成员名。接收方 SHOULD 拒绝含重复成员名的消息。签名内容总是按收到的字节验证，所以重复成员名不会绕过签名；但不同解析器可能取不同的值。Go 参考实现目前不做这项检查。
- 接收方 MUST 忽略不认识的成员（前向兼容）。
- 整数 MUST 落在 [-(2^53-1), 2^53-1] 内。本协议中的整数（时间戳、seq、计数）都是非负整数，MUST NOT 带小数点或指数（`1.0`、`1e3` 不是合法整数）。
- 字符串 MUST 是合法 Unicode，MUST NOT 包含单独的代理项（lone surrogate）。

### 4.2 base64url
- 采用 RFC 4648 §5 字母表，**MUST NOT 带 `=` 填充**。
- 接收方 MUST 拒绝含填充、空白或字母表以外字符的输入。测试向量 `padded_base64url` 覆盖这一点。

### 4.3 时间
- 帧和 JWT 中的时间字段（`iat`、`exp`、`token_expires_at`）是 NumericDate：自 1970-01-01T00:00:00Z 起的整数秒。
- HTTP 管理接口中名为 `*_at` 的字符串字段使用 RFC 3339 格式，时区 MUST 为 `Z`，例如 `2026-01-01T00:00:00Z`。
- 时钟偏差：访问令牌和断言允许 30 秒偏差，证明 JWT 允许 60 秒（§5.4）。实现 SHOULD 使用 NTP 同步时钟。

### 4.4 标识符
- `device_id`、`task_id`、`session_id`、`jti`、`nonce`、帧的 `id` 都是不透明字符串：长度 1–128，字符集 `[A-Za-z0-9._~-]`。
- 网关 MAY 给标识符加类型前缀（参考实现用 `dev_`、`task_`、`sess_`）。`device_id` MUST NOT 包含 `/`，因为它会出现在模型 ID 里（§6.6）。
- 每个标识符 MUST 至少有 122 比特随机性，或者是 UUIDv7/ULID。

---

## 5. 密钥、令牌与证明（JOSE 配置）

### 5.1 密钥
- 设备密钥和网关密钥都是 Ed25519 密钥（RFC 8032）。
- 在 JSON 中，公钥以 RFC 8037 OKP JWK 表示：`{"kty":"OKP","crv":"Ed25519","x":"<base64url 32 字节>"}`。在设备授权请求中，公钥以裸 32 字节的 base64url 传输。
- **指纹**：按 RFC 7638 计算，即 `base64url(SHA-256('{"crv":"Ed25519","kty":"OKP","x":"' + x + '"}'))`。成员按字典序排列，不含空白。指纹用作 `cnf.jkt` 和 `kid`。
- 设备私钥 MUST NOT 离开设备，SHOULD 保存在系统密钥库（Keychain、DPAPI、Secret Service）中，至少 MUST 加密存储。
- 网关 MUST 在 `/.well-known/jwks.json` 发布当前签名公钥。轮换期间 MUST 同时发布新旧两把公钥，时长不少于访问令牌 TTL。

### 5.2 JWS 通用规则
所有 JWS 都使用紧凑序列化，受保护头 MUST 包含 `alg: "EdDSA"` 和 `typ`，MAY 包含 `kid`。验证方 MUST 依次：
1. 确认正好有三段，且每段都是合法 base64url（§4.2）；
2. 确认 `alg` 恰好为 `EdDSA`，**拒绝 `none`、`HS*`、`RS*` 等其他值**；
3. 确认 `typ` 等于预期值（下表）；
4. 遇到 `crit` 头，或 `b64: false`，MUST 拒绝；
5. 对 ASCII 字节串 `protected || "." || payload` 验证 Ed25519 签名，SHOULD 使用严格验证（拒绝小阶公钥和非规范 S 值）；
6. 签名通过后，才解析载荷 JSON。

| 用途 | `typ` | 签名方 | `kid` |
|---|---|---|---|
| 访问令牌 | `at+jwt` | 网关 | 网关公钥指纹（REQUIRED） |
| 客户端断言（刷新） | `JWT` | 设备 | 设备公钥指纹（OPTIONAL） |
| 证明 JWT | `lsp2-proof+jwt` | 设备 | 无 |
| 任务签名 | `lsp2-task+jws` | 网关 | 网关公钥指纹（REQUIRED） |
| E2EE 密钥声明（§17.1） | `lsp2-e2ee-key+jwt` | 设备 | 设备公钥指纹 |

### 5.3 访问令牌（RFC 9068 配置）
| 声明 | 要求 |
|---|---|
| `iss` | 网关的签发者标识 = 网关的 https 源（origin），如 `https://gw.example.com`（§18.1） |
| `sub` | `device_id` |
| `aud` | `<iss>/tenants/<tenant>`：同时绑定网关和 SaaS（租户），见 §18.3 |
| `tenant` | 租户 ID |
| `iat`、`exp` | NumericDate；`exp - iat` SHOULD ≤ 900 秒，MUST ≤ 3600 秒 |
| `jti` | 唯一 ID |
| `cnf.jkt` | 设备公钥的 RFC 7638 指纹 |

网关验证访问令牌时，除 §5.2 的步骤外，MUST 检查：`iss` 等于自身 issuer；`aud` 等于 `<iss>/tenants/<tenant>`；`exp + 30 ≥ now`；`iat - 30 ≤ now`；`sub` 对应的设备存在、状态为 `active`、属于 `tenant`；`cnf.jkt` 等于该设备公钥的指纹。

### 5.4 证明 JWT（`typ: lsp2-proof+jwt`）
载荷：

| 声明 | 要求 |
|---|---|
| `iss` | `device_id`。在配对阶段还没有 `device_id`，此时 MUST 为空字符串 `""` |
| `aud` | 用途：`lsp2:token`、`lsp2:hello` 或 `lsp2:session.token` |
| `iat` | 签名时刻 |
| `jti` | 唯一 ID |
| `ath` | `base64url(SHA-256(UTF-8(凭据)))`。被绑定的凭据：`lsp2:token` 绑定 `device_code`；`lsp2:hello` 绑定当前连接使用的访问令牌；`lsp2:session.token` 绑定新的访问令牌 |

验证方 MUST 检查：签名使用该设备登记的公钥；`aud`、`iss`、`ath` 都符合预期；`|now - iat| ≤ 60`；`jti` 在 120 秒内没有出现过。重复的 `jti` MUST 拒绝，这一点在网关多实例部署时同样 MUST 成立。

### 5.5 客户端断言（RFC 7523）
设备刷新令牌时提交 JWT：`typ: JWT`，`iss = sub = device_id`，`aud = <iss> + "/v2/token"`（RFC 7523 §3：令牌端点地址），`exp - iat ≤ 300`，`jti` 唯一。网关 MUST 用该设备登记的公钥验证签名；在 `exp + 60` 秒之前，同一 `jti` 只能使用一次。

### 5.6 任务签名（`typ: lsp2-task+jws`）
载荷（[`task.run.payload.json`](../spec/schemas/task.run.payload.json)）：

| 字段 | 要求 |
|---|---|
| `task_id` | MUST 等于信封中的 `task_id`（防止把签名挪到别的任务上） |
| `iat`、`exp` | `exp - iat` SHOULD ≤ 60 秒。设备 MUST 拒绝 `now > exp` 的任务 |
| `nonce` | ≥ 16 字符。在 `exp + 60` 秒之前，设备 MUST 拒绝重复的 `nonce` |
| `device_id` | MUST 等于本设备的 ID |
| `model` | 本设备在 `hello`/`models.update` 中声明过的模型 ID（不带 `device_id/` 前缀） |
| `messages` | 1–200 条（使用 `e2ee` 时省略），`role ∈ {system, user, assistant, tool, developer}`；`content` 为字符串、内容片段数组（§16.2）或 `null`；可带 `tool_calls`、`tool_call_id` |
| `tools`、`tool_choice` | OPTIONAL，见 §16.1 |
| `blobs` | OPTIONAL，`[{id, size, sha256, media_type}]`，见 §16.3 |
| `e2ee` | OPTIONAL，`{blob}`，见 §17 |
| `max_tokens`、`temperature` | OPTIONAL |

设备 MUST 用配对时得到的网关公钥（或 JWKS 中 `kid` 对应的公钥）验证签名。验证失败时，MUST 回复 `task.error`（`BAD_SIGNATURE`），MUST NOT 调用模型。

---

## 6. HTTP 接口

完整定义见 [`spec/openapi.yaml`](../spec/openapi.yaml)。所有接口 MUST 通过 TLS 1.2 及以上版本提供；仅在开发环境的回环地址上可以例外。

### 6.1 发现
- 租户后端 SHOULD 在 `GET /.well-known/lsp-connector` 返回 `{gateway_url, tenant, link_url, protocol: "lsp2"}`，供设备只凭租户网址就能完成配对。该文档 MUST NOT 包含任何秘密。
- 网关 MUST 提供 `GET /.well-known/jwks.json`（`application/jwk-set+json`）。
- 网关 MUST 提供 `GET /.well-known/lsp2-gateway[?tenant=<id>]` 元数据文档（§18.2）；带 `tenant` 时还包含签名的 SaaS 身份（§19.1）。`gateway_url` MUST 是网关的 origin（不带路径）。

### 6.2 设备授权：`POST /v2/device/authorize`
- 请求 MUST 支持 `application/x-www-form-urlencoded`（RFC 8628 §3.1），MAY 同时接受 JSON。参数：`tenant`（必填）、`public_key`（必填，base64url 32 字节）、`device_name`（≤ 100 字符）。
- 响应字段：`device_code`、`user_code`、`expires_in`、`interval`（≥ 1）。响应头 MUST 带 `Cache-Control: no-store`。`verification_uri` 是 OPTIONAL：确认页由租户提供，地址通过发现文档的 `link_url` 告知设备，这是本协议对 RFC 8628 的唯一偏离。
- `user_code` MUST 为 `XXXX-XXXX` 格式，字母表 `BCDFGHJKLMNPQRSTVWXZ`（20 个辅音字母，约 34.6 比特）。比较时 MUST 忽略大小写和分隔符。
- `device_code` 是不透明的高熵字符串。网关 SHOULD 只保存其哈希。

### 6.3 令牌：`POST /v2/token`
请求为表单编码（同样 MAY 接受 JSON）。

**device_code 授权**：`grant_type=urn:ietf:params:oauth:grant-type:device_code`、`device_code`、`proof`（证明 JWT，`aud=lsp2:token`，`iss=""`，`ath` 绑定 `device_code`，用配对时提交的公钥签名）。网关 MUST 按以下顺序处理：
1. `device_code` 未知 → `invalid_grant`；
2. 证明无效或 `jti` 重复 → `invalid_grant`（**先验证证明，再透露授权状态**）；
3. 已过期 → `expired_token`；已拒绝 → `access_denied`；已使用过 → `invalid_grant`；
4. 尚未确认：轮询间隔小于 `interval` 时返回 `slow_down`，并把 `interval` 加 5 秒；否则返回 `authorization_pending`；
5. 已确认：把 `device_code` 标记为已使用（一次性），返回令牌。

**jwt-bearer 授权**：`grant_type=urn:ietf:params:oauth:grant-type:jwt-bearer`、`assertion`（§5.5）。设备不存在或已吊销时返回 `invalid_grant`。设备收到 `invalid_grant` 后 MUST 停止重试，提示用户重新配对。

**成功响应**：`{access_token, token_type: "Bearer", expires_in, device_id, gateway_key: <JWK>}`，响应头带 `Cache-Control: no-store`。错误响应为 RFC 6749 §5.2 格式的 `{"error": "..."}`，HTTP 状态码 400。

### 6.4 租户管理接口
认证：`Authorization: Bearer <租户 API Key>`，加上 `X-End-User: <终端用户 ID>`（1–128 字符）。租户 API Key MUST 只在服务器之间使用，MUST NOT 出现在浏览器或设备上。网关 MUST 只存储 Key 的哈希，比较时 SHOULD 使用常数时间比较。

| 接口 | 作用 |
|---|---|
| `POST /v2/tenant/device-approvals` `{user_code, jkt?, deny?}` | 确认或拒绝配对码；设备归属于 `X-End-User`。带 `jkt` 时与设备公钥指纹不符 MUST 返回 409（§17.2） |
| `GET /v2/tenant/devices` | 列出该终端用户的设备：`created_at`（RFC 3339）、`public_key`、`jkt`、`e2ee_key`、在线时还有 `model_capabilities`、`capabilities`、`max_concurrency` |
| `GET /v2/tenant/tasks/{task_id}/events` | 按 `Last-Event-ID` 重新接入任务的 SSE 流（§14.4） |
| `POST /v2/tenant/devices/{device_id}/revoke` | 吊销设备，网关 MUST 立即关闭该设备的在线会话 |

- 错误响应 MUST 使用 RFC 9457（`application/problem+json`），含 `type`、`title`、`status`、`detail`，以及扩展成员 `code`（§9 的错误码）。`type` 为 `https://localsubproxy.dev/problems/<code 的小写连字符形式>`。
- 访问不属于该租户或该终端用户的设备，MUST 返回 404（不透露设备是否存在）。
- 同一租户每分钟确认失败超过 10 次时，网关 MUST 返回 429（`RATE_LIMITED`），用于防止暴力猜测配对码。

### 6.5 OpenAI 兼容接口
认证方式同 §6.4。错误使用 OpenAI 错误对象：`{"error":{"message","type":"lsp2_error","code"}}`。
- `GET /v1/models`：只列出该终端用户**在线**设备上的模型，`id` 格式为 `<device_id>/<model>`。
- `POST /v1/chat/completions`：不含 data: URI 时请求体 SHOULD ≤ 1 MiB，含图片/音频时 ≤ 48 MiB（§16.3）；不认识的 OpenAI 字段 MUST 忽略。支持 `tools`/`tool_choice`（§16）、内容片段数组（§16.2）、`lsp2_e2ee`（§17），以及请求头 `Idempotency-Key`、`Last-Event-ID`（§14.4）。
  - `stream: false`：返回一个 `chat.completion` 对象。
  - `stream: true`：返回 `text/event-stream`。第一个事件 SHOULD 是 `delta: {role: "assistant", content: ""}`；每个 `task.delta` 对应一个 `chat.completion.chunk` 事件；结束时发送带 `finish_reason`（以及 `usage`，如有）的块，然后发送 `data: [DONE]`。中途出错时 MUST 发送 `data: {"error":{...}}` 后关闭连接，MUST NOT 再发送 `[DONE]`。网关 SHOULD 每 15 秒发送一次注释行 `: keep-alive`。
  - 响应头 `X-Task-Id` 给出 `task_id`。
  - 调用方断开 HTTP 连接时，网关 MUST 向设备发送 `task.cancel`（`reason: "client_disconnected"`）。

### 6.6 模型路由
`model` 为 `<device_id>/<model>` 时，路由到该设备（必须属于调用方）。`model` 不带设备前缀时，网关 MAY 选择调用方任意一台声明了该模型的在线设备。设备离线时返回 503（`DEVICE_OFFLINE`）；没有任何设备声明该模型时返回 404（`MODEL_NOT_FOUND`）；设备已达并发上限时返回 429（`DEVICE_BUSY`），并带 `Retry-After` 头。

### 6.7 Anthropic Messages 兼容接口
认证方式同 §6.4，另外接受 Anthropic 客户端使用的 `x-api-key: <tenant key>` 头（与 `Authorization: Bearer` 等价）；`anthropic-version` 头可有可无，网关不校验其取值。

- `POST /v1/messages`：请求体按下表翻译为 §6.5 的 chat 请求，然后走与 `/v1/chat/completions` 完全相同的路由（§6.6）、签名（§5.6）、blob（§16.3）、流控（§15）与续传（§14.4）逻辑，也支持 `Idempotency-Key`。`max_tokens`（正整数）与 `model` 为必填；`lsp2_e2ee` 不适用于本接口，MUST 以 400 拒绝。

| Anthropic 请求 | 翻译为 |
|---|---|
| `system`（字符串或 text 块数组） | 首条 `role: "system"` 消息（多个 text 块以 `\n\n` 连接） |
| `text` 块 | `{"type":"text"}` 片段 |
| `image` 块，`source.type: "base64"` | `image_url`，`url` 为 `data:<media_type>;base64,<data>`（大于 16 KiB 时照常走 blob） |
| `image` 块，`source.type: "url"` | `image_url`，`url` 原样 |
| assistant 的 `tool_use` 块 | `tool_calls[]`：`{id, type:"function", function:{name, arguments: JSON(input)}}` |
| user 的 `tool_result` 块 | 单独一条 `role:"tool"` 消息，`tool_call_id = tool_use_id`；`content` 为字符串或 text 块拼接，`is_error: true` 时加前缀 `Error: ` |
| `thinking` / `redacted_thinking` 块 | 丢弃 |
| 其他块类型、服务端工具（`tools[].type` 不是 `custom`） | 400 |
| `tools[]: {name, description, input_schema}` | `{type:"function", function:{name, description, parameters: input_schema}}` |
| `tool_choice` `auto` / `any` / `none` / `{type:"tool",name}` | `"auto"` / `"required"` / `"none"` / `{type:"function",function:{name}}` |
| `max_tokens`、`temperature`、`stream` | 同名字段 |

- **非流式**响应：`{"id":"msg_<task>","type":"message","role":"assistant","model","content":[...],"stop_reason","stop_sequence":null,"usage":{"input_tokens","output_tokens"}}`。`content` 依次为一个 `text` 块（如有文本）和每个工具调用一个 `tool_use` 块（`input` 为解析后的参数对象，解析失败时为 `{}`）。`stop_reason` 由 `finish_reason` 映射：`stop`→`end_turn`、`length`→`max_tokens`、`tool_calls`→`tool_use`，其余→`end_turn`。
- **流式**（`stream: true`）响应为带事件名的 SSE：`message_start` → 每个内容块 `content_block_start` / 若干 `content_block_delta`（`text_delta` 或 `input_json_delta`）/ `content_block_stop` → `message_delta`（`delta.stop_reason`、`usage`）→ `message_stop`。文本与工具调用交替出现时，网关 MUST 先关闭当前块再开始新块，`index` 从 0 递增。保活使用 `event: ping`。中途出错时发送 `event: error`，`data` 为下述错误对象，然后关闭连接。
- **续传**：由同一个 `task.delta`/`task.done` 产生的一组 SSE 事件中，只有最后一个带 `id: <seq>`，因此 `Last-Event-ID` 总是指向一组已完整送达的事件。用同一个 `Idempotency-Key` 加 `Last-Event-ID` 重新请求时，网关 MUST 只发送该 id 之后的事件组（不重发 `message_start`）。
- **错误**使用 Anthropic 错误对象：`{"type":"error","error":{"type","message","lsp2_code"}}`，`lsp2_code` 为 §9 的错误码。`error.type` 由 HTTP 状态决定：400/422→`invalid_request_error`，401→`authentication_error`，403→`permission_error`，404→`not_found_error`，413→`request_too_large`，429→`rate_limit_error`，503→`overloaded_error`，其他 5xx→`api_error`。
- `GET /v1/models` 在请求带 `anthropic-version` 头时返回 Anthropic 列表形状：`{"data":[{"type":"model","id","display_name","created_at"}],"has_more":false,"first_id","last_id"}`；否则返回 §6.5 的 OpenAI 形状。

---

## 7. WebSocket 通道

完整定义见 [`spec/asyncapi.yaml`](../spec/asyncapi.yaml) 和 [`spec/schemas/envelope.json`](../spec/schemas/envelope.json)。

### 7.1 建立连接
- `GET /v2/connect`，请求头：`Authorization: Bearer <访问令牌>`、`Sec-WebSocket-Protocol: lsp2`。
- 网关在升级前 MUST 验证令牌（§5.3）。缺少子协议时返回 400；令牌无效时返回 401；设备已吊销或令牌未绑定该设备时返回 403。错误响应使用 RFC 9457 格式。
- 网关 MUST 在响应中选择子协议 `lsp2`；设备看到其他值 MUST 断开。
- 带 `Origin` 头的请求（来自浏览器）MUST 拒绝（403），以防跨站 WebSocket 劫持。
- 同一设备建立新会话时，网关 MUST 关闭该设备的旧会话。

### 7.2 帧
- 每条 WebSocket **文本**消息正好包含一个 JSON 对象。MUST NOT 发送二进制消息；收到二进制消息时 SHOULD 以关闭码 1003 断开。
- 单帧 MUST ≤ `welcome.max_frame_bytes`（参考实现为 262144 字节）。超限时接收方 MUST 以 1009 断开。

信封：

| 成员 | 类型 | 说明 |
|---|---|---|
| `v` | 整数 | MUST 为 `2` |
| `type` | 字符串 | 消息类型，格式 `^[a-z][a-z0-9._]{0,63}$` |
| `id` | 字符串 | 请求/响应关联 ID；`hello`、`session.token` 必填，响应 MUST 原样带回 |
| `task_id` | 字符串 | 任务相关消息必填 |
| `seq` | 整数 ≥ 1 | 数据帧（`task.run`、`blob.chunk`、`task.delta`、`task.done`、`task.error`）必填，每个任务、每个方向从 1 开始（§14、§15） |
| `data` | 对象 | 消息体，结构由 `type` 决定 |
| `jws` | 字符串 | 只用于 `task.run`；此时 MUST NOT 出现 `data` |

### 7.3 消息
| `type` | 方向 | `data` 模式 | 说明 |
|---|---|---|---|
| `hello` | 设备→网关 | `hello.json` | 必须是第一帧，含证明 JWT（`aud=lsp2:hello`） |
| `welcome` | 网关→设备 | `welcome.json` | 握手成功，`id` 与 `hello` 相同 |
| `task.run` | 网关→设备 | 无；`jws` 载荷符合 `task.run.payload.json` | 下发任务 |
| `task.delta` | 设备→网关 | `task.delta.json` | 增量文本 |
| `task.done` | 设备→网关 | `task.done.json` | 任务成功结束（终止消息） |
| `task.error` | 设备→网关 | `task.error.json` | 任务失败结束（终止消息） |
| `task.cancel` | 网关→设备 | `task.cancel.json` | 要求停止任务 |
| `session.token` | 设备→网关；网关以同名消息回复 | 请求 `session.token.request.json`，回复 `session.token.response.json` | 连接内续期令牌 |
| `models.update` | 设备→网关 | `models.update.json` | 重新声明模型列表和并发数 |
| `ping` / `pong` | 双向 | 无 | 应用层心跳；`pong` 带回 `ping` 的 `id` |
| `blob.chunk` | 网关→设备 | `blob.chunk.json` | 数据帧：二进制附件分块（§16.3） |
| `task.ack` | 双向 | `task.ack.json` | 控制帧：确认对方 `seq ≤ data.seq` 的数据帧（§14、§15） |
| `error` | 双向 | `error.json` | 连接级错误 |

### 7.4 握手
1. 连接建立后，设备 MUST 在 15 秒内发送 `hello`。超时，或第一帧不是 `hello`，网关 MUST 断开连接。
2. 网关检查：`v == 2`（否则回复 `error` `VERSION_UNSUPPORTED` 并断开）；`data.device_id == token.sub`；证明 JWT 有效（`ath` 绑定**本次连接使用的访问令牌**），且 `jti` 未用过。失败时回复 `error`（`UNAUTHENTICATED`）并断开。
3. `max_concurrency` 取值 1–16，超出范围时网关 MUST 截断到边界值。
4. 网关回复 `welcome`：`session_id`、`heartbeat_seconds`、`max_frame_bytes`、`token_expires_at`、`capabilities`（§11）。

### 7.5 任务与顺序
- 网关收到 OpenAI 请求后，创建 `task_id`，发送 `task.run`。
- 设备收到 `task.run` 后，依次检查签名（§5.6）、`device_id`、`exp`、`nonce`、模型是否在本机允许列表中、并发数是否已满，任一不满足即回复 `task.error`：签名错误为 `BAD_SIGNATURE`；不属于本设备、已过期或重放为 `FORBIDDEN` 或 `BAD_REQUEST`；并发已满为 `DEVICE_BUSY`。拒绝时 MUST NOT 调用模型。
- 同一 `task_id` 的设备回报帧中，`seq` MUST 从 1 开始、严格加 1；终止消息（`task.done` 或 `task.error`）的 `seq` 是最后一个。终止消息之后 MUST NOT 再发送该任务的帧。
- 网关发现 `seq` 不连续时，MUST 让该任务失败（`INTERNAL`，HTTP 502），并向设备发送 `task.cancel`（`reason: "protocol_error"`）。
- 未协商 `resume` 时，连接断开即让所有进行中的任务失败：`task.run` 尚未发出的任务为 `DEVICE_OFFLINE`（`retryable: true`），已发出的为 `OUTCOME_UNKNOWN`（不可自动重试，模型可能已执行）。协商了 `resume` 时按 §14 处理。

### 7.6 取消
- 网关在以下情况发送 `task.cancel`：调用方断开连接（`client_disconnected`）、任务超时（`timeout`）、协议错误（`protocol_error`）。
- 设备收到后 SHOULD 尽快中止模型调用，并发送 `task.error`（`CANCELLED`）作为终止消息。如果任务已经结束，或者 `task_id` 未知，MUST 忽略这条取消。
- 发出取消后，该任务后续到达的帧，网关 MUST 静默丢弃，MUST NOT 把它们当作协议错误。

### 7.7 心跳与续期
- 网关每 `heartbeat_seconds` 发送一次 WebSocket 控制帧 ping，设备 MUST 回复 pong（RFC 6455 §5.5.2）。60 秒内没有收到任何数据，任一方都 MAY 断开。应用层 `ping` 帧 OPTIONAL。
- 设备 SHOULD 在 `max(token_expires_at - 60, 剩余有效期的一半)` 时刻用 jwt-bearer 授权取得新令牌，然后发送 `session.token`，`data` 为 `{access_token, proof}`，其中证明的 `aud=lsp2:session.token`，`ath` 绑定**新**令牌。
- 网关按握手的标准验证新令牌（`sub` 必须相同，设备必须仍为 `active`）。通过后回复 `session.token`，`data` 为 `{token_expires_at}`，`id` 与请求相同；失败时回复 `error`（`UNAUTHENTICATED`）并断开。
- 当前令牌过期超过 30 秒仍未续期时，网关 MUST 断开会话。

---

## 8. 状态机

### 8.1 配对（网关侧，每个 device_code）
```
pending ──确认──▶ approved ──令牌已签发──▶ consumed
   │  └──拒绝──▶ denied
   └──超过 expires_in──▶ expired
```

### 8.2 设备记录
`active ──吊销──▶ revoked`（终态）。被吊销的设备 MUST 立即被断开，并且 MUST NOT 再取得令牌。

### 8.3 会话（网关侧）
```
UPGRADED ──hello 有效──▶ OPEN ──关闭/吊销/令牌过期/错误──▶ CLOSED
    └──15 秒超时或 hello 无效──▶ CLOSED
OPEN 状态下：session.token 只延长令牌有效期，不改变状态
```

### 8.4 任务（网关侧）
```
RUNNING ──task.done──▶ SUCCEEDED
   ├──task.error(CANCELLED) 或调用方断开──▶ CANCELLED
   └──task.error / 超时 / 断线 / seq 不连续──▶ FAILED
```
设备侧：`RECEIVED → (验证失败 → REJECTED) | EXECUTING → DONE | ERROR | CANCELLED`。

### 8.5 设备连接（设备侧）
`IDLE → TOKEN → CONNECTING → HANDSHAKE → OPEN`。断开后按指数退避重连：1 秒起，翻倍，上限 30 秒，SHOULD 加随机抖动。收到 `invalid_grant` 时进入 `REVOKED`，停止重连。

---

## 9. 错误

`error` 和 `task.error` 的 `data` 为 `{code, message, retryable?}`。`code` 格式为 `^[A-Z][A-Z0-9_]{1,63}$`。对于不认识的 `code`，接收方 MUST 按不可重试处理，除非 `retryable` 为 `true`。

| code | 含义 | 建议 HTTP 状态码 | 默认可重试 |
|---|---|---|---|
| `UNAUTHENTICATED` | 令牌或证明无效 | 401 | 否 |
| `FORBIDDEN` | 无权访问，或被本机策略拒绝 | 403（资源归属问题用 404） | 否 |
| `BAD_REQUEST` | 消息格式或参数错误 | 400 | 否 |
| `BAD_SIGNATURE` | `task.run` 签名无效 | 502 | 否 |
| `VERSION_UNSUPPORTED` | 协议版本不支持 | 400 | 否 |
| `DEVICE_OFFLINE` | 设备不在线或中途断开 | 503 | 是 |
| `DEVICE_BUSY` | 设备已达并发上限 | 429 | 是 |
| `MODEL_NOT_FOUND` | 模型不存在 | 404 | 否 |
| `UPSTREAM_ERROR` | 本机模型出错 | 502 | 视情况 |
| `TIMEOUT` | 任务超时 | 504 | 是 |
| `CANCELLED` | 任务已取消 | —（流已关闭） | 否 |
| `RATE_LIMITED` | 请求过于频繁 | 429 | 是 |
| `INTERNAL` | 网关内部错误或协议违规 | 500 / 502 | 视情况 |
| `OUTCOME_UNKNOWN` | 任务已下发但结果未知（断线未续传） | 502 | 否 |
| `FLOW_CONTROL` | 超出对方通告的窗口（§15） | —（断开连接） | 否 |
| `SLOW_CONSUMER` | SSE 调用方长时间不读取（§15.4） | —（流已关闭） | 否 |
| `CAPABILITY_UNSUPPORTED` | 模型或设备不支持请求的能力 | 400 | 否 |
| `PAYLOAD_TOO_LARGE` | 附件超出限制 | 413 | 否 |
| `IDEMPOTENCY_KEY_REUSED` | 同一 `Idempotency-Key` 用于不同请求体 | 422 | 否 |
| `E2EE_DECRYPT_FAILED` | 设备无法解密或校验密封请求 | 400 / 502 | 否 |
| `CONSENT_DENIED` | 配对未获用户同意：本机用户在同意页拒绝，或 SaaS 侧用户拒绝（OAuth `access_denied` 映射为此码），§19.3 | 403 | 否 |

OAuth 接口的错误值沿用 RFC 6749/8628：`invalid_request`、`invalid_grant`、`unsupported_grant_type`、`authorization_pending`、`slow_down`、`access_denied`、`expired_token`、`server_error`。

---

## 10. 超时与限制

| 项目 | 值 | 级别 |
|---|---|---|
| hello 截止时间 | 连接后 15 秒 | MUST |
| 读超时（无任何数据） | 60 秒 | SHOULD |
| 心跳间隔 | 20 秒（以 `welcome.heartbeat_seconds` 为准） | SHOULD |
| 单帧大小 | ≤ 256 KiB（以 `welcome.max_frame_bytes` 为准，且 ≥ 64 KiB） | MUST |
| 访问令牌 TTL | ≤ 15 分钟（推荐），≤ 60 分钟（上限） | SHOULD / MUST |
| 令牌过期后的宽限 | 30 秒 | MUST |
| 证明 JWT 时钟偏差 | ±60 秒 | MUST |
| 断言有效期 | ≤ 300 秒 | MUST |
| device_code 有效期 | 10 分钟（推荐） | SHOULD |
| 初始轮询间隔 | ≥ 1 秒；每次 `slow_down` 加 5 秒 | MUST |
| `task.run` 有效期（exp - iat） | ≤ 60 秒 | SHOULD |
| 任务总时长 | 10 分钟（推荐） | SHOULD |
| `max_concurrency` | 1–16 | MUST |
| 模型列表 | ≤ 500 项 | MUST |
| `messages` | 1–200 条 | MUST |
| chat 请求体 | ≤ 1 MiB | SHOULD |

| 续传窗口 | 30 秒（`welcome.resume_window_seconds`） | SHOULD |
| 流控窗口默认值 | 每任务 1 MiB、每连接 4 MiB；MUST ≥ 64 KiB | SHOULD / MUST |
| 单个附件 / 单任务附件 | ≤ 20 MiB / ≤ 32 MiB | MUST |
| 内联 data: URI 阈值 | 16 KiB，超过即转为 blob | SHOULD |
| E2EE 请求有效期 | `iat` 起 300 秒 | MUST |
| 并发排队 | 设备槽位满时 FIFO 等待 ≤ 15 秒，然后 429 | SHOULD |

**背压**：网关 MUST NOT 为单个任务无限缓冲数据，背压由 §15 的窗口机制实现。

---

## 11. 版本与能力协商

- **主版本**：由 WebSocket 子协议名（`lsp2`）和信封的 `v` 表示。不兼容的变更 MUST 使用新的子协议名（`lsp3`）。设备 MAY 同时提供多个子协议，网关从中选择一个。
- **兼容扩展**（新增可选成员、新增消息类型、新增错误码）不改变主版本。接收方 MUST 忽略不认识的成员。收到不认识的 `type` 时，网关 MUST 回复 `error`（`BAD_REQUEST`）且 MUST NOT 断开连接；设备 MUST 忽略不认识的 `type`。
- **能力**：`hello.capabilities` 列出设备支持的能力，`welcome.capabilities` MUST 是它与网关所支持能力的交集。双方 MUST 只使用交集中的能力。本版本定义的能力：

| 能力 | 含义 |
|---|---|
| `session.token` | 连接内续期令牌（§7.7） |
| `models.update` | 连接内更新模型列表 |
| `resume` | 断线续传（§14） |
| `flow` | 字节窗口流量控制与 `task.ack`（§15） |
| `tools` | function calling（§16.1） |
| `multimodal` | 内容片段与 `blob.chunk`（§16.2–§16.3） |
| `e2ee` | 端到端加密（§17） |

- 厂商私有的能力名和消息类型 SHOULD 以 `x-` 开头（消息类型用 `x.`），以免与将来的标准名冲突。

---

## 12. 安全要求

1. **传输**：生产环境 MUST 使用 TLS（`https`/`wss`）。设备 MUST 验证证书。
2. **最小凭据**：设备只持有自己的私钥。访问令牌短期有效，并与私钥绑定；令牌泄露后，没有私钥也无法建立会话或续期。
3. **网关被冒充**：设备 MUST 只执行用固定的网关公钥（配对时得到，或来自 TLS 保护的 JWKS）签名的 `task.run`。
4. **重放**：证明 JWT 的 `jti`、断言的 `jti`、`task.run` 的 `nonce` 都 MUST 做去重；`device_code` 只能使用一次。
5. **租户隔离**：每个查询 MUST 同时按 `tenant` 和 `X-End-User` 过滤。访问别人的资源返回 404。
6. **配对码**：只有已登录的终端用户才能通过租户后端确认配对码；网关 MUST NOT 提供匿名查询配对码的接口。确认失败 MUST 限速（§6.4）。
7. **本机策略**：设备 MUST 只执行用户允许的模型，并 MUST 执行本地并发上限。
8. **静态数据**：网关 SHOULD 加密存储提示词和输出（参考实现使用 AES-256-GCM），并按保留期清理；`device_code` 和租户 Key 只保存哈希。
9. **日志**：MUST NOT 记录访问令牌、证明、断言、`device_code`、租户 Key 或提示词原文。
10. **浏览器**：`/v2/connect` 拒绝带 `Origin` 的请求；租户 Key 不得出现在前端代码中。

---

## 13. 一致性等级与测试

| 等级 | 要求 |
|---|---|
| **lsp2-core 网关** | §4–§10 中所有 MUST；能通过 `lsp2-conformance` 的全部检查 |
| **lsp2-core 设备** | 能验证 `spec/testvectors` 中所有正例、拒绝所有反例；实现 §7.4–§7.7 |
| **lsp2-openai** | §6.5 的 OpenAI 兼容接口与 SSE 行为 |
| **lsp2-anthropic** | §6.7 的 Anthropic Messages 兼容接口（`lsp2-conformance` 的 A1–A10） |

验证方法：
```bash
# 1) 你的 JOSE 实现对照测试向量（Go/Python/Node/Rust 示例见 spec/examples）
spec/validate.sh
# 2) 黑盒测试你的网关
LSP2_TENANT_KEY=<租户 key> go run ./cmd/lsp2-conformance \
  -gateway https://your-gateway -tenant <tenant> -schemas spec/schemas \
  [-issuer https://public-origin]   # 网关位于反向代理后、访问地址与 issuer 不同时
```
`lsp2-conformance` 依次扮演设备和租户后端，共 63 项检查（含 X1、X-* 扩展检查，以及 M1–M5、I1–I3 身份绑定检查），覆盖：JWKS、RFC 8628 流程与错误顺序、令牌签名与 `cnf.jkt`、一次性 `device_code`、断言防重放、子协议与令牌校验、hello 证明绑定、能力交集、未知消息类型、模型列表、`task.run` 的 JWS 校验、SSE 输出、取消、会话续期和吊销。它还会用 JSON Schema 校验网关发出的每一帧。

---

## 14. 断线续传（能力 `resume`）

### 14.1 数据帧与确认
- 数据帧：`task.run`、`blob.chunk`（网关→设备）、`task.delta`、`task.done`、`task.error`（设备→网关）。其余为控制帧，不带 `seq`、不重传。
- 发送方对每个任务的数据帧按 1、2、3… 编号，保留尚未被确认的帧。接收方用 `task.ack {seq}` 确认“该任务 `seq` 及以前的所有帧”（累计确认）。
- 接收方 MUST 丢弃 `seq ≤ 已收最大连续 seq` 的帧（重复）；遇到跳号 MUST 让任务失败（§7.5），MUST NOT 静默接受。

### 14.2 握手
1. `welcome` 带 `resume_token`（不透明，≥ 128 比特）和 `resume_window_seconds`。
2. 断线后，设备在窗口内重连，`hello.resume = {token, tasks:[{task_id, recv_seq}]}`，`recv_seq` 是该任务已从网关收到的最大连续 `seq`。仍须提供新的证明 JWT（§7.4），令牌规则不变。
3. 网关验证 token 属于该设备且未过期后，回复 `welcome.resume = {resumed: true, tasks:[{task_id, recv_seq}]}`，`recv_seq` 是网关已收到的设备帧。新的 `welcome` 同时给出新的 `resume_token`（旧 token 作废）。
4. 双方随后 MUST 按顺序重传对方 `recv_seq` 之后的所有未确认帧，然后才发送新帧（重传帧不得被新帧超越）。
5. 设备未列出的进行中任务，网关 MUST 以 `OUTCOME_UNKNOWN` 结束；token 无效或过期时 `resumed: false`，所有旧任务按 §7.5 失败。

### 14.3 网关侧任务保留
设备断开后，网关在 `resume_window_seconds` 内保留任务和未确认帧，调用方的 SSE 连接保持打开（发送 keep-alive）。窗口到期仍未续传，任务按 §7.5 失败。

### 14.4 HTTP 侧续传
- 每个 SSE 事件 MUST 带 `id:`：角色前导事件为 `0`，其后为对应设备数据帧的 `seq`（单调递增）。
- 调用方 MAY 带 `Idempotency-Key`（1–255 字符）。同一租户、同一终端用户内，相同 key + 相同请求体（按 SHA-256 比较）MUST 接回已有任务而不是再次执行；请求体不同 MUST 返回 422 `IDEMPOTENCY_KEY_REUSED`。key 至少保留 24 小时。
- 重新接入时带 `Last-Event-ID: <n>`（POST 同一 key，或 `GET /v2/tenant/tasks/{id}/events`），网关 MUST 只发送 `id > n` 的事件；已结束的任务从存储重放。
- 带 key 的请求，调用方断开后网关 SHOULD 保留任务 30 秒再取消；不带 key 时按 §6.5 立即取消。

---

## 15. 流量控制（能力 `flow`）

### 15.1 窗口
`hello.flow`（设备授予网关）和 `welcome.flow`（网关授予设备）各为 `{task_window, conn_window}`，单位是**已发送、未确认数据帧的序列化字节数**（即 WebSocket 文本消息长度）。两值 MUST ≥ 64 KiB 且 ≥ `max_frame_bytes`；缺省为 1 MiB / 4 MiB。

### 15.2 接收方
- 接收方在交付（网关：写给 SSE 订阅者；设备：交给执行器/重组缓冲）后发送 `task.ack`。网关 MUST 以所有在线订阅者中最慢的位置作为确认位置——这是端到端背压的来源。
- 收到会使未确认字节超出所通告窗口的帧（且该窗口非空）时，接收方 MUST 发送 `error`（`FLOW_CONTROL`）并断开连接。

### 15.3 发送方
发送 n 字节的数据帧，当且仅当 `task_used + n ≤ task_window`（或 `task_used == 0`）**且** `conn_used + n ≤ conn_window`（或 `conn_used == 0`）。不满足时 MUST 等待确认；多个等待的任务 SHOULD 按 FIFO 获得窗口，以保证公平。窗口为空时总允许发送一帧，防止死锁。测试向量 `flow` 给出了发送与接收两侧的逐步判定。

### 15.4 慢消费者
网关 SHOULD 对长期不读取的 SSE 调用方设置上限（参考实现 60 秒），超时后以 `SLOW_CONSUMER` 结束任务并向设备发送 `task.cancel`。

---

## 16. tools 与多模态（能力 `tools`、`multimodal`）

### 16.1 tools
- `hello`/`models.update` 的 `model_capabilities` 为每个模型声明 `tools`、`vision`、`audio` 中的若干项。`GET /v1/models` MUST 返回每个模型的 `capabilities`。
- 请求带 `tools` 而目标模型未声明 `tools`、或设备未协商 `tools` 能力时，网关 MUST 返回 400 `CAPABILITY_UNSUPPORTED`，MUST NOT 下发任务。
- `tools`、`tool_choice` 原样放入签名的 `task.run`。设备在 `task.delta.tool_calls` 中按 OpenAI 流式格式回报（`index` 区分调用，`arguments` 可分片），以 `finish_reason: "tool_calls"` 结束。网关 MUST 透传为 SSE `delta.tool_calls`；非流式响应 MUST 按 `index` 合并成 `message.tool_calls`。
- 后续请求中的 `role: "assistant"`+`tool_calls` 和 `role: "tool"`+`tool_call_id` 消息 MUST 原样转给设备。
- 适配器映射（参考实现）：OpenAI 兼容直通；Anthropic 映射为 `tool_use`/`tool_result` 内容块与 `input_schema`；Ollama 映射为 `tools` 与 `message.tool_calls`（arguments 为对象）；Codex 订阅不支持（不声明能力）。

### 16.2 内容片段
`content` 可为片段数组：`{type:"text",text}`、`{type:"image_url",image_url:{url,detail?}}`（需 `vision`）、`{type:"input_audio",input_audio:{data,format}}`（需 `audio`）。缺少能力时 400 `CAPABILITY_UNSUPPORTED`。`image_url.url` 允许 `https://`（原样转发，由设备自行获取）与 `data:`；`http://` MUST 拒绝。

### 16.3 blob
- 大于 16 KiB 的 data: URI 和音频数据，网关 MUST 移入 blob：`task.run.blobs` 列出 `{id, size, sha256(base64url), media_type}`，片段中改写为 `lsp2-blob:<id>`。
- `task.run`（seq 1）之后，网关按偏移顺序发送 `blob.chunk {blob_id, offset, data(base64url, ≤ 96 KiB 原始字节), last}` 数据帧，它们同样受 §15 窗口约束。
- 设备 MUST 在全部 blob 到齐且 `size`、`sha256` 与签名载荷一致后才执行；不一致时 `task.error`（`BAD_REQUEST`）。单 blob ≤ 20 MiB，单任务 ≤ 32 MiB，超出时网关返回 413 `PAYLOAD_TOO_LARGE`。

---

## 17. 端到端加密（能力 `e2ee`）

目标：租户后端与设备之间的提示词和输出对网关不可见（网关只看到路由信息、用量和结束原因）。

### 17.1 设备密钥声明
设备生成 X25519 密钥对（RFC 7748），用身份私钥签发 JWS（`typ: lsp2-e2ee-key+jwt`，`kid` 为设备公钥指纹），载荷 `{iss, sub: device_id, iat, exp, jwk:{kty:"OKP", crv:"X25519", x, kid:<RFC 7638 指纹>, use:"enc", alg:"ECDH-ES"}}`，`exp - iat` ≤ 30 天。声明放在 `hello.e2ee_key`（或 `models.update.e2ee_key`），网关存储并通过 `GET /v2/tenant/devices` 原样提供。

### 17.2 信任锚
网关可能被攻破，因此租户 MUST NOT 只凭网关返回的公钥信任设备。设备在本机显示/在 `verification_uri_complete` 中携带其身份公钥指纹 `jkt`；用户确认时由租户后端记录（钉住）该 `jkt` 并随 `device-approvals` 提交（不符时网关返回 409）。之后租户 MUST 用钉住的 `jkt` 校验 `public_key` 与密钥声明签名，并检查 `sub`、`exp` 与 X25519 `kid`。

### 17.3 密封请求
租户构造 `{rid, iat, model, messages, tools?, tool_choice?, max_tokens?, temperature?, response_key}`（`response_key` 为新随机 32 字节，`rid` 唯一），按 RFC 7516 JWE 紧凑序列化加密：`alg: "ECDH-ES"`（直接密钥协商，RFC 7518 §4.6，X25519 按 RFC 8037）、`enc: "A256GCM"`、`kid`、`cty: "lsp2-request+json"`；encrypted key 段为空；`zip`、`crit` MUST NOT 出现。通过 `lsp2_e2ee` 字段提交，此时 MUST NOT 带明文 `messages`/`tools`（否则 400）。

### 17.4 网关与设备处理
- 网关把 JWE 作为 `media_type: application/jose` 的 blob 下发，`task.run.e2ee = {blob}`，`task.run.model` 为路由模型，不含明文消息。网关 MUST NOT 尝试解密，日志和存储中只出现密文。
- 设备 MUST：拒绝 `alg`/`enc` 以外的取值、`zip`；用 X25519 私钥解密；检查 `now - iat ≤ 300`、`rid` 未在窗口内出现过、密封的 `model` 等于 `task.run.model`。任一失败回复 `task.error`（`E2EE_DECRYPT_FAILED`，消息不得包含明文）。

### 17.5 密封输出
- 每个结果帧用 `response_key` 以 AES-256-GCM 加密：nonce = 4 字节 0 ‖ BE64(seq)，AAD = UTF-8 `"lsp2-e2ee/v1|" + rid + "|" + type + "|" + seq`。`task.delta.data = {e2ee: base64url(密文‖tag)}`，明文为 `{text?, tool_calls?}`，MUST NOT 同时带明文字段；`task.done` 保留明文 `finish_reason`、`usage`（计费需要），另带密封的 `{finish_reason}`。
- 网关的 SSE 块带 `lsp2_e2ee: {seq, type, data}`；租户用 `response_key` 解开。由于 `seq`、`type`、`rid` 都绑定在 AAD 中，网关无法重排、跨请求拼接或伪造帧。

### 17.6 威胁模型
| 威胁 | 结果 |
|---|---|
| 网关读取提示词/输出 | 防御：只有密文 |
| 网关替换设备公钥 | 防御：租户用配对时钉住的 jkt 校验 |
| 网关重放请求 | 防御：`iat` 300 秒 + `rid` 去重 |
| 网关重排/删改输出帧 | 防御：AAD 绑定 seq/type/rid；删尾可被察觉（缺少密封 `task.done`） |
| 网关拒绝服务、观察元数据（模型名、时间、长度、用量） | 不防御 |
| 设备或租户本身被攻破 | 不在范围 |
不提供前向保密之外的保证：X25519 设备密钥泄露会暴露用该密钥加密的历史请求，设备 SHOULD 定期轮换（`models.update.e2ee_key`）。

---

## 18. 网关身份绑定

### 18.1 issuer
- 每个网关 MUST 配置唯一的 **issuer**：它对外的 https origin（`scheme://host[:port]`，小写，无路径、查询、userinfo，默认端口省略）。仅开发环境的回环地址允许 `http`。
- issuer 是访问令牌的 `iss`、所有受众的基准，也是元数据文档的 `issuer`。参考实现通过 `server -issuer https://…` 配置；未配置时仅在开发模式下按请求 Host 推导，生产环境 MUST NOT 依赖推导。

### 18.2 元数据：`GET /.well-known/lsp2-gateway`
参照 RFC 8414（授权服务器元数据）。响应 `application/json`，可缓存（SHOULD ≤ 300 秒）：

| 字段 | 要求 |
|---|---|
| `issuer` | REQUIRED，§18.1 |
| `jwks_uri` | REQUIRED，MUST 与 issuer 同源 |
| `device_authorization_endpoint`、`token_endpoint` | REQUIRED，同源 |
| `connect_endpoint` | REQUIRED，`wss://<host>/v2/connect`（开发环境 `ws://`） |
| `chat_completions_endpoint` | OPTIONAL |
| `protocol_versions` | REQUIRED，如 `["2"]` |
| `capabilities` | REQUIRED，网关支持的扩展 |
| `scopes_supported` | REQUIRED，§19.2 的作用域 |
| `tenant`、`audience`、`saas`、`requested_models`、`requested_scopes`、`iat`、`exp`、`signed_metadata` | 仅 `?tenant=` 时出现，§19.1 |

未知租户 MUST 返回 404。

### 18.3 令牌受众
- 访问令牌：`iss = <issuer>`，`aud = <issuer>/tenants/<tenant>`，`tenant` 声明同值。为租户 A 签发的令牌在租户 B 或其他网关处都无效。
- 设备断言（§5.5）：`aud = <issuer>/v2/token`。网关 MUST 拒绝其他 `aud`（`invalid_grant`）。
- 令牌响应 SHOULD 带 `issuer` 字段，设备 MUST 校验它与元数据一致。

### 18.4 设备侧校验
设备在配对时记录 `issuer` 和 `audience`，并作为身份的一部分持久化。之后每次取得访问令牌，设备 MUST 用固定的网关公钥验证签名，并检查 `iss`、`aud`、`tenant`、`sub = device_id`、`cnf.jkt = 自身公钥指纹`。任何一项不符 MUST 拒绝该令牌并停止重连（参考实现返回 `ErrGatewayIdentity`），不得退而使用该令牌。

## 19. 配对同意

### 19.1 签名元数据
`GET /.well-known/lsp2-gateway?tenant=<id>` 返回 §18.2 的字段，外加：

| 字段 | 说明 |
|---|---|
| `tenant`、`audience` | 租户 ID 与 `<issuer>/tenants/<tenant>` |
| `saas` | `{name (REQUIRED), logo_uri, homepage_uri, policy_uri（隐私政策）, tos_uri, pairing_uri（用户确认配对码的页面）}` |
| `requested_models` | SaaS 打算使用的模型（可为空：由本机配置决定） |
| `requested_scopes` | §19.2 中的作用域，至少一个 |
| `iat`、`exp` | `exp - iat` MUST ≤ 24 小时 |
| `signed_metadata` | 上述全部字段（不含自身）的紧凑 JWS，`typ: lsp2-metadata+jwt`，`alg: EdDSA`，`kid` = 网关签名密钥指纹，密钥 MUST 在 `jwks_uri` 中 |

与 RFC 8414 §2.1 一致：设备 MUST 只使用 `signed_metadata` 内的值。

设备 MUST：从期望的 issuer（深链接或发现文档给出的网关 origin）读取元数据；`jwks_uri` 同源；用 JWKS 中 `kid` 对应的 Ed25519 密钥验证签名；`issuer`、`tenant`、`audience` 与期望一致；`saas.name` 非空；未过期；各端点同源；`pairing_uri` 为 https（开发环境回环除外）。任何一步失败 MUST NOT 显示同意页。配对完成后，令牌响应中的 `gateway_key` MUST 等于签名元数据的密钥，否则中止。

### 19.2 作用域注册表
| scope | 含义 |
|---|---|
| `lsp2.chat` | 文本对话（默认） |
| `lsp2.tools` | 函数调用（§16.1） |
| `lsp2.vision` | 图片输入 |
| `lsp2.audio` | 音频输入 |

作用域用于同意页展示；本机的策略引擎仍以本地配置为准。

### 19.3 同意流程
1. 设备通过深链接或 SaaS 地址得到 `(issuer, tenant)`，按 §19.1 校验签名元数据。
2. 设备 MUST 展示：SaaS 名称、logo、主页、隐私政策、网关 origin、租户、请求的模型与作用域、网关密钥指纹。
3. 用户 MUST 显式点击“同意”或“拒绝”。没有默认同意；超时视为拒绝。
4. 拒绝：设备不向网关发送任何请求，本地 API 返回 `CONSENT_DENIED`。
5. 同意：设备才生成设备密钥并调用 `device_authorize`（§6.2），在本机显示 `user_code`，并打开 `pairing_uri?user_code=…&jkt=…`。
6. SaaS 侧用户拒绝时，令牌端点返回 `access_denied`，设备将其映射为 `CONSENT_DENIED`。
7. 被拒绝（`deny: true`）的 `user_code` 在过期前不可再被批准：网关对再次提交的 `/v2/tenant/device-approvals` 返回 403 Problem Details，`code` 为 `CONSENT_DENIED`。

只用于自动化（测试、脚本、`connector -yes`）的实现 MAY 跳过第 2–3 步，但交互式应用 MUST NOT。

### 19.4 深链接 `lsp2://pair`
```
lsp2://pair?v=2&gateway=<https issuer origin>&tenant=<tenant id>[&remote=<SaaS origin>]
```
- `v` REQUIRED，当前为 `2`；`gateway` REQUIRED，按 §18.1 规范化；`tenant` 匹配 `^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`；参数不得重复。
- 链接 MUST NOT 携带秘密或展示信息；展示的一切都来自签名元数据，因此伪造的链接最多只能把用户引到另一个真实的、已签名的身份上。
- 桌面应用 SHOULD 注册 `lsp2` URL scheme；不能注册时，用户可以粘贴链接。

## 20. 本机审计日志与用量统计（桌面端，非线协议）

本节约束桌面实现，不影响互通。

- 每个执行完毕的任务记录一行：`connection_id`（device_id）、`gateway`（issuer）、`tenant`、`saas_name`、`task_id`、`model`、`adapter`、开始时间、`status`（`ok | error | cancelled`）、`error_code`、`prompt_tokens`、`completion_tokens`、`usage_reported`（运行时是否给出用量）、`latency_ms`、`ttft_ms`、`e2ee`、`tools`、图片数。
- MUST NOT 记录 prompt、输出、工具参数或附件内容。
- 统计：按 `saas`、`model`、`day`、`hour`、`status`、`connection` 任意组合聚合请求数、失败数、prompt/completion/total tokens、平均与最大延迟、未计量请求数；时间桶默认 UTC，可选本地时区。
- 保留期默认 90 天，MUST 定期清理；用户可以手动清理。
- 参考实现：`internal/audit`（SQLite `audit.db`）；本地 API `audit_list`、`usage_stats`、`audit_purge`；CLI `localsubproxy usage`、`localsubproxy audit`。

## 21. 尚未定义的内容

- 网关多实例部署时的会话路由和共享去重/续传存储（参考实现为单进程）；
- E2EE 的多设备/群组密钥与密钥透明日志；
- 网关签名密钥轮换（JWKS 中并存多把密钥的过渡期规则）；
- 网关侧的按租户用量计费接口；
- 按 SaaS 的本机策略（配额、时间窗）与 `QUOTA_EXCEEDED` / `POLICY_DENIED` 错误码。
