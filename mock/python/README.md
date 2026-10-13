# Python LSP2 模拟网关

`gateway.py` 是协议 v2（[docs/protocol-v2.md](../../docs/protocol-v2.md)）网关的 Python 实现（asyncio + aiohttp，Ed25519 用 `cryptography`），可替代 Go 参考网关做互通测试。需要 Python ≥ 3.10。

## 运行

```bash
cd mock/python
python3 -m venv .venv && .venv/bin/pip install -r requirements.txt
export LSP_TENANT_KEY_ACME=$(openssl rand -hex 24)   # 租户 API Key（≥ 32 字符）
export LSP2_SIGNING_SEED=$(openssl rand -hex 32)     # 可选：固定网关签名密钥
.venv/bin/python gateway.py --listen 127.0.0.1:8080 --tenant acme:LSP_TENANT_KEY_ACME
# 或带 SaaS 展示身份：
.venv/bin/python gateway.py --listen 127.0.0.1:8080 --tenant-profiles ../tenant-profiles.example.json
```

参数与 Node 版相同：`--listen`、`--issuer`、`--tenant id:ENV`（可重复）、`--tenant-profiles`、`--signing-key-env`（默认读 `LSP2_SIGNING_SEED`）、`--max-concurrency`、`--web-dir`（聊天页目录，默认 `../web`，见 [../web/README.md](../web/README.md)）、`--no-chat-ui`；对应环境变量 `LSP2_LISTEN`、`LSP2_ISSUER`、`LSP2_TENANT_PROFILES`。

Docker：

```bash
docker build -f mock/python/Dockerfile -t lsp2-mock-python .   # 在仓库根目录构建，镜像内含聊天页
docker run --rm -p 8080:8080 -e LSP_TENANT_KEY_ACME -e LSP2_SIGNING_SEED lsp2-mock-python \
  --issuer http://127.0.0.1:8080 --tenant acme:LSP_TENANT_KEY_ACME
```

单元测试：`.venv/bin/python -m unittest test_lsp2 test_web`。

## 实现要点

- `lsp2.py`：base64url、EdDSA JWS/JWT、proof、指纹、origin、`Outbox` / `Inbound`。
- `gateway.py`：每个 WebSocket 连接只有一个写协程，所有帧（控制帧和流控数据帧）按入队顺序发出，保证 seq 顺序；服务器以 `handler_cancellation=True` 运行，客户端断开 SSE 时处理协程被取消，从而向设备发送 `task.cancel`。

## 功能（与 Go 参考网关 `internal/lsp2/gateway` 等价）

| 能力 | 说明 |
|---|---|
| 配对（RFC 8628） | `POST /v2/device/authorize`（form 或 JSON）→ `device_code` / `user_code`（`XXXX-XXXX`，字母表 BCDFGHJKLMNPQRSTVWXZ）、`interval`、`Cache-Control: no-store`；轮询 `authorization_pending` / `slow_down`；`device_code` 单次有效；租户后端 `POST /v2/tenant/device-approvals`（大小写/分隔符不敏感，`jkt` 钉住设备密钥，`deny: true` 拒绝） |
| 令牌 | EdDSA（Ed25519）JWT，`typ: at+jwt`，`iss` = 网关 issuer origin，`aud` = `<issuer>/tenants/<tenant>`，`cnf.jkt` 绑定设备密钥；`/.well-known/jwks.json` |
| 网关身份（§18/§19） | `GET /.well-known/lsp2-gateway`；`?tenant=` 时附带 SaaS 身份和 `signed_metadata`（`typ: lsp2-metadata+jwt`，1 小时有效） |
| 刷新（RFC 7523） | `grant_type=jwt-bearer`，设备私钥签名的断言，`aud` = `<issuer>/v2/token`，`jti` 防重放，寿命 ≤ 300 s；配对时用 proof JWT（`aud: lsp2:token`，`ath` = hash(device_code)） |
| WebSocket `lsp2` | 子协议必选、拒绝浏览器 Origin、401/403 Problem Details；`hello` proof 校验；能力协商；`ping/pong`；未知消息不断开；`session.token` 续期；`models.update`；吊销即断开 |
| seq/ack/resume | 每任务数据帧编号 1..n，`task.ack` 依订阅者消费进度推进；`resume_token` 30 s 窗口，重连后重传未确认帧，重复帧丢弃，跳号即失败 |
| 信用流控 | 遵守设备通告的 `task_window` / `conn_window`（FIFO 公平），向设备通告网关窗口（1 MiB / 4 MiB），越界 `FLOW_CONTROL` |
| tools / 多模态 | `tools` / `tool_choice` 进入签名的 `task.run`；按模型能力拒绝（`CAPABILITY_UNSUPPORTED`）；> 16 KiB 的 `data:` 图片与 `input_audio` 改为 `blob.chunk`（96 KiB/块，签名的 size + sha256） |
| E2EE | 校验并发布设备的 X25519 密钥声明；`lsp2_e2ee` JWE 原样作为 `application/jose` blob 转发，网关不解密、不保存明文；SSE 透传 `lsp2_e2ee {seq,type,data}` |
| OpenAI 兼容 | `GET /v1/models`（含每模型能力）、`POST /v1/chat/completions`（SSE：`id: 0` 角色前导，事件 id = 数据帧 seq，`data: [DONE]`；非流式聚合 tool_calls）；`Idempotency-Key`（不同请求体 422）、`Last-Event-ID`、`GET /v2/tenant/tasks/{id}/events` 重新挂接；客户端断开 → `task.cancel` |
| 错误 | `/v2/*` 返回 RFC 9457 `application/problem+json`（`type/title/status/detail/code`）；`/v1/*` 返回 OpenAI 错误对象；OAuth 端点返回 `{"error": ...}`。被拒绝的 `user_code` 再次批准返回 403 `CONSENT_DENIED` |

状态全部在内存中（模拟用途）：重启后设备需重新配对，任务事件不落盘。

## 验证

```bash
# 在仓库根目录：对 Node、Python 和 Go 参考网关分别跑 lsp2-conformance（63 项）和 Go 连接器端到端测试
scripts/mock-interop.sh node python go
```

端到端测试（`mock/external`）用真实的桌面连接器通过 `lsp2://pair` 链接配对（校验签名元数据、本地拒绝、SaaS 拒绝），再跑流式对话、tools、40 KiB 图片、E2EE、本地审计日志和吊销。
