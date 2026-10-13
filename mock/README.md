# mock/：协议 v2 的服务端模拟实现

> 网关、存储和设备端已移到 `internal/lsp2/*`，作为正式代码使用。本目录只保留业务服务模拟（`saas/`）、假模型、演示和 `lspmock` 命令行。

本目录提供业务服务（SaaS）的模拟实现，配合 `internal/lsp2` 中的参考网关与设备端使用。协议说明见 [docs/protocol-v2.md](../docs/protocol-v2.md)。

| 包 | 作用 |
|---|---|
| `saas/` | Mock 业务服务：登录、`/link` 配对页、设备列表、聊天页（转发网关的 SSE 流） |
| `fakemodel/` | 一个假的模型：OpenAI 兼容 `/v1` 与 Ollama 原生 `/api/chat`、`/api/tags`，都支持流式输出 |
| `demo/` | 在单个进程内跑完整端到端演示，同时也是测试 |
| `cmd/lspmock/` | 命令行入口：`demo / gateway / saas / connector / fakemodel` |
| `node/` | Node.js 版完整网关（与参考网关等价，通过 63 项 `lsp2-conformance`），见 [node/README.md](node/README.md) |
| `python/` | Python（aiohttp）版完整网关，同上，见 [python/README.md](python/README.md) |
| `web/` | 三种网关共用的模拟 SaaS 聊天页（`/`、`/chat`）：配对、设备、流式对话、图片、停止、用量与耗时，见 [web/README.md](web/README.md) |
| `external/` | 用真实 Go 连接器对任意外部网关做端到端测试（`LSP2_EXTERNAL_GATEWAY`）；`scripts/mock-interop.sh` 一键跑三种实现 |

## 一条命令看完整流程

```bash
go run ./mock/cmd/lspmock demo
```

演示依次覆盖：配对（链接携带设备公钥指纹 jkt）、握手与能力协商、流式回答、**断线续传**（流式途中两次断开设备 WebSocket，结果逐字一致）、**流控背压**（客户端暂停读取时设备执行器被阻塞）、**tools 往返**、**图片输入**（120 KiB data: URI 走 blob 分块）、**端到端加密**（`/api/chat/e2ee`，网关存储与日志中无明文）、中途取消、上游报错与模型不存在、同租户隔离、静态加密、令牌续期、吊销设备。

## 分进程运行（可接真实的 LM Studio 或 Ollama）

```bash
go build -o bin/lspmock ./mock/cmd/lspmock
export LSP_TENANT_KEY_ACME=$(openssl rand -hex 24)   # 租户 API Key，网关与 SaaS 使用同一个
export LSP_TENANT_KEY=$LSP_TENANT_KEY_ACME
export LSP_DATA_KEY=$(openssl rand -hex 32)          # 可选：开启提示词和输出的静态加密
export LSP_SIGNING_SEED=$(openssl rand -hex 32)      # 网关签名私钥的种子；不设置的话，网关重启后设备需要重新配对

bin/lspmock fakemodel &                               # 用真实 LM Studio 时可跳过这一步
bin/lspmock gateway -db gateway.db -tenants acme:LSP_TENANT_KEY_ACME &
bin/lspmock saas -tenant acme -users alice:alice-pw,bob:bob-pw &
bin/lspmock connector -saas http://127.0.0.1:3000 -model-url http://127.0.0.1:11500 -models qwen3-8b -caps tools,vision
```

连接器启动后会打印配对码。用浏览器打开 http://127.0.0.1:3000 ，以 `alice / alice-pw` 登录，在“连接新设备”页面输入配对码。之后首页会显示设备在线，可以直接流式对话，点“停止”即可取消当前任务。端到端加密对话走 `POST /api/chat/e2ee`（SaaS 用配对时钉住的 jkt 校验设备 X25519 密钥声明后封装 JWE）。

- 接 LM Studio：`-model-url http://127.0.0.1:1234 -models <模型名>`
- 接 Ollama：`-model-url http://127.0.0.1:11434 -models llama3.2`（使用 Ollama 自带的 `/v1` 接口）

## 正式部署前须知

- 网关和 SaaS 前面需要加 HTTPS 反向代理；`/v2/connect` 要允许 WebSocket 升级，SSE 路径要关闭缓冲。
- mock SaaS 的用户名和密码直接写在命令行参数里，仅供演示。
- 会话保存在网关进程的内存中，部署多个网关实例时需要做粘性路由。
