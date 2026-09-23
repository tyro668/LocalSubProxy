#!/bin/sh
# 发布构建：内核（Go FFI 共享库 + CLI）与 UI（Flutter）分别构建，然后整合进 .app 并签名。
set -eu
repo_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)

# 1. 内核：FFI 共享库与 CLI（server/pairing 等设备侧命令）
kernel_dir="$repo_dir/build/kernel/release"
"$repo_dir/scripts/build-kernel.sh" "$kernel_dir"
cd "$repo_dir"
tmp_cli=$(mktemp -d)
trap 'rm -rf "$tmp_cli"' EXIT
GOOS=darwin GOARCH=arm64 go build -o "$tmp_cli/arm64" ./cmd/localsubproxy
GOOS=darwin GOARCH=amd64 go build -o "$tmp_cli/amd64" ./cmd/localsubproxy
lipo -create -output "$kernel_dir/localsubproxy" "$tmp_cli/arm64" "$tmp_cli/amd64"

# 2. UI：Flutter 发布构建（shims 目录修复 Xcode 27 lipo 与 Flutter 工具的兼容问题）
app="$repo_dir/desktop/build/macos/Build/Products/Release/LocalSubProxy.app"
rm -rf "$app"
cd "$repo_dir/desktop"
PATH="$repo_dir/scripts/shims:$PATH" flutter --no-version-check build macos --release
if [ ! -f "$app/Contents/MacOS/LocalSubProxy" ]; then
  printf 'Flutter build did not produce LocalSubProxy.app\n' >&2
  exit 1
fi

# 3. 整合：共享库放到 Contents/Frameworks（native_api.dart 的加载路径），CLI 放到 Contents/MacOS
mkdir -p "$app/Contents/Frameworks"
cp "$kernel_dir/liblocalsubproxy.dylib" "$app/Contents/Frameworks/"
cp "$kernel_dir/localsubproxy" "$app/Contents/MacOS/localsubproxy-cli"

# 4. 签名（开发用 ad-hoc；分发时替换为 Developer ID）
codesign --force --deep --sign - "$app"
printf 'Built %s\n' "$app"
