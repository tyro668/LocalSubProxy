#!/bin/sh
# 调试入口：先构建 Go 内核共享库，再以调试模式启动 Flutter UI。
# UI 通过环境变量 LSP_NATIVE_LIB 加载刚构建的内核（见 desktop/lib/native_api.dart）。
# 额外参数原样传给 flutter run，例如: scripts/debug-macos.sh --dart-define=FOO=bar
set -eu
repo_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
"$repo_dir/scripts/build-kernel.sh" "$repo_dir/build/kernel/debug"
export LSP_NATIVE_LIB="$repo_dir/build/kernel/debug/liblocalsubproxy.dylib"
# shims 目录修复 Xcode 27 lipo 与 Flutter 工具的兼容问题
export PATH="$repo_dir/scripts/shims:$PATH"
cd "$repo_dir/desktop"
exec flutter --no-version-check run -d macos "$@"
