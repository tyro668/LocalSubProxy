# 更新记录

## 未发布

- 「可用模型」和配对确认页顶部新增「允许全部模型」开关：打开后保存为 `*`，列表锁定并全部勾选，以后新增的方案和模型自动开放；关闭后恢复之前的逐个勾选。配对确认页按钮改为「拒绝 / 允许」。
- 修复：Windows 托盘右键菜单显示乱码（「é…ç½®」「é€€å‡º」）。Windows 原生代码现在按 UTF-8 编译（MSVC `/utf-8`），菜单显示「配置」「退出」。
- 移除改名过渡期的全部兼容代码：旧数据目录自动迁移、旧环境变量与旧调试变量回退、旧命令转发、旧 SaaS 发现路径、安装包清理旧安装目录和旧快捷方式。Avora 尚未正式发布，不需要这些过渡。

## 2.1.2

- 修复：网关的 task.ack 可能乱序发出。客户端取消后，设备迟到帧的确认（如 2）偶尔晚于终止帧的确认（3）写出，设备看到的确认倒退，像是对终止帧之后的帧做了确认（v2.1.1 macOS 打包时一致性检查 S3 因此失败）。现在每个任务的确认按序串行写出，低于已发确认的不再发送。
- 修复：设备发来非法帧（非法终止帧、非法增量、序号跳号）或网关落盘失败时，网关先发 task.cancel 再判定结果；设备对取消的回复（task.error CANCELLED）偶尔先到，任务就被记成设备取消而不是 INTERNAL。现在先判定结果再通知设备停止。

## 2.1.1

- 产品名只用 Avora：窗口标题、托盘提示、macOS 显示名、Windows 安装包名称与快捷方式、界面文字、文档、架构图和截图都改为 Avora。
- GitHub Release 标题为「Avora <版本>」。

## 2.1.0

- 应用定名 Avora。协议名不变：LMBP、`lmbp` 子协议、`/lmbp/v1`、`/.well-known/lmbp`、`lmbp-conformance`、SDK 包名 `lmbp-server`。
- 命令 `avora`；Go 模块 `github.com/tyro668/avora`；Java 包 / Maven groupId `dev.avora`；应用标识 `dev.avora.desktop`；数据目录 `<配置目录>/avora`；环境变量 `AVORA_*`；SaaS 发现文档 `/.well-known/avora`；Schema `$id` / 错误类型 `https://avora.dev/...`；安装包 `Avora-<版本>-...`。
- 参考网关环境变量 `LMBP_SIGNING_SEED`、`LMBP_DATA_KEY`、`LMBP_TENANT_KEY_*`（`avora server -tenants` 默认 `default:LMBP_TENANT_KEY_DEFAULT`）；内核 C 导出 `AVORA_Start` / `AVORA_Call` / `AVORA_Free`；调试变量 `AVORA_NATIVE_LIB`；测试变量 `AVORA_ACCEPT_MOCK_DIR`、`AVORA_ACCEPT_PORT`；mock 命令 `lmbpmock`。
- Windows 窗口标题显示「Avora」（此前为空，任务栏和 Alt-Tab 不显示名称）。
- 内核动态库（`libavora.dylib`、`avora.dll`）改为 `-trimpath` 构建，产物中不再带构建机的源码路径。
