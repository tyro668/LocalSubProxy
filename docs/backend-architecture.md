# 本地后端架构

适配层统一放在 `internal/backend/`。`connector` 处理方案持久化、授权、任务准入、并发和审计；后端负责配置校验、模型发现、健康探测、请求映射和执行。新增提供方不需要在 connector 中增加类型分支。

```text
internal/backend/
  backend.go       Backend 接口、Definition 注册描述、依赖
  types.go         配置和健康状态
  registry.go      显式注册、工厂、能力和并发声明
  request.go       按已声明能力检查请求
  emitter.go       输出分片、UTF-8 边界、总量限制
  errors.go        统一映射到 LMBP 错误
  cli.go           CLI 纯文本映射及探测记账
  builtin/         组装所有内置后端（唯一集中注册点）
  httpapi/         HTTP 后端共用的 URL、探测和数据处理
  cliruntime/      CLI 解析路径、argv、环境、工作目录、进程生命周期
  codex/           Codex app-server 及原有登录流程
  grokbuild/       Grok Build streaming-json
  antigravity/     Antigravity CLI stream-json
  openai/          OpenAI 兼容接口
  anthropic/       Anthropic 兼容接口
  ollama/          Ollama HTTP
```

依赖方向：`connector → builtin → 具体后端 → backend`。`backend` 根包不导入实现；具体实现不导入 connector，也不互相调用。公共运行时只处理传输和资源生命周期，不解析厂商事件。

`Backend` 接口仅包含 `Models`、`Probe`、`Execute`。可选的 `ModelCatalog` 接口返回模型级思考档位、默认值，以及档位对应的模型变体；不提供元数据的 HTTP 后端仍只需实现原接口。`provider_catalog` 是新增的本机查询，原 `provider_models` / `codex_models` 字符串列表 API 保留。登录方式各提供方不同，Codex 现有登录 API 保留；Grok 和 Antigravity 由用户先在官方 CLI 登录。避免把某一家 CLI 的会话、审批、登录协议塞进公共接口。

## 兼容性

- `connector.AdapterConfig`、`AdapterHealth` 保留为公共类型别名；加密配置格式、方案 ID、发布模型 ID、授权和审计路径保持不变。
- 原有四种 `type` 值保持不变，Codex 的 `codex_bin` 字段继续读写。新 CLI 使用 `executable`，新增类型为 `grok_build`、`antigravity_cli`。
- 请求直接使用现有 LMBP 结构，保留 HTTP 后端对工具、图片、音频、采样参数和用量的原有映射；不另建一套重复 DTO。
- 缺失可执行文件不会导致已保存方案消失；方案仍可编辑、禁用和删除，重新启用时校验运行时。
- Flutter 的 `backend_catalog.dart` 只维护展示名称、路径默认值和登录提示，运行能力以 Go 注册表为准。

## CLI 执行边界

三个 CLI 后端都只声明纯文本能力和单方案并发 1；不支持 `tools`、图片、音频、`temperature`、`top_p`、`stop`、请求级 `max_tokens`。不支持的请求在启动进程前返回 `CAPABILITY_UNSUPPORTED`。消息合并为带角色标签的文本，不等同于提供方原生角色消息。

Grok 和 Antigravity 每个任务启动独立进程、使用临时空仓库，不复用其他请求的会话。提示词通过私有文件或 stdin 传递，argv 不包含用户提示词。取消、超时、输出回调失败都会停止进程；临时目录随调用清理。输出使用有界逐行解析，只有明确的成功终态及成功退出才返回成功。

- Grok 使用临时 `GROK_HOME`，`GROK_AUTH_PATH` 指向官方 CLI 现有凭据文件；不读取或复制凭据内容。禁用自动更新、记忆、外部配置发现和托管 MCP，设置受限工具列表并显式排除 MCP 工具入口，同时验证流中公布的工具集合为空。
- Antigravity 使用临时自定义 agent，仅保留内部 `finish` 操作，禁用命令、技能、插件和 MCP。当前 CLI 没有独立全局配置目录选项，因此启动前检查用户全局自定义项；发现 hooks、MCP、插件、规则等会拒绝启动并指出配置路径，不修改用户配置。空配置允许使用。
- 这些限制依赖官方 CLI 的受限运行语义，不构成操作系统级隔离；CLI 升级后应重新验证。版本能力检查失败、未知协议事件、异常终态或输出冲突都返回错误。

## 添加后端

1. 在 `internal/backend/<name>/` 实现协议转换及 `Definition()`。定义自己的配置校验、能力和并发上限。
2. 在 `builtin/registry.go` 注册工厂。共性只在确有多个使用方时放入 `httpapi` 或 `cliruntime`。
3. 补充真实协议格式的进程/HTTP 测试，覆盖流式文本、终态、用量、取消、失败以及输出回调错误；再补配置往返测试。
4. 在 Flutter 展示目录、命令行类型别名和用户文档中加入入口。不要把厂商事件或认证细节泄漏到 connector。

## 验证

```sh
go test ./...
go test -race ./internal/backend/... ./internal/connector ./internal/native
go vet ./...
cd desktop
flutter analyze --no-pub
flutter test --no-pub
```

CLI 测试使用测试程序自身模拟子进程，不依赖用户账号、已安装的 CLI 或网络。真实模型验证应单独在已登录的本机执行，并检查模型发现、流式结果、终态以及工具限制。

## 思考强度

- 公共 `ModelInfo` / `EffortOption` 只描述能力，厂商协议解析留在各后端。Codex 读取 `model/list`（含分页）；Grok 读取 ACP `session/new` 的每模型元数据，不发送推理请求或修改用户设置。
- Antigravity 只在实时目录中有多个同组、标签匹配的档位变体时提供选择。选择档位直接解析为现有变体 ID，不叠加冲突的 `--effort`；单个带档位后缀的模型不推断为可调。
- 空值或 `default` 表示不覆盖。显式档位在连接测试和执行前校验；能力未知或不支持时返回 `CAPABILITY_UNSUPPORTED`，不静默降级。CLI 上报的档位仍可能被上游服务拒绝，这类错误原样返回。
- 旧 `off` 保留原有语义：Grok/Antigravity 归一为空值；Codex 归一为 `none`，仅在模型支持时可执行。HTTP 后端保持原映射。
- Flutter 只根据目录展示档位，`none` 才显示「关闭」。异步结果与方案、服务、可执行路径和查询序号绑定；模型变体变化不改变已经确定的方案名称。
