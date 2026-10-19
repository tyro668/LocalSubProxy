# 更新记录

## 未发布

- 修复：Windows 托盘右键菜单显示乱码（「é…ç½®」「é€€å‡º」）。Windows 原生代码现在按 UTF-8 编译（MSVC `/utf-8`），菜单显示「配置」「退出」。

## 2.1.2

- 修复：网关的 task.ack 可能乱序发出。客户端取消后，设备迟到帧的确认（如 2）偶尔晚于终止帧的确认（3）写出，设备看到的确认倒退，像是对终止帧之后的帧做了确认（v2.1.1 macOS 打包时一致性检查 S3 因此失败）。现在每个任务的确认按序串行写出，低于已发确认的不再发送。
- 修复：设备发来非法帧（非法终止帧、非法增量、序号跳号）或网关落盘失败时，网关先发 task.cancel 再判定结果；设备对取消的回复（task.error CANCELLED）偶尔先到，任务就被记成设备取消而不是 INTERNAL。现在先判定结果再通知设备停止。

## 2.1.1

- 产品名只用 Avora，不再使用中文名：窗口标题、托盘提示、macOS 显示名、Windows 安装包名称与快捷方式、界面文字、文档、架构图和截图都改为 Avora。
- Windows 安装包升级时删除 2.1.0 留下的中文名开始菜单 / 桌面快捷方式。
- GitHub Release 标题为「Avora <版本>」。

## 2.1.0

### 命名清理（接续改名）

| 原名 | 新名 | 兼容 |
|---|---|---|
| 参考网关环境变量 `LSP_SIGNING_SEED`、`LSP_DATA_KEY`、`LSP_TENANT_KEY_*` | `LMBP_SIGNING_SEED`、`LMBP_DATA_KEY`、`LMBP_TENANT_KEY_*`（`avora server -tenants` 默认 `default:LMBP_TENANT_KEY_DEFAULT`） | 新名未设置时读取旧名并打印弃用提示，保留一个版本；`scripts/mock-start.sh` 自动把旧 `.mock/env` 中的键复制为新名 |
| 内核 C 导出 `LSP_Start` / `LSP_Call` / `LSP_Free` | `AVORA_Start` / `AVORA_Call` / `AVORA_Free` | 无（桌面端与内核同包发布） |
| 调试变量 `LSP_NATIVE_LIB` | `AVORA_NATIVE_LIB` | 旧名仍可用一个版本 |
| 测试变量 `LSP_ACCEPT_MOCK_DIR`、`LSP_ACCEPT_PORT` | `AVORA_ACCEPT_MOCK_DIR`、`AVORA_ACCEPT_PORT` | 无 |
| mock 命令 `lspmock`（`mock/cmd/lspmock`） | `lmbpmock`（`mock/cmd/lmbpmock`） | 无 |
| SDK 文档/示例变量 `lsp` | `lmbp` | — |

- Windows 窗口标题显示「Avora」（此前为空，任务栏和 Alt-Tab 不显示名称）。

### 改名：LocalSubProxy → Avora

应用名 Avora（不再使用中文名）。协议名不变：LMBP、`lmbp` 子协议、`/lmbp/v1`、`/.well-known/lmbp`、`lmbp-conformance`、SDK 包名 `lmbp-server`。

| 原名 | 新名 |
|---|---|
| 命令 `localsubproxy` | `avora` |
| Go 模块 `github.com/tyro668/LocalSubProxy` | `github.com/tyro668/avora` |
| Java 包 / Maven groupId `dev.localsubproxy` | `dev.avora` |
| 环境变量 `LOCALSUBPROXY_*` | `AVORA_*` |
| 数据目录 `<配置目录>/localsubproxy` | `<配置目录>/avora` |
| 应用标识 `dev.localsubproxy.localsubproxyUi` | `dev.avora.desktop` |
| SaaS 发现文档 `/.well-known/localsubproxy` | `/.well-known/avora` |
| Schema `$id` / 错误类型 `https://localsubproxy.dev/...` | `https://avora.dev/...` |
| 安装包 `LocalSubProxy-<版本>-...` | `Avora-<版本>-...` |

兼容（保留一个版本，之后移除）：

- 首次用默认数据目录启动时，旧目录（含连接、设备私钥、模型方案、审计日志）自动迁移到新目录，旧目录改名为 `localsubproxy.backup-<时间>` 备份，不需要重新配对。旧版仍在运行时不迁移，并提示先退出旧版。
- 旧的 `LOCALSUBPROXY_*` 环境变量仍可读取，并打印弃用提示。
- CLI 压缩包附带 `localsubproxy` 转发命令：提示改名后以相同参数运行 `avora`。
- 设备先请求 `/.well-known/avora`，返回 404 时再请求旧的 `/.well-known/localsubproxy`；示例 SaaS 两个地址都提供。
- Windows 安装包沿用原 AppId（升级时替换原卸载项），并删除旧的默认安装目录和快捷方式。macOS 不会自动删除旧的 `LocalSubProxy.app`，迁移完成后请手动删除。
- 无界面模式配置成系统服务（systemd / LaunchAgent / 计划任务）的，请把服务定义改用 `avora`，见 [docs/headless.md](docs/headless.md)。
- 内核动态库（`libavora.dylib`、`avora.dll`）改为 `-trimpath` 构建，产物中不再带构建机的源码路径。
