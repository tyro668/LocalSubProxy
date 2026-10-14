# Rust LMBP 模拟网关（`mock/rust`）

LMBP 网关（[docs/lmbp-spec.md](../../docs/lmbp-spec.md)）的 Rust 版模拟实现，功能与 Java 版（[../java](../java/README.md)）及 Node/Python 模拟网关对齐：配对与同意（含 `CONSENT_DENIED`）、iss/aud 身份绑定、JWKS 与 `/.well-known/lmbp`（含 `signed_metadata`）、WebSocket 子协议 `lmbp`、按字节的流控、断线续传、`Idempotency-Key`、E2EE 透传、Problem Details 错误、帧大小与 Schema 限制、单一终态裁决、按模型的参数能力表、OpenAI 与 Anthropic 接口，以及共用的聊天页（`mock/web`）。

HTTP 与 WebSocket 用标准库线程手写，没有异步运行时和 Web 框架。依赖只有：`serde_json`（保持键顺序）、`ed25519-dalek`、`sha2`、`sha1`、`base64`、`getrandom`、`regex`、`parking_lot`（版本锁定在 `Cargo.lock`）。

用于互通测试，**不是 Rust SDK，也不面向生产**。

## 运行

```bash
(cd mock/rust && cargo build --release)              # → mock/rust/target/release/lmbp-mock
export LSP_TENANT_KEY_ACME=$(openssl rand -hex 24)
export LMBP_SIGNING_SEED=$(openssl rand -hex 32)      # 可选
mock/rust/target/release/lmbp-mock --listen 127.0.0.1:8080 --tenant-profiles mock/tenant-profiles.example.json
```

一键启动：`scripts/mock-start.sh rust`。参数与 Java/Node/Python 版一致（`--listen`、`--issuer`、`--tenant ID:ENV`、`--tenant-profiles`、`--signing-key-env`、`--max-concurrency`、`--web-dir`、`--no-chat-ui`）。

## 与 Node/Python 模拟网关的差别（如实说明）

- **状态只在内存中**，不支持 `--db`；签名密钥可用 `LMBP_SIGNING_SEED` 固定。
- 最小 HTTP/1.1（每连接一个线程、一个请求，`Connection: close`）和 RFC 6455 WebSocket；一把全局锁模拟 Node 的单线程语义，等待时释放，套接字写入在锁外。
- JSON 数字输出按 JavaScript 规则格式化（整数值的浮点数输出为整数），极端浮点数的格式可能与 `JSON.stringify` 有细微差别；解析用 serde_json，它拒绝孤立的 UTF-16 代理项（JavaScript 接受）。
- 审计日志与 token 用量统计在连接器 / 桌面端，网关侧没有（Node/Python 模拟网关也没有）。

## 测试

```bash
(cd mock/rust && ../../scripts/strict-test.py cargo --release)   # 自测：JOSE 共享向量、JSON、origin、参数、聊天页
scripts/mock-interop.sh rust                                     # conformance + 连接器端到端 + 官方客户端
scripts/chat-ui-e2e.sh rust
```

截至提交时：`lmbp-conformance` 103 项全部通过、无跳过（同样不等于符合规范）。
