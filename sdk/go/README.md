# lsp2server：LSP2 服务端 Go SDK

让你的 SaaS 后端使用**用户自己电脑上的模型**（经 LocalSubProxy 桌面端）。业务代码只做两件事：

1. **配对**：给用户展示配对链接 / 二维码，再批准用户在桌面端看到的配对码；
2. **调用模型**：标准的 OpenAI（`/v1/chat/completions`、`/v1/models`）或 Anthropic（`/v1/messages`）请求，可以直接用官方 `openai-go` / `anthropic-sdk-go`，也可以用 SDK 自带的进程内客户端。

WebSocket 会话、JOSE 签名、令牌刷新、seq/ack/续传、信用流控、大图分块、E2EE 透传都封装在 SDK 内部（内嵌参考网关 `internal/lsp2/gateway`，与 `localsubproxy server` 同一份代码）。

## 安装

```bash
go get github.com/tyro668/LocalSubProxy/sdk/go
```

```go
import lsp2server "github.com/tyro668/LocalSubProxy/sdk/go"
```

需要 Go ≥ 1.22。SQLite 存储使用纯 Go 驱动，无需 cgo。

## 快速开始（net/http）

```go
srv, err := lsp2server.New(ctx, lsp2server.Options{
    Tenants: []lsp2server.Tenant{{ID: "acme", APIKey: os.Getenv("TENANT_API_KEY"),
        Profile: lsp2server.TenantProfile{DisplayName: "Acme CRM", RequestedScopes: []string{"lsp2.chat"}}}},
    Issuer: "https://saas.example.com",          // 网关对外 https origin；留空为开发模式
    Store:  must(lsp2server.OpenSQLiteStore("lsp2.db", encKey32)), // 默认内存
})
defer srv.Close()

mux := http.NewServeMux()               // 你自己的路由
// 1. 配对
mux.HandleFunc("GET /app/pair", func(w http.ResponseWriter, r *http.Request) {
    info, _ := srv.Pairing.Create("acme", "")  // info.Link / info.QRSVG / info.QRPNG
    json.NewEncoder(w).Encode(info)
})
mux.HandleFunc("POST /app/pair/approve", func(w http.ResponseWriter, r *http.Request) {
    res, err := srv.Pairing.Approve(r.Context(), "acme", currentUser(r), r.FormValue("user_code"), "")
    ...
})
// 2. 调用模型（进程内，无网络开销）
c, _ := srv.Client("acme", currentUser(r))
models, _ := c.Models(ctx)
m, _ := c.Messages(ctx, lsp2server.MessagesRequest{Model: models[0].ID, MaxTokens: 512,
    Messages: []lsp2server.AnthropicMessage{{Role: "user", Content: "你好"}}})

http.ListenAndServe(":3000", srv.Middleware(mux)) // 协议路径由 SDK 处理，其余交给 mux
```

完整示例：[`examples/go-nethttp`](../../examples/go-nethttp/main.go)（独立 Go 模块，演示外部项目引用本 SDK）。

## API

| 分类 | API |
|---|---|
| 构造 | `New(ctx, Options{Tenants, Issuer, Store, SigningKey, MaxConcurrency, Logger, Retention})`；`ctx` 结束时停止过期任务清理 |
| 挂载 | `Handler()`（挂在根路径）、`Middleware(next)`（协议路径由 SDK 处理，其余交给 `next`）、`Close()` |
| 密钥 | `Keys.KID()`、`Keys.PublicJWK()`、`Keys.JWKS()`；`GenerateSigningKey()`、`GenerateTenantKey()`。未传 `SigningKey` 时，签名种子保存在 Store 中（实现了 `SecretStore` 的存储），首次启动生成 |
| 配对 | `Pairing.Link(tenant, issuer)`、`Pairing.Create(...) → PairingInfo{Link, QRSVG, QRPNG}`、`Pairing.Lookup(ctx, tenant, code) → PendingPairing{DeviceName, JKT, ...}`、`Pairing.Approve(ctx, tenant, user, code, jkt)`、`Pairing.Deny(...)`、`Pairing.ListDevices(ctx, tenant, user) → []DeviceInfo{Online, Models, ...}`、`Pairing.RevokeDevice(ctx, tenant, user, id)` |
| 错误 | `ErrCodeInvalid`、`ErrCodeDenied`、`ErrJKTMismatch`、`ErrDeviceUnknown`、`ErrUnknownTenant`；模型调用返回 `*APIError{Status, Code, Message}` |
| 进程内客户端 | `Client(tenant, user)` → `Models`、`ChatCompletion`、`ChatCompletionStream`、`Messages`、`MessagesStream`；流为 `*Stream[T]`：`for s.Next() { s.Current() }; s.Err()`，`Close()` 会取消任务 |
| 传输层 | `Transport()`：进程内 `http.RoundTripper`，可交给任意 HTTP 客户端（包括官方 SDK 的 `option.WithHTTPClient`） |
| 存储 | `NewMemoryStore()`、`OpenSQLiteStore(path, key32)`（key 非空时 AES-256-GCM 加密提示词、输出与密钥）；自定义存储实现 `Store`（可选 `SecretStore`） |
| 协议类型 | `ChatMessage`、`Content`、`Text()`、`Tool`、`ToolCall`、`Usage`、`JWK`、`TenantProfile` 等（别名，外部模块无需引用 `internal/`） |

`Approve` 的 `jkt` 参数可选：传入 `Lookup` 返回的设备密钥指纹，可防止配对码被换成其他设备。

## 官方客户端原样可用

```go
oai := openai.NewClient(option.WithBaseURL("https://saas.example.com/v1/"), option.WithAPIKey(tenantKey),
    option.WithHeader("X-End-User", userID))
claude := anthropic.NewClient(option.WithBaseURL("https://saas.example.com/"), option.WithAPIKey(tenantKey))
// 同进程时：option.WithHTTPClient(&http.Client{Transport: srv.Transport()})
```

模型 ID 形如 `dev_xxx/qwen3-8b`（设备 ID / 模型名），从 `Models` 获取。

## 测试

```bash
go test ./sdk/go                         # 真实桌面连接器配对 + memory/sqlite + 中间件 + 进程内客户端
scripts/mock-interop.sh sdk-go sdk-go-sqlite   # 73 项一致性 + 连接器端到端 + 官方 openai/anthropic 客户端
```

## 限制

- 存储是单实例的：多副本部署需要共享会话路由（同一用户的设备 WebSocket 与请求须落到同一实例）。
- `/v1/messages` 不支持 E2EE（`lsp2_e2ee`），需要端到端加密时用 `/v1/chat/completions`。
- Anthropic 模型列表的 `display_name`、`created_at` 为占位值。
