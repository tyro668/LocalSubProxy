# 更新记录

## 未发布（计划 2.1.0）

### 改名：LocalSubProxy → 一线牵（Avora）

应用中文名「一线牵」，英文名 Avora。协议名不变：LMBP、`lmbp` 子协议、`/lmbp/v1`、`/.well-known/lmbp`、`lmbp-conformance`、SDK 包名 `lmbp-server`。

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
- Windows 安装包沿用原 AppId（升级时替换原卸载项），并删除旧的默认安装目录和快捷方式。
