# lmbp-server：LMBP 服务端 Node.js SDK

让你的 SaaS 后端使用**用户自己电脑上的模型**（经 LocalSubProxy 桌面端）。业务代码只做两件事：

1. **配对**：展示配对链接 / 二维码，批准用户在桌面端看到的配对码；
2. **调用模型**：标准 OpenAI（`/v1/chat/completions`、`/v1/models`）或 Anthropic（`/v1/messages`）请求，直接用官方 `openai` / `@anthropic-ai/sdk`，或用 SDK 的进程内客户端。

WebSocket 会话、JOSE、续传、流控、大图分块、E2EE 透传全部在 SDK 内部。TypeScript 编写，ESM，自带类型声明。需要 Node ≥ 18.17。

## 安装

```bash
npm install lmbp-server            # 仓库内：npm install ../../sdk/node
npm install better-sqlite3         # 可选：SqliteStore
```

## 快速开始（Express）

```js
import http from 'node:http';
import express from 'express';
import { LMBPServer, SqliteStore, PairingError } from 'lmbp-server';

const lsp = new LMBPServer({
  tenants: [{ id: 'acme', apiKey: process.env.TENANT_API_KEY, profile: { display_name: 'Acme CRM' } }],
  issuer: 'https://saas.example.com',    // 留空为开发模式（取请求 Host）
  store: new SqliteStore('lmbp.db'),     // 默认 MemoryStore
});

const app = express();
app.use(lsp.middleware());               // 协议路径 + /v1/*，其余 next()
app.use(express.json());
// 1. 配对
app.get('/app/pair', (req, res) => res.json(lsp.pairing.create('acme')));   // { link, qrSVG, qrDataURL }
app.post('/app/pair/approve', (req, res) => res.json(lsp.pairing.approve('acme', req.user.id, req.body.user_code)));
// 2. 调用模型
app.post('/app/ask', async (req, res) => {
  const c = lsp.client('acme', req.user.id);
  const [m] = await c.models();
  const msg = await c.messages.create({ model: m.id, max_tokens: 512, messages: [{ role: 'user', content: req.body.prompt }] });
  res.json(msg);
});

const server = http.createServer(app);
lsp.attach(server);                      // /lmbp/v1/connect 的 WebSocket 升级
server.listen(3000);
```

完整示例：[`examples/express/server.mjs`](../../examples/express/server.mjs)。

其他框架：

- 原生 `node:http`：`http.createServer(lsp.handler)` 后 `lsp.attach(server)`，或直接 `await lsp.listen(8080)`。
- Fastify：`await app.register(lsp.fastifyPlugin)`（自动注册路由和 WebSocket 升级）。
- Koa / Connect 等：使用 `lsp.middleware()`，或按 `PATHS` 显式转发到 `lsp.handler`。

## API

| 分类 | API |
|---|---|
| 构造 | `new LMBPServer({ tenants, issuer, store, signingSeed, maxConcurrency, chatUIDir, log, tuning })` |
| 挂载 | `handler`、`middleware()`、`fastifyPlugin`、`attach(server)`、`listen(port, host)`、`close()`、`PATHS` |
| 密钥 | `keys.kid()`、`keys.publicJWK()`、`keys.jwks()`；`LMBPServer.generateSigningSeed()`、`LMBPServer.generateTenantKey()`。未传 `signingSeed` 时种子保存在 Store 中 |
| 配对 | `pairing.link(tenant, issuer?)`、`pairing.create(...) → { link, qrSVG, qrDataURL }`、`pairing.lookup(tenant, code)`、`pairing.approve(tenant, user, code, { jkt })`、`pairing.deny(...)`、`pairing.listDevices(tenant, user)`、`pairing.revokeDevice(tenant, user, id)`；失败抛 `PairingError`（`code`：`BAD_REQUEST` / `NOT_FOUND` / `FORBIDDEN`） |
| 进程内客户端 | `client(tenant, user)` → `models()`、`chat.completions.create/stream`、`messages.create/stream`（`stream` 为 async iterable，支持 `signal` 取消、`idempotencyKey`）；失败抛 `LMBPAPIError{status, code}` |
| 存储 | `MemoryStore`、`SqliteStore(path \| db)`（better-sqlite3）；自定义存储实现 `Store` 接口（同步方法） |
| 工具 | `anthropic.*`：Anthropic ↔ OpenAI 请求/响应转换函数 |

## 官方客户端原样可用

```js
const openai = new OpenAI({ baseURL: 'https://saas.example.com/v1', apiKey: TENANT_API_KEY, defaultHeaders: { 'X-End-User': userId } });
const claude = new Anthropic({ baseURL: 'https://saas.example.com', apiKey: TENANT_API_KEY, defaultHeaders: { 'X-End-User': userId } });
```

模型 ID 形如 `dev_xxx/qwen3-8b`，从 `models()` 获取。

## 测试

```bash
cd sdk/node && npm test                              # SDK 单元测试（协议、配对、存储、框架、进程内客户端）
scripts/mock-interop.sh node node-sqlite express     # 73 项一致性 + 连接器端到端 + 官方客户端
```

## 限制

- 尚无 Redis 存储（Store 接口为同步，Redis 客户端为异步）；存储为单实例。
- `/v1/messages` 不支持 E2EE。
- Anthropic 模型列表的 `display_name`、`created_at` 为占位值。
