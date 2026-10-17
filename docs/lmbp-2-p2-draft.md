# LMBP 2.x 后续方向（P2 草案，非规范）

> **状态：非规范草案。** 本文不构成 LMBP 2.0 的任何要求，实现不得据此声称一致性。规范以 [`lmbp-spec.md`](lmbp-spec.md) 为准。本文只记录已知缺口和倾向的方案，供下一个次版本讨论。

## 1. 网关签名密钥轮换

**现状**：§5.1 要求轮换期间 JWKS 同时发布新旧公钥，但没有规定设备何时接受新密钥。设备配对时固定了网关公钥（§12 第 3 条）。

**倾向方案**：
- 网关先在 JWKS 中加入新密钥，至少保留一个访问令牌 TTL，再开始用它签名。
- 设备遇到未知 `kid` 时，从**同源** `jwks_uri` 重新拉取 JWKS；新密钥必须由旧密钥签名的“轮换声明”背书（`typ: lmbp-key-rotation+jwt`，载荷 `{iss, new_jwk, iat, exp}`），否则拒绝。
- 元数据增加 `key_rotation_supported`；`signed_metadata` 的 `kid` 可以是新旧任一密钥。

**待定**：旧密钥泄露后的紧急撤销路径（没有带外渠道时，设备无法区分正常轮换和攻击者轮换）。

## 2. 同意与作用域

**现状**：§19.2 的作用域只用于同意页展示，本机策略以本地配置为准；令牌不携带作用域。

**倾向方案**：
- 访问令牌增加 `scope` 声明，取值为用户在同意页确认的作用域子集。
- 网关按 `scope` 拒绝越权请求（例如未授予 `lmbp.tools` 时带 `tools` 的请求返回 403 `FORBIDDEN`），设备仍按本机策略二次检查。
- 用户在本机撤回某个作用域时，设备通过 `models.update` 增加 `scopes` 字段通知网关。

## 3. 严格模式

**现状**：2.0 已经要求拒绝而不是截断，但仍有若干“忽略”规则（§4.1 未知成员、§16.5 未列出的请求字段）。

**倾向方案**：元数据增加 `strict: true` 声明。严格模式下：
- 帧和 `hello` 中的未知成员返回 `BAD_REQUEST`（`x-` 前缀除外）；
- §16.5 中“忽略”的请求字段改为 400，列出字段名；
- 重复 JSON 成员名必须拒绝（§4.1 目前是 SHOULD）。

严格模式只用于测试和高安全部署，不作为默认值，以免破坏前向兼容。

## 4. RFC 化与 IANA 注册

需要注册或申请的标识：

| 项目 | 注册表 | 当前值 |
|---|---|---|
| WebSocket 子协议 | IANA WebSocket Subprotocol Name Registry（RFC 6455 §11.5） | `lmbp` |
| URI scheme | IANA URI Schemes（RFC 7595，先 provisional） | `lmbp` |
| well-known URI | IANA Well-Known URIs（RFC 8615） | `lmbp` |
| JWS `typ` / 媒体类型 | IANA Media Types（`application/lmbp-task+jws` 等） | 见 §5.2 |
| Problem type | 无需注册，使用 `https://localsubproxy.dev/problems/…` | — |

RFC 化路径：先把规范改写为 Internet-Draft（xml2rfc v3），英文为准，中文作为非规范译本；需求 ID（§13.2）保留为附录。

## 5. 许可与治理

- **许可**：规范文本倾向 CC BY 4.0，测试向量与 Schema 倾向 CC0，参考实现保持仓库现有许可。需要在仓库根目录明确列出。
- **变更流程**：规范变更以 PR 提出，必须同时修改 Schema、测试向量、conformance 检查和 §13.2 需求表（`spec/validate.sh` 已校验后两者一致）；不兼容变更递增主版本和子协议名（§11）。
- **版本支持**：每个主版本发布后，前一主版本的参考实现至少维护 12 个月的安全修复。
- **决策**：在出现第二个独立实现方之前，由维护者决定；之后成立由各实现方组成的评审组，规范 MUST 级变更需要至少两个独立实现通过 conformance。
