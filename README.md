# LocalSubProxy

LocalSubProxy 是 macOS / Windows 桌面应用：让你授权的远端服务（SaaS）调用你本机的模型——已登录的 Codex 订阅，或你配置的 OpenAI / Anthropic 兼容 API。密钥和模型都留在本机，SaaS 只能用你允许的模型。

## 工作原理

![LocalSubProxy 架构图](docs/images/architecture.svg)

桌面端主动连出到网关，本机无需开放端口。设备与网关之间使用开放协议 **LMBP 1.0**（正式版），见 [规范](docs/lmbp-spec.md)。

## 桌面用户：快速开始

1. 从 [Releases](https://github.com/tyro668/LocalSubProxy/releases) 下载安装包（macOS `.dmg`，Windows `setup.exe`）。安装包暂未签名，首次打开的处理方法见 [docs/release.md](docs/release.md#未签名构建的注意事项)。
2. **添加方案**：选择「OpenAI Codex（订阅）」「OpenAI 兼容协议」或「Anthropic 兼容协议」，填写后可点「测试连接」。提供商没有模型列表接口时，打开「跳过模型列表探测」。
3. **连接 SaaS**：在底部输入服务地址，或粘贴 SaaS 给的 `lmbp://pair?…` 链接，点「连接」。同意页显示经网关签名校验的 SaaS 名称、隐私政策和请求的权限，并让你勾选允许它使用的模型。同意后把显示的 `XXXX-XXXX` 配对码填到 SaaS 页面上。
4. 可以同时连接多个 SaaS，每个连接有自己的模型白名单和并发上限，可随时暂停或移除。断线会自动重连，无需重新配对。

其他说明：

- 关闭窗口只隐藏界面，内核在后台运行；从系统托盘「配置」重新打开，「退出」停止。
- 每次请求写入本机审计日志 `audit.db`（不记录对话内容，默认保留 90 天），可在「近 7 天用量」或命令行 `localsubproxy usage`、`localsubproxy audit` 查看。
- Codex 方案只接受纯文本，不支持 `temperature`、`top_p`、`stop`、`max_tokens`，见规范 §16.4。

## SaaS 开发者：选一个 SDK

| 语言 | SDK | 示例 |
|---|---|---|
| Go（参考实现） | [sdk/go](sdk/go/README.md) | [examples/go-nethttp](examples/go-nethttp) |
| Node.js | [sdk/node](sdk/node/README.md) | [examples/express](examples/express) |
| Python（ASGI） | [sdk/python](sdk/python/README.md) | [examples/fastapi](examples/fastapi) |
| Java（Spring Boot） | [sdk/java](sdk/java/README.md) | [examples/spring-boot-demo](examples/spring-boot-demo/README.md) |

SDK 负责配对和协议，你的业务代码用官方 `openai` / `anthropic` 客户端调用即可。各实现的差异见 [docs/implementations.md](docs/implementations.md)。本机联调（假模型 + 网关 + 聊天页）：`scripts/mock-start.sh [go|node|python|java|rust]`，详见 [mock/README.md](mock/README.md)。

## 文档

完整索引见 [docs/README.md](docs/README.md)。服务器或无桌面环境请用命令行的无界面模式：[docs/headless.md](docs/headless.md)。

## 从源码构建与测试

- 构建：macOS `./scripts/build-macos.sh`，Windows `.\scripts\build-windows.ps1`（需要 Go ≥ 1.24.6、Flutter）。发布流程见 [docs/release.md](docs/release.md)。
- 全量校验：`scripts/verify-all.sh`（14 步，覆盖各语言 SDK、mock、规范校验、互通测试和聊天页 E2E，任何跳过都判失败）。
