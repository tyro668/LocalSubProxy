#!/usr/bin/env bash
# Cross-build the headless CLI (avora) for Linux, macOS and Windows,
# amd64 and arm64, into dist/: tar.gz for Linux/macOS, zip for Windows.
#   scripts/build-cli.sh [version]      # default: desktop/pubspec.yaml version
# Pure Go (CGO_ENABLED=0): no C toolchain needed for any target.
set -eu
repo=$(cd "$(dirname "$0")/.." && pwd)
cd "$repo"
version=${1:-$(sed -n 's/^version: *\([^+]*\).*/\1/p' desktop/pubspec.yaml)}
out=$repo/dist
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
mkdir -p "$out"
for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64; do
  os=${target%/*} arch=${target#*/}
  name=avora-cli-$version-$os-$arch
  ext=; [ "$os" = windows ] && ext=.exe
  mkdir -p "$work/$name"
  CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath -ldflags "-s -w" -o "$work/$name/avora$ext" ./cmd/avora
  cp docs/headless.md "$work/$name/"
  if [ "$os" = windows ]; then
    rm -f "$out/$name.zip"
    if command -v zip >/dev/null; then
      (cd "$work" && zip -qr "$out/$name.zip" "$name")
    else
      (cd "$work" && python3 -m zipfile -c "$out/$name.zip" "$name")
    fi
  else
    tar -C "$work" -czf "$out/$name.tar.gz" "$name"
  fi
  echo "dist/$name"
done
