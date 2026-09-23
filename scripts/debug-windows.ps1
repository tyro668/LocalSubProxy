# 调试入口：先构建 Go 内核 DLL，再以调试模式启动 Flutter UI。
# UI 通过环境变量 LSP_NATIVE_LIB 加载刚构建的内核（见 desktop/lib/native_api.dart）。
# 额外参数原样传给 flutter run。
$ErrorActionPreference = 'Stop'
$repo = Split-Path -Parent $PSScriptRoot
$out = Join-Path $repo 'build\kernel\debug'
& (Join-Path $PSScriptRoot 'build-kernel.ps1') -OutDir $out
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
$env:LSP_NATIVE_LIB = Join-Path $out 'localsubproxy.dll'
Push-Location (Join-Path $repo 'desktop')
try { flutter --no-version-check run -d windows @args }
finally { Pop-Location }
