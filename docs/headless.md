# 无界面模式（headless）

`avora` 命令行可以不打开桌面应用，单独运行和桌面端**同一个内核**：多个 SaaS 连接、六种模型方案、模型授权、撤销、用量与审计。支持 Linux、macOS、Windows（amd64 / arm64）。

下载：Releases 中的 `avora-cli-<版本>-<系统>-<架构>.tar.gz`（Windows 为 `.zip`）。从源码构建：`go build ./cmd/avora`，或 `scripts/build-cli.sh` 一次构建全部六个平台。

## 工作方式

- `avora serve` 在前台运行内核，Ctrl-C / SIGTERM 退出。其余命令都是它的客户端，必须先启动 `serve`。
- 数据目录与桌面端相同，格式也相同（`identity.json`、`device_key.enc`、`adapters.enc`、`audit.db`）：Linux `~/.config/avora`，macOS `~/Library/Application Support/avora`，Windows `%AppData%\avora`。所有命令都可用 `--data-dir` 指定其他目录。
- 同一数据目录同时只能有一个内核（`kernel.lock`）：桌面应用和 `serve` 不能同时运行，后启动的会报 `data directory in use` 并退出。
- 控制通道只在本机：Linux/macOS 为数据目录下的 `control.sock`（权限 0600）；Windows 为仅当前用户可访问的命名管道 `\\.\pipe\avora-<目录哈希>`。不监听任何网络端口。
- 所有客户端命令都支持 `--json`，输出内核的原始 JSON 数据，便于脚本处理。

## 命令

```text
avora serve [--data-dir DIR] [--quiet]
avora pair <https://SaaS 地址 | lmbp://pair?...> [--allow 方案/模型,...|*] [--yes] [--max-concurrency N]
avora scheme add --type ollama|openai|anthropic|codex|grok|antigravity [--id ID] [--name 名称] [--base-url URL]
                         [--models m1,m2] [--capabilities tools,vision,audio]
                         [--api-key-env 变量名 | --api-key-stdin] [--codex-bin codex]
                         [--executable CLI路径]
avora scheme list | update <id> [同上的选项, --enable, --disable] | rm <id> | test <id>
avora status | connections
avora revoke <连接 ID>
avora models set <连接 ID> <方案/模型,...|*|none>
avora usage | audit          # 本机用量统计与审计日志
```

- API Key 只能来自环境变量（`--api-key-env`）或标准输入（`--api-key-stdin`），命令行参数里出现 `--api-key` 会被拒绝（避免进入 shell 历史和 `ps`）。
- 方案名最长 100 字节；不填时用第一个模型名（重复时自动加 `_2`、`_3`）。
- `status` 中的 `online` 只表示 LMBP 会话确实已连上网关。
- 某个已保存的方案配置失效时，其他方案照常加载；失效方案在 `scheme list` 中显示为 `unavailable: invalid configuration: ...`，不会被提供给 SaaS，可用 `scheme update` 修正或 `scheme rm` 删除。
- `connector` 子命令是旧的单连接模式（独立的身份文件），保留兼容，新部署请用 `serve`。

## 示例

```console
$ avora serve &
avora serve: data dir /home/alice/.config/avora, control /home/alice/.config/avora/control.sock
$ avora scheme add --type ollama --base-url http://127.0.0.1:11434 --models qwen3-8b
added scheme ollama (qwen3-8b, ollama_http): models qwen3-8b
$ export OPENAI_KEY=sk-...   # 或: echo "$KEY" | avora scheme add ... --api-key-stdin
$ avora scheme add --type openai --base-url https://api.openai.com/v1 --models gpt-4.1 --api-key-env OPENAI_KEY
$ avora pair https://chat.example.com --allow ollama/qwen3-8b
SaaS:            Acme Chat
...
Confirm on the SaaS side:
  code: FCTH-QHDC
Waiting for confirmation (Ctrl-C cancels)...
paired: connection b_816403ff4440 (Acme Chat)
$ avora status
kernel:       running (data dir /home/alice/.config/avora)
connections:  1 (1 online)
schemes:      2 (2 healthy)

ID              SAAS       STATUS  TASKS  MODELS           URL
b_816403ff4440  Acme Chat  online  0      ollama/qwen3-8b  https://chat.example.com
```

## 配置 Grok Build 和 Antigravity CLI

先在运行 Avora 的同一系统用户下安装官方 CLI 并登录：Grok 使用 `grok login`；Antigravity 使用 `agy` 并完成交互登录。Avora 使用官方 CLI 的登录状态，不接收这两种方案的 API Key。

先用 `grok --no-auto-update models` 或 `agy models` 查出账号当前可用的模型 ID，再添加方案：

```sh
avora scheme add --type grok --id grok --models YOUR_GROK_MODEL --executable /path/to/grok
avora scheme add --type antigravity --id agy --models YOUR_AGY_MODEL --executable /path/to/agy
avora scheme test grok
avora scheme test agy
```

`--executable` 可省略，分别从 PATH 查找 `grok`、`agy`；包含空格的路径按所在 shell 的规则加引号。类型也支持完整名称 `grok_build`、`antigravity_cli`，`agy` 是 `antigravity` 的别名。Codex 继续使用原有 `--codex-bin`。

`--reasoning-effort default`（或省略此选项）不覆盖模型设置；显式档位只允许当前模型目录支持的值。`none` 是显式关闭，不能代替默认。Antigravity 会将档位映射到目录中的同组模型变体，不支持调整的模型只能使用默认。旧 `off` 在 Grok / Antigravity 中兼容为默认，在 Codex 中按 `none` 校验；不支持的档位在测试或执行时明确报错。

这两种适配器只提供文本推理，每个方案并发为 1，不向远端开放 CLI 的文件、终端、MCP 或代理工具。CLI 必须支持当前适配器需要的 headless 参数及结构化输出，缺少选项会明确报错。当前已在本机验证 Grok Build 1.0.52 和已安装的 Antigravity CLI；不同平台的 CLI 安装与账号可用性仍取决于厂商。

Antigravity 的全局 hooks、MCP、插件、规则或技能配置可能影响独立任务。目前发现这些配置会返回 `POLICY_DENIED` 并指出路径；请用无这些自定义项的系统用户运行，或自行禁用/移走对应配置。Avora 不会自动修改官方 CLI 配置。模型目录探测成功代表当前账号可见该模型，实际配额/服务可用性仍由推理请求确认。

完整边界和扩展方式见 [本地后端架构](backend-architecture.md)。

## 作为系统服务运行

服务必须以**普通用户身份**运行（数据目录和控制通道属于该用户），客户端命令也由同一用户执行。

### Linux（systemd 用户服务）

`~/.config/systemd/user/avora.service`：

```ini
[Unit]
Description=Avora (headless)
After=network-online.target

[Service]
ExecStart=/usr/local/bin/avora serve
Restart=on-failure
RestartSec=5

[Install]
WantedBy=default.target
```

```sh
systemctl --user daemon-reload
systemctl --user enable --now avora
loginctl enable-linger "$USER"     # 未登录时也保持运行
journalctl --user -u avora -f
```

### macOS（launchd LaunchAgent）

`~/Library/LaunchAgents/com.avora.serve.plist`：

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>com.avora.serve</string>
  <key>ProgramArguments</key>
  <array><string>/usr/local/bin/avora</string><string>serve</string></array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>StandardErrorPath</key><string>/tmp/avora.log</string>
</dict>
</plist>
```

```sh
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.avora.serve.plist
launchctl kickstart -k gui/$(id -u)/com.avora.serve
```

### Windows（登录时启动的计划任务）

`avora.exe` 不是 Windows 服务程序（不实现 SCM 接口），推荐用计划任务以当前用户在登录时启动（PowerShell）：

```powershell
$a = New-ScheduledTaskAction -Execute "C:\Tools\avora.exe" -Argument "serve"
$t = New-ScheduledTaskTrigger -AtLogOn -User $env:USERNAME
$s = New-ScheduledTaskSettingsSet -RestartCount 3 -RestartInterval (New-TimeSpan -Minutes 1) -ExecutionTimeLimit 0
Register-ScheduledTask -TaskName "Avora" -Action $a -Trigger $t -Settings $s
Start-ScheduledTask -TaskName "Avora"
```

如需在用户未登录时运行，可用 NSSM 等服务包装器，并把服务的登录账户设为**该用户**（不要用 LocalSystem，否则数据目录和命名管道属于 SYSTEM，用户的 `avora status` 无法连接）。
