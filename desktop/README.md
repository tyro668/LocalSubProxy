# desktop：一线牵（Avora）桌面端（Flutter）

macOS / Windows 配置界面。通过 FFI 加载 Go 内核（`cmd/avora_native`），接口见 [docs/desktop-status-contract.md](../docs/desktop-status-contract.md)。

- 构建：在仓库根目录运行 `./scripts/build-macos.sh` 或 `.\scripts\build-windows.ps1`（会先编译内核）；调试用 `scripts/debug-macos.sh` / `scripts/debug-windows.ps1`。
- 测试：`flutter test`、`flutter analyze`。
- 发布：见 [docs/release.md](../docs/release.md)。
