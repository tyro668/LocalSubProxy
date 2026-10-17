# 文档索引

| 我想… | 看这里 |
|---|---|
| 安装并使用桌面端 | [图文教程 guide.md](guide.md) · [根 README](../README.md) |
| 不用桌面界面运行（服务器、systemd / launchd / Windows 计划任务） | [headless.md](headless.md) |
| 在 SaaS 里接入 | [sdk/go](../sdk/go/README.md) · [sdk/node](../sdk/node/README.md) · [sdk/python](../sdk/python/README.md) · [sdk/java](../sdk/java/README.md) |
| 了解各实现的差异和成熟度 | [implementations.md](implementations.md) |
| 读协议规范（LMBP 2.0 正式版） | [lmbp-spec.md](lmbp-spec.md) |
| 用其他语言自己实现网关 | [implementers-guide.md](implementers-guide.md)，机器可读契约和测试向量在 [spec/](../spec) |
| 本机联调、模拟网关、聊天页 | [mock/README.md](../mock/README.md) |
| 桌面端 UI 与内核的接口 | [desktop-status-contract.md](desktop-status-contract.md) |
| 构建和发布安装包 | [release.md](release.md) |

术语：**方案**＝桌面端配置的一个模型来源（内核和 CLI 中叫 adapter / 适配器）；**连接**＝桌面端与一个 SaaS 网关的配对（内核中叫 binding）；**设备**＝网关看到的桌面端身份（`device_id`），每个连接一个。
