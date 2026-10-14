# Node.js LMBP 模拟网关

`gateway.mjs` 是LMBP（[docs/lmbp-spec.md](../../docs/lmbp-spec.md)）网关的 Node.js 命令行外壳：协议实现全部在服务端 SDK [`sdk/node`](../../sdk/node/README.md)（包名 `lmbp-server`）里，这里只解析参数、注册租户并调用 `new LMBPServer(...).listen()`。可替代 Go 参考网关给连接器、SaaS 后端或其他语言实现做互通测试。

## 运行

> 一键启动（假模型 + 网关 + 聊天页，并打印登录信息和配对命令）：`scripts/mock-start.sh node`，停止用 `scripts/mock-stop.sh`。详见 [mock/README.md](../README.md)。

```bash
cd mock/node
npm ci                      # 通过 file:../../sdk/node 引用 SDK，会自动构建
export LSP_TENANT_KEY_ACME=$(openssl rand -hex 24)   # 租户 API Key（≥ 32 字符）
export LMBP_SIGNING_SEED=$(openssl rand -hex 32)     # 可选：固定网关签名密钥
node gateway.mjs --listen 127.0.0.1:8080 --tenant acme:LSP_TENANT_KEY_ACME
# 或带 SaaS 展示身份（同意页显示的名称、链接、请求的模型与作用域）：
node gateway.mjs --listen 127.0.0.1:8080 --tenant-profiles ../tenant-profiles.example.json
```

| 参数 / 环境变量 | 说明 |
|---|---|
| `--listen` / `LMBP_LISTEN` | 监听地址，默认 `127.0.0.1:8080` |
| `--issuer` / `LMBP_ISSUER` | 网关 issuer origin（https；回环地址可用 http）。不设置时取请求的 origin（开发模式） |
| `--tenant id:ENV` | 注册租户，API Key 从环境变量读取（可重复） |
| `--tenant-profiles F` / `LMBP_TENANT_PROFILES` | 租户 JSON 文件，格式见 `../tenant-profiles.example.json` |
| `LMBP_SIGNING_SEED` | 32 字节十六进制 Ed25519 种子；不设置则每次启动随机生成 |
| `--db FILE` / `LMBP_DB` | 用 SQLite（better-sqlite3）持久化配对、设备、任务与签名密钥；默认内存 |
| `--max-concurrency` | 每设备并发上限，默认 16 |
| `--web-dir` / `LMBP_WEB_DIR` | 聊天页目录，默认 `../web`；在 `/` 与 `/chat` 提供，见 [../web/README.md](../web/README.md) |
| `--no-chat-ui` | 不提供聊天页 |

Docker：

```bash
docker build -f mock/node/Dockerfile -t lmbp-mock-node .   # 在仓库根目录构建，镜像内含聊天页
docker run --rm -p 8080:8080 -e LSP_TENANT_KEY_ACME -e LMBP_SIGNING_SEED lmbp-mock-node \
  --issuer http://127.0.0.1:8080 --tenant acme:LSP_TENANT_KEY_ACME
```

连接 Go 连接器：`localsubproxy connector -pair-link 'lmbp://pair?v=1&gateway=http://127.0.0.1:8080&tenant=acme' ...`，然后用租户 Key 调用 `POST /lmbp/v1/tenant/devices/approvals` 批准显示的配对码（或直接打开本网关自带的聊天页 http://127.0.0.1:8080/chat ，见 [../web/README.md](../web/README.md)）。

单元测试：`npm test`（聊天页与 CSP）；协议单元测试在 `sdk/node`（`cd sdk/node && npm test`）。

## 代码结构

- `gateway.mjs`：CLI（参数、租户文件、`SqliteStore`），约 60 行。
- 协议引擎、Store、配对 API、OpenAI/Anthropic 兼容层：见 [`sdk/node`](../../sdk/node/README.md)。

## 功能（与 Go 参考网关 `internal/lmbp/gateway` 等价）

| 能力 | 说明 |
|---|---|
| 配对（RFC 8628） | `POST /lmbp/v1/device/authorize`（form 或 JSON）→ `device_code` / `user_code`（`XXXX-XXXX`，字母表 BCDFGHJKLMNPQRSTVWXZ）、`interval`、`Cache-Control: no-store`；轮询 `authorization_pending` / `slow_down`；`device_code` 单次有效；租户后端 `POST /lmbp/v1/tenant/devices/approvals`（大小写/分隔符不敏感，`jkt` 钉住设备密钥，`deny: true` 拒绝） |
| 令牌 | EdDSA（Ed25519）JWT，`typ: at+jwt`，`iss` = 网关 issuer origin，`aud` = `<issuer>/tenants/<tenant>`，`cnf.jkt` 绑定设备密钥；`/.well-known/jwks.json` |
| 网关身份（§18/§19） | `GET /.well-known/lmbp`；`?tenant=` 时附带 SaaS 身份和 `signed_metadata`（`typ: lmbp-metadata+jwt`，1 小时有效） |
| 刷新（RFC 7523） | `grant_type=jwt-bearer`，设备私钥签名的断言，`aud` = `<issuer>/lmbp/v1/token`，`jti` 防重放，寿命 ≤ 300 s；配对时用 proof JWT（`aud: lmbp:token`，`ath` = hash(device_code)） |
| WebSocket `lmbp` | 子协议必选、拒绝浏览器 Origin、401/403 Problem Details；`hello` proof 校验；能力协商；`ping/pong`；未知消息不断开；`session.token` 续期；`models.update`；吊销即断开 |
| seq/ack/resume | 每任务数据帧编号 1..n，`task.ack` 依订阅者消费进度推进；`resume_token` 30 s 窗口，重连后重传未确认帧，重复帧丢弃，跳号即失败 |
| 信用流控 | 遵守设备通告的 `task_window` / `conn_window`（FIFO 公平），向设备通告网关窗口（1 MiB / 4 MiB），越界 `FLOW_CONTROL` |
| tools / 多模态 | `tools` / `tool_choice` 进入签名的 `task.run`；按模型能力拒绝（`CAPABILITY_UNSUPPORTED`）；> 16 KiB 的 `data:` 图片与 `input_audio` 改为 `blob.chunk`（96 KiB/块，签名的 size + sha256） |
| E2EE | 校验并发布设备的 X25519 密钥声明；`lmbp_e2ee` JWE 原样作为 `application/jose` blob 转发，网关不解密、不保存明文；SSE 透传 `lmbp_e2ee {seq,type,data}` |
| OpenAI 兼容 | `GET /v1/models`（含每模型能力）、`POST /v1/chat/completions`（SSE：`id: 0` 角色前导，事件 id = 数据帧 seq，`data: [DONE]`；非流式聚合 tool_calls）；`Idempotency-Key`（不同请求体 422）、`Last-Event-ID`、`GET /lmbp/v1/tenant/tasks/{id}/events` 重新挂接；客户端断开 → `task.cancel` |
| Anthropic 兼容 | `POST /v1/messages`（`x-api-key` 或 Bearer；非流式与 SSE 命名事件 `message_start`…`message_stop`；text / image / tool_use / tool_result 转换；断线重连时 `Idempotency-Key` + `Last-Event-ID` 续传）；带 `anthropic-version` 头时 `/v1/models` 返回 Anthropic 列表格式；错误为 `{type:"error",error:{type,message,lmbp_code}}`。见规范 §6.7 |
| 错误 | `/lmbp/v1/*` 返回 RFC 9457 `application/problem+json`（`type/title/status/detail/code`）；`/v1/*` 返回 OpenAI 错误对象；OAuth 端点返回 `{"error": ...}`。被拒绝的 `user_code` 再次批准返回 403 `CONSENT_DENIED` |

默认状态在内存中：重启后设备需重新配对；加 `--db` 后配对与任务事件落到 SQLite，重启后设备可直接重连。

## 验证

```bash
# 在仓库根目录：对 Node、Python 和 Go 参考网关分别跑 lmbp-conformance（73 项，含 Anthropic `/v1/messages` A1–A10）和 Go 连接器端到端测试
scripts/mock-interop.sh node node-sqlite python python-sqlite go sdk-go
```

端到端测试（`mock/external`）用真实的桌面连接器通过 `lmbp://pair` 链接配对（校验签名元数据、本地拒绝、SaaS 拒绝），再跑流式对话、tools、40 KiB 图片、E2EE、本地审计日志和吊销。
