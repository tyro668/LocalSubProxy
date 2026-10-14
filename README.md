# LocalSubProxy

LocalSubProxy 是一个 macOS / Windows 桌面应用，让远端服务使用本机已登录的 Codex 订阅，或使用你配置的 OpenAI、Anthropic 兼容 API。打开应用时，配置界面和本地内核会一起启动；添加模型方案无需先连接远端服务。

## 使用流程

1. 打开应用，在底部的「远端服务地址」输入服务地址（或粘贴 SaaS 提供的 `lmbp://pair?…` 配对链接），点击「连接」。按联调说明启动参考网关和模拟服务后，本机可填写 `http://127.0.0.1:3000`；远端地址使用 HTTPS。
2. 应用先显示**配对同意页**：SaaS 名称、logo、主页、隐私政策、网关地址、请求的模型与权限。这些信息都经过网关签名校验，不是链接里自带的。点击「同意并配对」后，应用显示一个 `XXXX-XXXX` 配对码和确认链接；点击「拒绝」则不会向网关发送任何请求。
3. 在已登录的远端服务页面输入配对码并确认，应用会自动完成连接。界面会显示本机设备名称、服务地址和连接状态；设备私钥加密保存在本机。远端服务或网关停止时会显示「未连接」，服务恢复后自动重连，无需重新配对。
4. 点击「添加方案」配置模型。你也可以先添加方案，再连接远端服务。
5. 连接后，「近 7 天用量」按 SaaS × 模型显示请求数、token 和失败数。每次请求都写入本机审计日志 `audit.db`，记录 SaaS、模型、时间、token、延迟、状态和错误码，**不记录对话内容**，默认保留 90 天。命令行可以用 `localsubproxy usage -group saas,model,day` 和 `localsubproxy audit` 查看。

## 配置模型方案

点击「添加方案」，填写方案名称并选择「API 提供商」。目前可以新建三种方案：

| 提供商 | 需要填写或操作 |
| --- | --- |
| OpenAI Codex（订阅） | 使用本机官方 Codex CLI 的 ChatGPT 登录态；如未登录，点击「连接订阅」并在浏览器完成登录，然后选择模型。Codex 只接受纯文本：多轮对话会合并成一段带角色标签的文本，`temperature`、`top_p`、`stop`、`max_tokens` 不受支持，请求中带这些参数会被明确拒绝（`/v1/messages` 必填的 `max_tokens` 除外，会被忽略），见规范 §16.4。 |
| OpenAI 兼容协议 | 填写 API 地址、API 密钥和供应商提供的模型 ID。 |
| Anthropic 兼容协议 | 填写 API 地址、API 密钥和供应商提供的模型 ID。 |

「上下文窗口」和「最大输出 Token」可留空使用自动检测。若提供商不支持模型列表接口，开启「模型列表探测」旁的开关以跳过探测。填写完成后可点击「测试连接」检查可用性，再点击「确定」保存。

方案列表中可以编辑、重命名、设为默认方案或删除方案。断开远端服务连接时，本机模型方案会保留。

## 窗口与退出

点击窗口关闭按钮只会隐藏配置界面，内核继续在后台运行；macOS 的 Dock 图标也会隐藏。通过系统托盘菜单的「配置」重新打开窗口，或选择「退出」停止应用。

## 从源码构建

macOS 运行 `./scripts/build-macos.sh`，Windows 运行 `.\scripts\build-windows.ps1`。本机联调见 [mock/README.md](mock/README.md)（一键启动：`scripts/mock-start.sh [go|node|python]`）；服务端接口见 [docs/lmbp-spec.md](docs/lmbp-spec.md)。

## LMBP 协议与其他语言实现

设备与网关之间使用开放协议 **LMBP（Local Model Bridge Protocol）1.0**。它完全基于公开标准：
- RFC 8628 设备配对；
- 用 Ed25519/EdDSA 签名的 JWS/JWT，令牌与设备密钥绑定（`cnf.jkt`）；
- WebSocket 子协议 `lmbp`；
- OpenAI 兼容接口，支持 SSE 流式输出；
- 可协商的扩展：断线续传（resume）、按字节窗口的流量控制（flow）、tools 与多模态输入（tools / multimodal）、基于 JWE（ECDH-ES + X25519, A256GCM）的端到端加密（e2ee）。

任何语言（Java、Node.js、Python、Go、Rust……）都可以实现网关或租户后端：

| 资料 | 位置 |
|---|---|
| 规范（RFC 风格） | [docs/lmbp-spec.md](docs/lmbp-spec.md) |
| 实现指南（各语言推荐库、陷阱、代码骨架） | [docs/implementers-guide.md](docs/implementers-guide.md) |
| JSON Schema / OpenAPI 3.1 / AsyncAPI 3.0 | [spec/](spec) |
| 测试向量 | [spec/testvectors/vectors.json](spec/testvectors/vectors.json) |
| Python / Node.js / Rust 协议示例骨架（已用测试向量验证，不是 SDK） | [spec/examples/](spec/examples) |
| 黑盒一致性测试 | `LMBP_TENANT_KEY=... go run ./cmd/lmbp-conformance -gateway <url> -tenant <id>` |
| 一键校验 | `spec/validate.sh`（契约与向量）；`scripts/verify-all.sh`（全量矩阵，任何跳过都判失败） |

Go 参考实现位于 `internal/lmbp`，网关用 `localsubproxy server -issuer https://<网关公网地址> [-tenant-profiles profiles.json]` 启动。`profiles.json` 里配置每个租户的展示身份，它会出现在桌面端的同意页上。业务服务的模拟实现与端到端演示见 [mock/README.md](mock/README.md)（`go run ./mock/cmd/lspmock demo`）。

### 各实现的成熟度（如实说明）

| 实现 | 位置 | 成熟度 |
|---|---|---|
| Go | `internal/lmbp`、`localsubproxy server`、`sdk/go` | **参考实现**：`sdk/go` 内嵌参考网关 `internal/lmbp/gateway`，与 `localsubproxy server` 是同一份代码。行为以它为准。 |
| Node.js | `sdk/node`（`mock/node` 是它的命令行外壳） | **独立重新实现**，与 Go 不共享代码，存在真实的行为差异。例如：JWKS 返回 `application/json`（Go 为 `application/jwk-set+json`）；`Store` 接口是同步的、没有 `context`，Go 的接口返回 `error`；HTTP/WebSocket 栈、超时与默认参数的实现细节不同。 |
| Python | `sdk/python`（`mock/python` 是它的命令行外壳） | **独立重新实现**（纯 ASGI，自带 HTTP/SSE 渲染），同样与 Go 存在真实差异：`Store` 是同步 Protocol；WebSocket 依赖 uvicorn 的 `websockets` 实现；时间参数以秒为单位，等等。 |
| Rust | `mock/rust`（模拟网关）、`spec/examples/rust`（协议示例骨架） | `mock/rust` 是 Node 引擎的**模拟网关移植**：通过 `lmbp-conformance`、连接器端到端、官方客户端与聊天页 E2E，但状态只在内存（无持久化存储）、HTTP/WebSocket 为手写最小实现，**不是 SDK，不面向生产**。`spec/examples/rust` 仍只是 JOSE/E2EE/流控骨架。 |
| Java | `mock/java`（模拟网关）、实现指南中的示意代码 | `mock/java` 是只依赖 JDK 21 的**模拟网关移植**，测试范围同 Rust，同样只在内存中保存状态，**不是 SDK，不面向生产**。实现指南里的 Java 片段（Jackson/Spring 写法）仍是未编译的示意代码。 |

> **通过共享 Schema、测试向量（含 `spec/testvectors/jose.json` 的负例）和 `lmbp-conformance`，不等于符合规范。** 它们只说明：实现在被检查的那些点上，与参考实现和彼此一致。黑盒检查覆盖不到的路径，例如存储写入失败、并发竞态、资源上限和错误恢复，仍然可能不同。2026-10 审查发现的问题（包括 ACK 先于持久化、排队期间重复请求被判为 `OUTCOME_UNKNOWN`）在修复前都“全部通过”了当时的一致性测试。全量矩阵见 `scripts/verify-all.sh`（任何失败或跳过都判为失败）。
