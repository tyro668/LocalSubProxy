#!/bin/sh
# 发布构建：内核（Go FFI 共享库 + CLI）与 UI（Flutter）分别构建，然后整合进 .app 并签名。
# 本地与 GitHub Actions（.github/workflows/desktop-release.yml）共用。
#
# 可选环境变量：
#   BUILD_NAME / BUILD_NUMBER   传给 flutter --build-name/--build-number（默认取 pubspec）
#   MACOS_SIGN_IDENTITY         Developer ID Application 身份；为空则 ad-hoc 签名
#   MACOS_KEYCHAIN              签名身份所在钥匙串（CI 临时钥匙串）
#   PACKAGE=1                   额外打包 dist/ 下的 .zip 与 .dmg
#   NOTARIZE=1 + APPLE_ID/APPLE_TEAM_ID/APPLE_APP_PASSWORD  公证并 staple（需 Developer ID 签名）
#   USE_LIPO_SHIM=1             启用 scripts/shims/lipo（Xcode 27 兼容；默认开启）
set -eu
repo_dir=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)

# 1. 内核：FFI 共享库与 CLI（server/pairing 等设备侧命令），arm64 + x86_64 通用二进制
kernel_dir="$repo_dir/build/kernel/release"
"$repo_dir/scripts/build-kernel.sh" "$kernel_dir"
cd "$repo_dir"
tmp_cli=$(mktemp -d)
trap 'rm -rf "$tmp_cli"' EXIT
GOOS=darwin GOARCH=arm64 go build -trimpath -o "$tmp_cli/arm64" ./cmd/localsubproxy
GOOS=darwin GOARCH=amd64 go build -trimpath -o "$tmp_cli/amd64" ./cmd/localsubproxy
lipo -create -output "$kernel_dir/localsubproxy" "$tmp_cli/arm64" "$tmp_cli/amd64"

# 2. UI：Flutter 发布构建（shims 目录修复 Xcode 27 lipo 与 Flutter 工具的兼容问题）
app="$repo_dir/desktop/build/macos/Build/Products/Release/LocalSubProxy.app"
rm -rf "$app"
cd "$repo_dir/desktop"
set -- --no-version-check build macos --release
[ -n "${BUILD_NAME:-}" ] && set -- "$@" --build-name "$BUILD_NAME"
[ -n "${BUILD_NUMBER:-}" ] && set -- "$@" --build-number "$BUILD_NUMBER"
if [ "${USE_LIPO_SHIM:-1}" = 1 ]; then
  PATH="$repo_dir/scripts/shims:$PATH" flutter "$@"
else
  flutter "$@"
fi
if [ ! -f "$app/Contents/MacOS/LocalSubProxy" ]; then
  printf 'Flutter build did not produce LocalSubProxy.app\n' >&2
  exit 1
fi

# 3. 整合：共享库放到 Contents/Frameworks（native_api.dart 的加载路径），CLI 放到 Contents/MacOS
mkdir -p "$app/Contents/Frameworks"
cp "$kernel_dir/liblocalsubproxy.dylib" "$app/Contents/Frameworks/"
cp "$kernel_dir/localsubproxy" "$app/Contents/MacOS/localsubproxy-cli"

# 4. 签名：有 Developer ID 时由内向外逐个签名并开启 Hardened Runtime；否则 ad-hoc
entitlements="$repo_dir/desktop/macos/Runner/Release.entitlements"
if [ -n "${MACOS_SIGN_IDENTITY:-}" ]; then
  set -- --force --timestamp --options runtime --sign "$MACOS_SIGN_IDENTITY"
  [ -n "${MACOS_KEYCHAIN:-}" ] && set -- "$@" --keychain "$MACOS_KEYCHAIN"
  find "$app/Contents/Frameworks" -depth \( -name '*.dylib' -o -name '*.framework' \) -print | while IFS= read -r item; do
    codesign "$@" "$item"
  done
  codesign "$@" "$app/Contents/MacOS/localsubproxy-cli"
  codesign "$@" --entitlements "$entitlements" "$app"
  codesign --verify --deep --strict --verbose=2 "$app"
else
  codesign --force --deep --sign - "$app"
fi
printf 'Built %s\n' "$app"

# 5. 打包（可选）
[ "${PACKAGE:-0}" = 1 ] || exit 0
version=${BUILD_NAME:-$(sed -n 's/^version: *\([^+]*\).*/\1/p' "$repo_dir/desktop/pubspec.yaml")}
dist="$repo_dir/dist"
mkdir -p "$dist"
zip="$dist/LocalSubProxy-$version-macos-universal.zip"
dmg="$dist/LocalSubProxy-$version-macos-universal.dmg"
rm -f "$zip" "$dmg"

notarize() {
  xcrun notarytool submit "$1" --apple-id "$APPLE_ID" --team-id "$APPLE_TEAM_ID" \
    --password "$APPLE_APP_PASSWORD" --wait --timeout 30m
}
if [ "${NOTARIZE:-0}" = 1 ]; then
  [ -n "${MACOS_SIGN_IDENTITY:-}" ] || { echo 'NOTARIZE=1 requires MACOS_SIGN_IDENTITY' >&2; exit 1; }
  ditto -c -k --keepParent "$app" "$tmp_cli/notarize.zip"
  notarize "$tmp_cli/notarize.zip"
  xcrun stapler staple "$app"
fi
ditto -c -k --sequesterRsrc --keepParent "$app" "$zip"

stage="$tmp_cli/dmg"
mkdir -p "$stage"
cp -R "$app" "$stage/"
ln -s /Applications "$stage/Applications"
hdiutil create -volname LocalSubProxy -srcfolder "$stage" -ov -format UDZO "$dmg"
if [ -n "${MACOS_SIGN_IDENTITY:-}" ]; then
  set -- --force --timestamp --sign "$MACOS_SIGN_IDENTITY"
  [ -n "${MACOS_KEYCHAIN:-}" ] && set -- "$@" --keychain "$MACOS_KEYCHAIN"
  codesign "$@" "$dmg"
  if [ "${NOTARIZE:-0}" = 1 ]; then notarize "$dmg"; xcrun stapler staple "$dmg"; fi
fi
printf 'Packaged %s\n%s\n' "$zip" "$dmg"
