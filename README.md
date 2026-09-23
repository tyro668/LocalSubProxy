# localsubproxy

基于 [`design/SaaS本地大模型连接器详细设计方案.html`](design/SaaS本地大模型连接器详细设计方案.html) 的 Go 最小闭环。一个二进制提供两个进程：

- `server`：SaaS 侧参考任务 API、调度、WSS 接入端和 HTTPS 长轮询降级端。SaaS 应用通过任务 API 提交请求与订阅结果。
- `connector`：用户设备上的常驻出站连接器。可选本机 Ollama 或已由用户登录的官方 Codex CLI；模型凭证留在官方客户端。

代码使用 Go 标准库与 `github.com/gorilla/websocket`，要求 Go 1.22 或更新版本。当前实现为**单租户、单设备、单次启用一个适配器的可运行 MVP**。它可用于接入验证；生产部署还需接入现有 SaaS 登录、租户权限与正式设备凭据系统。

## 启动

使用 Ollama 时先启动服务，并确认已下载模型：

```bash
ollama list
```

生成三份不同的随机密钥。`LOCALSUBPROXY_JOURNAL_KEY` 必须在连接器重启后保持一致，否则无法解密未确认事件。不要把这些值提交到仓库。

```bash
export LOCALSUBPROXY_SAAS_TOKEN="$(openssl rand -hex 32)"
export LOCALSUBPROXY_DEVICE_TOKEN="$(openssl rand -hex 32)"
export LOCALSUBPROXY_JOURNAL_KEY="$(openssl rand -hex 32)"
```

先构建，再在两个终端运行。两个进程需要相同的 `LOCALSUBPROXY_DEVICE_TOKEN`。SaaS token 只交给 SaaS 应用；journal key 只交给连接器。

```bash
go build -o bin/localsubproxy ./cmd/localsubproxy
bin/localsubproxy server -listen 127.0.0.1:8080 -data ./data/server.json -device-id dev_local
```

Ollama 连接器：

```bash
bin/localsubproxy connector \
  -server-url http://127.0.0.1:8080 \
  -device-id dev_local \
  -adapter-id ollama-local \
  -ollama-url http://127.0.0.1:11434 \
  -models llama3.2 \
  -journal ./data/journal.enc
```

把 `llama3.2` 替换为 `ollama list` 中的模型名。正式网络部署时，在 server 前提供支持 WebSocket Upgrade 的 HTTPS 反向代理，connector 使用 `https://` 地址，自动连接对应的 `wss://`；程序仅允许回环 IP 使用明文 HTTP/WS。默认 server 仅监听回环地址。

连接器出站访问 `/v1/connector/connect`，使用 `connector.v1` 子协议与设备 Bearer token。WSS 承载心跳、任务派发、接受、启动许可、取消、事件与持久化 ACK；握手被代理阻止或连接断开时，按指数退避重试，并通过原有 HTTPS 长轮询处理任务。重连时携带本地 journal 游标，服务端返回已保存的序号；连接器补传未确认事件，由服务端校验重复事件内容。单条 WSS 消息上限 256 KiB，心跳周期 25 秒，75 秒无活动视为断线。生产环境应由反向代理终止 TLS，并保留标准证书校验。

文本附件试点支持 UTF-8 `text/plain`，单文件最大 1 MiB、每任务最多 5 个。内联任务 JSON 上限 128 KiB，较长文本需改用附件。SaaS 上传到 `POST /v1/task-artifacts`，将返回的 `artifact_id` 放进任务的 `input.artifact_refs`；网关在同一任务内锁定引用，连接器只可凭设备身份及当前 `task_id`、`attempt_id` 下载，并校验 SHA-256 后作为不可信文本交给模型。SaaS 不能指定本机文件路径；二进制文件、转换器和结果产物上传尚未支持。附件和正文都经过 SaaS。

执行前服务端发放最多 120 秒的运行租约，运行中每 30 秒前续期；断网超过租约时连接器停止运行时并上报 `UNKNOWN`，服务端进入 `RECONCILING` 等待对账。本地 journal 默认上限 64 MiB，给终态事件预留 128 KiB；云端确认后的输出分块与敏感输入从 journal 中清理。可用 `-journal-max-mib` 调整上限。

模拟 SaaS 位于相邻的 `sophorn-mock-server` 项目。它用现有用户 Bearer token 校验 `ai` 权限与设备所有者，再以仅服务端持有的 `LOCALSUBPROXY_SAAS_TOKEN` 调用本服务。在模拟 SaaS 中设置 `LOCALSUBPROXY_URL=http://127.0.0.1:8080`、相同的 `LOCALSUBPROXY_SAAS_TOKEN`、`LOCALSUBPROXY_DEVICE_ID=dev_local`。控制台“本地模型”页可提交和取消任务。

联调设备认领时，用 `-require-binding` 启动 Go server。在连接器所在设备运行 `pairing` 命令，取得 5 分钟有效的 8 位码；用模拟 SaaS 登录用户在“本地模型”页输入认领码，完成后再启动 `connector`。绑定前网关拒绝设备连接与任务提交。错误码最多尝试 5 次，重新签发会使旧码失效；绑定结果和所有者写入网关快照。模拟 SaaS 从登录态读取用户 ID，浏览器传入的 `user_id` 无效。

服务端终端：

```bash
bin/localsubproxy server -listen 127.0.0.1:8080 -data ./data/server.json -device-id dev_local -require-binding
```

设备终端：

```bash
bin/localsubproxy pairing -server-url http://127.0.0.1:8080 -device-id dev_local
```

不启用 `-require-binding` 的旧联调方式仍使用 `LOCALSUBPROXY_OWNER_USER_ID`（默认 `user_demo-user`）作模拟 SaaS 的固定所有者。认领流程目前建立在预置设备 Bearer token 上，尚未实现设计稿中的设备密钥证明、短期设备令牌与轮换；不要把它当作生产设备注册。

### Codex 官方客户端试点

先按[官方认证文档](https://learn.chatgpt.com/docs/auth)在本机运行 `codex login`，选择 ChatGPT 登录。连接器只接受 App Server 报告的 ChatGPT 账户；API Key 登录不会被当作订阅账户使用。读取当前账户可见的模型 ID 不会发起推理：

```bash
bin/localsubproxy codex-models
```

从输出中选择一个模型，在**同一个 OS 用户账户**下运行连接器：

```bash
bin/localsubproxy connector \
  -server-url http://127.0.0.1:8080 \
  -device-id dev_local \
  -adapter-id codex-local \
  -adapter-type codex_app_server \
  -codex-bin "$(command -v codex)" \
  -models YOUR_CODEX_MODEL_ID \
  -journal ./data/journal.enc
```

SaaS 请求的 `operation` 必须为 `agent.run`，目标 `adapter_id` 为 `codex-local`。例如：

```bash
curl -sS http://127.0.0.1:8080/v1/llm-tasks \
  -H "Authorization: Bearer $LOCALSUBPROXY_SAAS_TOKEN" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: codex-example-0001' \
  -d '{"target":{"device_id":"dev_local","adapter_id":"codex-local","model_id":"YOUR_CODEX_MODEL_ID"},"operation":"agent.run","input":{"messages":[{"role":"user","content":"用三句话总结这段文本。"}]},"options":{"queue_ttl_seconds":300,"max_runtime_seconds":600}}'
```

Codex 适配器使用[官方 App Server 的 stdio JSON-RPC](https://learn.chatgpt.com/docs/app-server)：每次任务启动独立进程与临时工作目录，核对 ChatGPT 登录和模型列表，创建临时线程，转发最终答案片段。SaaS 的消息角色会作为带标签的文本输入，并不会转换成 Codex 的系统指令；工具、附件和会话续接尚未开放。它要求只读沙箱、禁止审批，并请求关闭 Shell、Apps、Hooks、MCP 等扩展；遇到工具动作事件会终止任务。取消时先请求 `turn/interrupt`；只有收到运行时的 `interrupted` 终态才确认 `CANCELLED`，否则保留 `UNKNOWN`。**这是试点适配器**：官方目前将 `codex app-server` 标为实验性，且当前 CLI 的只读沙箱不能证明所有用户级配置和插件均被完全隔离。请在受控终端验证版本与权限后使用，勿将其作为无监督、可处理敏感本机文件的生产执行器。任务文本会由 Codex 发送至 OpenAI，任务输入与结果仍经过 SaaS。

## SaaS 调用

SaaS 服务端保存 `LOCALSUBPROXY_SAAS_TOKEN`，以 Bearer token 调用。每次逻辑提交生成新的 `Idempotency-Key`；网络重试沿用原键。不要把此 token 放入浏览器。

```bash
curl -sS http://127.0.0.1:8080/v1/llm-tasks \
  -H "Authorization: Bearer $LOCALSUBPROXY_SAAS_TOKEN" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: example-0001' \
  -d '{"target":{"device_id":"dev_local","adapter_id":"ollama-local","model_id":"llama3.2"},"operation":"chat.generate","input":{"messages":[{"role":"user","content":"你好，请用一句话介绍自己"}]},"options":{"queue_ttl_seconds":300,"max_runtime_seconds":600}}'
```

响应为 `202` 和 `task_id`。查询与订阅：

```bash
curl -sS -H "Authorization: Bearer $LOCALSUBPROXY_SAAS_TOKEN" \
  http://127.0.0.1:8080/v1/llm-tasks/TASK_ID

curl -N -H "Authorization: Bearer $LOCALSUBPROXY_SAAS_TOKEN" \
  http://127.0.0.1:8080/v1/llm-tasks/TASK_ID/events
```

SSE 事件带 `attempt_id:seq` 形式的 ID，重连时传 `Last-Event-ID`。取消请求：

```bash
curl -X POST -H "Authorization: Bearer $LOCALSUBPROXY_SAAS_TOKEN" \
  http://127.0.0.1:8080/v1/llm-tasks/TASK_ID/cancel
```

`CANCEL_REQUESTED` 表示请求已登记；最终可能为 `CANCELLED`、`SUCCEEDED` 或 `UNKNOWN`。任务和事件经原子文件替换持久化，本地待确认事件保存在 AES-GCM 加密 journal 中。连接器重启后会补传事件；若启动意图已写入但上游结果无法确认，会标记 `UNKNOWN`，不会自动重复调用模型。

## 实现边界

这版实现了 **WSS 主通道、HTTPS 长轮询降级、设备撤销、基础运行租约、模拟用户认领和受限文本附件**，并接入模拟 SaaS 的用户鉴权与任务页面。SaaS 侧的两个静态 Bearer token 仍只适合单设备集成环境；尚未实现设备密钥证明、短期令牌轮换、多租户设备注册、数据库 outbox、通用附件与结果产物、完整运行时恢复、托盘界面和安装更新。服务端当前用单文件快照保存状态，适合小规模验证，不能作为多实例共享事实存储。Ollama 地址与 Codex 可执行文件由本机启动参数决定，云端不能更改。模型用量未知时返回 `null`，不伪造 token 数量。

Claude Code 的[官方合规文档](https://code.claude.com/docs/en/legal-and-compliance)明确限制第三方开发者通过 Free、Pro、Max 订阅凭证代用户路由请求，因此没有加入 Claude 订阅代理。若产品需要 Claude 能力，应另行设计符合官方条款的 API Key 或受支持云提供商路径。

生产接入应将静态设备 Bearer token 换成短期、设备密钥绑定的凭据，以关系数据库事务实现任务、outbox、事件与幂等记录，并按设计稿补齐正式设备注册、跨进程租约、版本协商和运维能力。不得把设备 token、journal key 或模型厂商凭证发送给 SaaS 前端。

## 验证

```bash
go test ./...
go vet ./...
```

集成测试会在回环地址启动假 Ollama、假 Codex App Server 和 SaaS server，验证 TLS 下的 WSS、HTTPS 降级、世代隔离、事件持久化 ACK、模型调用、幂等、流式事件、权限拒绝及取消。测试不调用真实模型，也不消耗订阅额度。
