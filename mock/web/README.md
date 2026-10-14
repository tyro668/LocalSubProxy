# mock/web：模拟 SaaS 聊天页

三种网关（Go 参考网关、`mock/node`、`mock/python`）共用的一个静态页面，用来模拟“SaaS 用户通过已配对的本机模型聊天”。纯 HTML + JS，无构建步骤，中文界面。

| 路径 | 内容 |
|---|---|
| `/`、`/chat` | `chat.html` |
| `/chat/chat.js` | 页面逻辑 |
| `/chat/qrcode.js` | 二维码生成（[qrcode-generator](https://github.com/kazuhikoarase/qrcode-generator) 1.4.4，MIT，原样内置，不依赖 CDN） |

功能：

- **配对状态**和**配对新设备**：生成 `lmbp://pair` 链接与二维码，复制 Connector 命令；输入设备显示的 `user_code`（可选 `jkt` 强校验）后点“批准”或“拒绝”。页面带 `?user_code=…&jkt=…` 打开时会自动填入，所以租户档案的 `pairing_uri` 可以直接指向 `<网关>/chat`。
- **我的设备**：在线/离线/已吊销，每台设备的模型与能力，吊销按钮。每 4 秒刷新一次。
- **聊天**：选择模型，通过 `/v1/chat/completions` 的 SSE 流式输出；多轮历史；“停止”会中断请求，网关随即向设备发送 `task.cancel`；“添加图片”以 `data:` URI 发送（超过 16 KiB 时网关走 blob 分块）。
- 每条回答显示首字耗时、总耗时、输入/输出 tokens、tok/s、`finish_reason` 与 `task_id`。
- 错误按错误码显示中文说明（`DEVICE_OFFLINE`、`CONSENT_DENIED`、`MODEL_NOT_FOUND`、`DEVICE_BUSY`、`CAPABILITY_UNSUPPORTED`、`PAYLOAD_TOO_LARGE`、`UNAUTHENTICATED` 等）。

## 认证（开发登录）

页面本身**不含任何密钥**。打开后，用以下三项登录：

- 租户 ID（如 `acme`）
- 最终用户（作为 `X-End-User` 发送，如 `alice`）
- 租户 API Key（网关 `--tenant acme:ENV` 中环境变量 `ENV` 的值）

页面先调用 `GET /lmbp/v1/tenant/devices` 验证 Key，然后把它保存在**本标签页的 sessionStorage** 中（关闭标签页即清除），只发给本网关的 `/lmbp/v1/tenant/*` 与 `/v1/*`。

这相当于让浏览器扮演“租户后端”，**仅供开发与演示使用**。真实 SaaS 中，租户 Key 只能放在后端，`X-End-User` 应由后端根据登录会话注入。因此：

- Go 参考网关默认**不提供**此页面，需要显式加 `-chat-ui`（`lspmock gateway` 默认开启）；
- Node/Python 模拟网关默认开启，可用 `--no-chat-ui` 关闭。

所有页面文件都带严格的 CSP（`script-src 'self'`，`connect-src 'self'`，`frame-ancestors 'none'`），以及 `X-Content-Type-Options: nosniff` 和 `Referrer-Policy: no-referrer`。用户和模型的文本一律用 `textContent` 渲染。

## 运行

```bash
export LSP_TENANT_KEY_ACME=$(openssl rand -hex 24)        # 租户 API Key，登录页面时粘贴它
export LOCALSUBPROXY_DATA_KEY=$(openssl rand -hex 32)     # 连接器加密本机适配器配置用

# 1) 假模型（同时提供 Ollama 原生 /api/chat 和 OpenAI /v1）；有真实 Ollama 时可跳过
go run ./mock/cmd/lspmock fakemodel -listen 127.0.0.1:11500 -models qwen3-8b &

# 2) 任选一个网关（都在 http://127.0.0.1:8080/chat 提供页面）
go run ./cmd/localsubproxy server -listen 127.0.0.1:8080 -db /tmp/gw.db -tenants acme:LSP_TENANT_KEY_ACME \
  -tenant-profiles mock/tenant-profiles.example.json -issuer http://127.0.0.1:8080 -chat-ui &
#   node mock/node/gateway.mjs --listen 127.0.0.1:8080 --issuer http://127.0.0.1:8080 --tenant-profiles mock/tenant-profiles.example.json &
#   mock/python/.venv/bin/python mock/python/gateway.py --listen 127.0.0.1:8080 --issuer http://127.0.0.1:8080 --tenant-profiles mock/tenant-profiles.example.json &

# 3) 浏览器打开 http://127.0.0.1:8080/chat ，以 acme / alice / $LSP_TENANT_KEY_ACME 登录，复制页面上的 Connector 命令，例如：
go run ./cmd/localsubproxy connector -pair-link 'lmbp://pair?v=1&gateway=http%3A%2F%2F127.0.0.1%3A8080&tenant=acme' \
  -adapter-id local -ollama-url http://127.0.0.1:11500 -models qwen3-8b -capabilities vision \
  -identity /tmp/dev/id.json -adapter-file /tmp/dev/adapters.enc -audit-db /tmp/dev/audit.db
#    在终端确认授权后，把连接器打印的配对码填到页面上，点“批准”。设备上线后就可以对话（模型名为 local/qwen3-8b）。
```

接真实 Ollama：`-ollama-url http://127.0.0.1:11434 -models llama3.2`。

## 端到端测试

```bash
scripts/chat-ui-e2e.sh                  # go node python；SHOTS=目录 指定截图位置，CHROME=浏览器路径
```

对每个网关，用无头 Chrome（playwright-core）完成以下检查：

1. 页面与 CSP 正常，HTML 中没有 Key；
2. 错误 Key 被拒，正确 Key 登录成功；
3. 显示配对链接与二维码；
4. 启动真实 Go 连接器（Ollama 适配器 → 假模型的 `/api/chat`），在页面上用 `user_code` + `jkt` 批准；
5. 设备上线；
6. 流式回答（逐步增长）带耗时与 tokens；
7. 第二轮带上历史；
8. 上传 ~35 KB 图片（走 blob）并被模型原样收到；
9. 停止生效；
10. 拒绝后再批准显示 `CONSENT_DENIED`；
11. 关闭连接器后发消息显示 `DEVICE_OFFLINE`。

截图：`chat-<impl>-paired.png`、`chat-<impl>.png`、`chat-<impl>-errors.png`。
