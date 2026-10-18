# Java LMBP 模拟网关（`mock/java`）

LMBP 网关（[docs/lmbp-spec.md](../../docs/lmbp-spec.md)）的 Java 版模拟实现，**只依赖 JDK 21**（`javac`、`jar`、`java`），没有任何第三方库。它是 Node SDK 引擎（`sdk/node/src/engine/gateway.js`）的逐段移植：配对与同意（含 `CONSENT_DENIED`）、iss/aud 身份绑定、JWKS 与 `/.well-known/lmbp`（含 `signed_metadata`）、WebSocket 子协议 `lmbp`、按字节的流控、断线续传、`Idempotency-Key`、E2EE 透传、Problem Details 错误、帧大小与 Schema 限制、单一终态裁决、按模型的参数能力表、OpenAI 与 Anthropic 接口，以及共用的聊天页（`mock/web`）。

用来给连接器、SaaS 后端或其他语言的实现做互通测试；**不是面向生产的网关**。生产用 Java SDK 见 [`sdk/java`](../../sdk/java)（从本模拟网关抽取并重构）。

## 运行

```bash
mock/java/build.sh                                   # → mock/java/build/lmbp-mock.jar
export LMBP_TENANT_KEY_ACME=$(openssl rand -hex 24)    # 租户 API Key（≥ 32 字符）
export LMBP_SIGNING_SEED=$(openssl rand -hex 32)      # 可选：固定网关签名密钥
java -jar mock/java/build/lmbp-mock.jar --listen 127.0.0.1:8080 --tenant-profiles mock/tenant-profiles.example.json
```

一键启动（假模型 + 网关 + 聊天页）：`scripts/mock-start.sh java`。

参数与 Node/Python 模拟网关一致：`--listen`、`--issuer`、`--tenant ID:ENV`（可重复）、`--tenant-profiles`、`--signing-key-env`（默认 `LMBP_SIGNING_SEED`）、`--max-concurrency`、`--web-dir`（默认 `./mock/web`，其次 `../web`）、`--no-chat-ui`。

## 与 Node/Python 模拟网关的差别（如实说明）

- **状态只在内存中**：不支持 `--db`（传入会直接报错退出）。重启后配对、设备和任务都会丢失；只有签名密钥可以通过 `LMBP_SIGNING_SEED` 固定。
- HTTP/1.1 与 RFC 6455 WebSocket 是手写的最小实现（每连接一个虚拟线程，每个连接只处理一个请求，`Connection: close`），不是通用服务器。
- 并发模型：用一把全局锁模拟 Node 的单线程语义；阻塞等待会释放锁，套接字写入在锁外进行。
- 审计日志与 token 用量统计在连接器 / 桌面端，网关侧没有（Node/Python 模拟网关也没有）。

## 测试

```bash
mock/java/test.sh            # 自测：JOSE 共享向量（含全部负例）、JSON、origin、参数校验、聊天页 CSP/关闭
scripts/mock-interop.sh java # lmbp-conformance + Go 连接器端到端 + 官方 openai/anthropic 客户端
scripts/chat-ui-e2e.sh java  # 无头浏览器跑聊天页
```

截至提交时：`lmbp-conformance` 全部通过、无跳过。通过一致性测试只说明在被检查的点上与参考实现一致，不等于符合规范。
