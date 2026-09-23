#!/bin/sh
# 构建 Go 内核（cmd/localsubproxy_native）为 C 共享库，供 Flutter UI 通过 FFI 加载。
# 用法: scripts/build-kernel.sh [输出目录]   默认输出到 build/kernel/
set -eu
repo_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
out_dir=${1:-"$repo_dir/build/kernel"}
mkdir -p "$out_dir"
case "$(uname -s)" in
  Darwin) lib=liblocalsubproxy.dylib ;;
  Linux) lib=liblocalsubproxy.so ;;
  *) echo "unsupported host: $(uname -s)" >&2; exit 1 ;;
esac
cd "$repo_dir"
if [ "$(uname -s)" = Darwin ]; then
  tmp_dir=$(mktemp -d)
  trap 'rm -rf "$tmp_dir"' EXIT
  GOOS=darwin GOARCH=arm64 CGO_ENABLED=1 CC='clang -arch arm64' go build -buildmode=c-shared -o "$tmp_dir/arm64.dylib" ./cmd/localsubproxy_native
  GOOS=darwin GOARCH=amd64 CGO_ENABLED=1 CC='clang -arch x86_64' go build -buildmode=c-shared -o "$tmp_dir/amd64.dylib" ./cmd/localsubproxy_native
  lipo -create -output "$out_dir/$lib" "$tmp_dir/arm64.dylib" "$tmp_dir/amd64.dylib"
else
  go build -buildmode=c-shared -o "$out_dir/$lib" ./cmd/localsubproxy_native
fi
printf 'Kernel built: %s\n' "$out_dir/$lib"
