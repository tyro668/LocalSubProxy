# 桌面端发布（GitHub Actions）

工作流：`.github/workflows/desktop-release.yml`

## 触发方式

| 触发 | 行为 |
|---|---|
| 推送 tag `v*`（如 `v1.2.0`、`v1.3.0-beta.1`） | 构建 macOS + Windows，创建 GitHub Release 并附上安装包；带 `-` 的版本标记为 prerelease |
| Actions 页面手动运行（workflow_dispatch） | 只构建、上传 artifact；可选是否跑快速测试、可指定版本号 |
| PR（改动 desktop/cmd/internal/打包相关文件） | 只构建、上传 artifact |
| 推送到 `ci-*` / `ci/**` 分支 | 只构建，用于合并前验证 |

发版：

```sh
# 可选：同步更新 desktop/pubspec.yaml 的 version
git tag v1.2.0 && git push origin v1.2.0
```

版本号取自 tag（去掉 `v`），通过 `--build-name` 写入 App；`--build-number` 为 Actions 运行序号。

## 产物

| 平台 | 文件 |
|---|---|
| macOS（universal：arm64 + x86_64） | `LocalSubProxy-<ver>-macos-universal.dmg`、`LocalSubProxy-<ver>-macos-universal.zip` |
| Windows x64 | `LocalSubProxy-<ver>-windows-x64-setup.exe`（Inno Setup，默认按用户安装，无需管理员）、`LocalSubProxy-<ver>-windows-x64.zip`（便携版） |
| Release | 以上文件 + `SHA256SUMS.txt` |

Artifact 名：`LocalSubProxy-<ver>-macos`、`LocalSubProxy-<ver>-windows`。

App 内容：Go 内核 `liblocalsubproxy.dylib` 放在 `LocalSubProxy.app/Contents/Frameworks/`，`localsubproxy.dll` 放在 `LocalSubProxy.exe` 同目录（对应 `desktop/lib/native_api.dart` 的加载路径）；CLI 分别为 `Contents/MacOS/localsubproxy-cli`、`localsubproxy-cli.exe`。

## Secrets（全部可选）

| Secret | 用途 |
|---|---|
| `MACOS_CERT_P12` | Developer ID Application 证书 .p12 的 base64（`base64 -i cert.p12 \| pbcopy`） |
| `MACOS_CERT_PASSWORD` | .p12 密码 |
| `MACOS_SIGN_IDENTITY` | 签名身份，如 `Developer ID Application: Name (TEAMID)` |
| `APPLE_ID` / `APPLE_TEAM_ID` / `APPLE_APP_PASSWORD` | notarytool 公证（App 专用密码）；需同时配置上面三项 |
| `WINDOWS_CERT_PFX` | 代码签名证书 .pfx 的 base64 |
| `WINDOWS_CERT_PASSWORD` | .pfx 密码 |

未配置 macOS 证书时使用 ad-hoc 签名；配置后启用 Hardened Runtime（`desktop/macos/Runner/Release.entitlements`），四项 Apple 公证 secret 齐全时自动公证并 staple（.app 与 .dmg）。未配置 Windows 证书时不签名。

## 未签名构建的注意事项

- **macOS**：ad-hoc 签名的 App 会被 Gatekeeper 拦截（"无法验证开发者"/"已损坏"）。在 Finder 中右键 App →「打开」→ 再点「打开」；或到「系统设置 → 隐私与安全性」点「仍要打开」。从浏览器下载后若提示已损坏，可执行 `xattr -dr com.apple.quarantine /Applications/LocalSubProxy.app`。
- **Windows**：未签名安装包会触发 SmartScreen「Windows 已保护你的电脑」，点「更多信息」→「仍要运行」。

## 本地构建

```sh
# macOS（需要 Xcode、Go ≥ 1.24.6、Flutter 3.47.7）
PACKAGE=1 BUILD_NAME=1.2.0 ./scripts/build-macos.sh          # 产物在 dist/
```

```powershell
# Windows（需要 VS 2022 C++ 工作负载、mingw-w64 gcc、Go、Flutter、Inno Setup 6）
$env:CGO_ENABLED = '1'
./scripts/build-windows.ps1 -BuildName 1.2.0 -Package          # 产物在 dist\
```

工具链版本：Go 用 `1.24.x` 最新补丁（≥ 1.24.6），Flutter 固定 `3.47.7`，在工作流 `env` 中统一修改。

## Windows 兼容性说明

- `internal/durable` 在 Windows 上不再对目录执行 fsync（目录句柄无法 `FlushFileBuffers`，会返回 Access is denied，导致保存报错）；文件内容仍在 rename 前 fsync，rename 使用 `os.Rename`（MoveFileEx + REPLACE_EXISTING）。
- `spec/testvectors/**` 通过 `.gitattributes` 保持 LF，避免 Windows 检出后向量比对失败。
- Windows 与 macOS 任务都会运行 `go test -short ./internal/... ./cmd/...` 和 `flutter test`；Windows 上跳过依赖 POSIX 可执行权限的用例。
- `ci-*` / `ci/**` 分支推送触发只构建不发布，用于验证打包流程，可按需删除。
