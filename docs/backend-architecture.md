# 本地后端架构

适配层在 `internal/backend/`。`connector` 负责方案持久化、授权、任务准入、并发和审计；后端负责配置校验、模型发现、健康探测、请求映射和执行。connector、命令行和桌面端都不按类型名分支，类型的一切描述都在注册表里。

```text
internal/backend/
  backend.go       Backend 接口、Definition（类型描述）、依赖
  registry.go      注册表：查找、校验、能力、并发、推理强度
  catalog.go       模型级推理强度（ModelCatalog、CheckEffort）
  stop.go          CLI 停止原因归一化与错误分类
  cli.go           CLI 文本映射、健康探测、登录状态
  request.go / emitter.go / errors.go   请求检查、输出分片、LMBP 错误
  builtin/         唯一注册点（顺序即展示顺序）
  httpapi/         HTTP 后端共用的 URL 校验、模型列表、探测
  cliruntime/      CLI 路径、argv、环境白名单、工作目录、进程树
  jsonrpc/         Codex app-server 与 Grok 模型目录共用的 JSON-RPC 帧
  clitest/         所有 CLI 后端共用的一致性测试套件
  codex/ grokbuild/ antigravity/ openai/ anthropic/ ollama/
```

依赖方向：`connector → builtin → 具体后端 → backend`。根包不导入实现；实现之间不互相调用，也不导入 connector。公共运行时只管传输和进程生命周期，不解析厂商事件。

## 类型描述

`Definition` 给出类型名、展示名、命令行名和别名、默认可执行文件、登录命令、是否上云、是否需要 API Key、默认地址、能力、并发上限和允许的推理强度。

- 命令行 `avora scheme add --type` 的类型、别名和帮助文本都取自注册表。
- 原生 `backends` 动作输出这些描述，桌面端据此绘制服务列表和表单；不认识的类型只读显示，不能编辑。
- 原生 `login_status` 动作对 Codex、Grok、Antigravity 返回 `ready` / `required` / `unknown` 和登录命令。凭据始终由官方 CLI 管理。
- 推理强度按类型限制：OpenAI 兼容 `off|low|medium|high`；Anthropic、Ollama 只有默认；Codex `off|none|minimal|low|medium|high|xhigh`（`off` 存为 `none`）；Grok `none … max`；Antigravity `none … ultra`。`default` 存为空值，表示不覆盖。

## CLI 后端

三个 CLI 后端只做纯文本推理，单方案并发 1，不支持工具、图片、音频和采样参数，不支持的请求在启动进程前返回 `CAPABILITY_UNSUPPORTED`。消息合并为带角色标签的文本。每个任务一个进程、一个随后删除的私有空工作区；提示词经私有文件或 stdin 传递，不出现在 argv。环境变量只传白名单。

进程树：Unix 下 CLI 自成进程组，只在回收前向进程组发信号（避免 PID 复用误杀）；Windows 下挂起启动、加入带 `KILL_ON_JOB_CLOSE` 的 Job Object 再恢复，结束作业即结束整棵树，包括已脱离父进程的孙进程。取消、超时、回调失败、工具调用都会结束整棵树。

停止原因统一归一化（`EndTurn`、`end_turn`、`END-TURN` 等价）：`end_turn` → `stop`，`max_tokens` → `length`，`cancelled` → `CANCELLED`，其余为上游错误。CLI 报告的限流、配额、未登录错误分别映射为可重试的 `PROVIDER_RATE_LIMITED`、`QUOTA_EXHAUSTED` 及 `AUTH_REQUIRED`。

### Codex

`codex app-server`，关闭 shell、应用、hooks、多代理、远程插件、网页搜索和 MCP；线程只读、不审批。App Server 的请求一律拒绝。模型目录没有给出 `supportedReasoningEfforts` 时，所选强度原样透传；目录给出时只允许其中的值。

### Grok Build

采用 headless 模式：`grok -p` 等价的 `--prompt-file … --output-format streaming-json`，不走 ACP 会话。理由：headless 是官方面向脚本的稳定接口，一次一进程、无审批往返，便于限制工具。ACP（`grok agent stdio`）只用于读取模型目录，且加 `--sandbox read-only`，不发送提示词。

- 临时 `GROK_HOME`，`GROK_AUTH_PATH` 指向官方凭据文件，不读取或复制凭据。禁用自动更新、hooks、记忆和外部配置发现。
- 参数：私有 agent、`--tools read_file --disallowed-tools read_file,search_tool,use_tool`（得到空工具集，且排除 MCP 入口）、`--no-plan --no-subagents --disable-web-search --permission-mode dontAsk --sandbox read-only --max-turns 1 --verbatim`。
- 流必须先出现 `available_commands` 且 `tools` 为空列表，之后的 `text` 与 `end` 才被接受；缺少清单或清单非空都返回 `POLICY_DENIED`。`tool_call`、`tool_call_update`、`plan` 一律中止任务。
- 用量：`prompt = input_tokens + cache_read_input_tokens + cache_creation_input_tokens`。

### Antigravity

`agy --agent avora-inference --disable-slash-commands --sandbox --input-format stream-json --output-format stream-json`。私有 agent 只保留 `finish`。`init` 必须选中该 agent 且工作目录正确，之前的任何输出都是 `POLICY_DENIED`；写入一条请求后关闭 stdin。临时文件目录（`TMPDIR`，Windows 为 `TEMP`/`TMP`）指向私有工作区。

隔离的限制：

- `HOME` 不能替换。凭据、钥匙串和设置在 `~/.gemini` 下，替换后无法登录。agy 可能在那里保存会话历史，Avora 无法改变。
- agy 没有独立配置目录选项。启动前检查全局 hooks、MCP、插件、规则、技能等，发现即 `POLICY_DENIED` 并给出路径；不修改用户配置。
- 工具步骤在执行后才报告。私有 agent 不提供工具，`--sandbox` 再限制终端；若仍出现工具步骤，任务立即中止，但已执行的动作无法撤回。

这些限制依赖官方 CLI 的语义，不是操作系统级隔离；CLI 升级后需重新验证。

## 添加后端

1. 在 `internal/backend/<name>/` 实现协议和 `Definition()`，在 `builtin` 注册。
2. CLI 后端实现 `clitest.Wire` 并运行 `clitest.Run`：流式文本、停止原因、用量、取消/超时及进程树清理、回调失败、终态后数据、截断、未知事件、工具调用中止、环境泄漏、工作区删除。协议无法表达的场景在 `NotApplicable` 中写明原因（记录日志，不用 t.Skip，verify-all 视跳过为失败）。
3. 有官方样例时放入 `testdata/` 并注明来源；非真实录制的样本要标明。
4. 运行 `go test ./internal/native -run TestDesktopBackendsFixture` 更新桌面测试夹具（`AVORA_UPDATE_FIXTURES=1`）。

## 验证

```sh
go vet ./...
go test -short ./...
go test -short -race ./internal/backend/... ./internal/connector
cd desktop && flutter analyze && flutter test
```

CLI 测试用测试程序自身充当 CLI，不需要账号、已安装的 CLI 或网络。`grokbuild/testdata`、`antigravity/testdata` 中的样本取自官方文档，并非真实录制；真实验证需在已登录的本机单独进行。
