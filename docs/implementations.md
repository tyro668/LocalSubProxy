# 各语言实现与成熟度

规范见 [lmbp-spec.md](lmbp-spec.md)。下表如实说明每个实现和 Go 参考实现的关系。

| 实现 | 位置 | 成熟度 |
|---|---|---|
| Go | `internal/lmbp`、`localsubproxy server`、`sdk/go` | **参考实现**：`sdk/go` 内嵌参考网关 `internal/lmbp/gateway`，与 `localsubproxy server` 是同一份代码。行为以它为准。 |
| Node.js | `sdk/node`（`mock/node` 是它的命令行外壳） | **独立重新实现**，与 Go 不共享代码，存在真实的行为差异。例如：JWKS 返回 `application/json`（Go 为 `application/jwk-set+json`）；`Store` 接口是同步的、没有 `context`，Go 的接口返回 `error`；HTTP/WebSocket 栈、超时与默认参数的实现细节不同。 |
| Python | `sdk/python`（`mock/python` 是它的命令行外壳） | **独立重新实现**（纯 ASGI，自带 HTTP/SSE 渲染），同样与 Go 存在真实差异：`Store` 是同步 Protocol；WebSocket 依赖 uvicorn 的 `websockets` 实现；时间参数以秒为单位，等等。 |
| Rust | `mock/rust`（模拟网关）、`spec/examples/rust`（协议示例骨架） | `mock/rust` 是 Node 引擎的**模拟网关移植**：通过 `lmbp-conformance`、连接器端到端、官方客户端与聊天页 E2E，但状态只在内存（无持久化存储）、HTTP/WebSocket 为手写最小实现，**不是 SDK，不面向生产**。`spec/examples/rust` 仍只是 JOSE/E2EE/流控骨架。 |
| Java | `sdk/java`（SDK：`lmbp-core` + `lmbp-spring-boot-starter`，示例 `examples/spring-boot-demo`）、`mock/java`（模拟网关） | `sdk/java` 的引擎是从 `mock/java`（Node 引擎的逐段移植）**抽取并重构**而来，不与 Go 共享代码，存在真实差异：`Store` 接口是同步的、由引擎单锁串行调用；Spring 适配基于 Servlet（Tomcat 已验证），容器只在写入失败时才能发现客户端断开，所以 SSE 空闲保活默认 1 秒；非流式请求在等待期间发现不了断开；`JdbcStore` 只在 SQLite 上验证过，单实例、任务事件不清理。演示应用通过 `lmbp-conformance`（内存与 SQLite 两种存储）、连接器端到端与官方 openai/anthropic 客户端。需要 JDK 21、Spring Boot 3.5。`mock/java` 仍是只依赖 JDK 的模拟网关，**不面向生产**；实现指南里的 Java 片段（Jackson 写法）仍是未编译的示意代码。 |

> **通过共享 Schema、测试向量（含 `spec/testvectors/jose.json` 的负例）和 `lmbp-conformance`，不等于符合规范。** 它们只说明：实现在被检查的那些点上，与参考实现和彼此一致。黑盒检查覆盖不到的路径，例如存储写入失败、并发竞态、资源上限和错误恢复，仍然可能不同。2026-10 审查发现的问题（包括 ACK 先于持久化、排队期间重复请求被判为 `OUTCOME_UNKNOWN`）在修复前都“全部通过”了当时的一致性测试。全量矩阵见 [`scripts/verify-all.sh`](../scripts/verify-all.sh)（14 步，任何失败或跳过都判失败）。

## 常用命令

- Go 参考网关：`localsubproxy server -issuer https://<网关公网地址> [-tenant-profiles profiles.json]`。`profiles.json` 配置每个租户在桌面端同意页上显示的身份，格式见 [mock/tenant-profiles.example.json](../mock/tenant-profiles.example.json)。
- 黑盒一致性测试（104 项）：`LMBP_TENANT_KEY=... go run ./cmd/lmbp-conformance -gateway <url> -tenant <id>`。
- 契约与测试向量校验：`spec/validate.sh`。
