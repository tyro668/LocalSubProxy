# 发布构建：内核（Go FFI DLL + CLI）与 UI（Flutter）分别构建，然后整合到同一输出目录。
$ErrorActionPreference = 'Stop'
$repo = Split-Path -Parent $PSScriptRoot

# 1. 内核：FFI DLL 与 CLI（server/pairing 等设备侧命令）
$kernel = Join-Path $repo 'build\kernel\release'
& (Join-Path $PSScriptRoot 'build-kernel.ps1') -OutDir $kernel
Push-Location $repo
try { go build -o (Join-Path $kernel 'localsubproxy.exe') ./cmd/localsubproxy; if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE } }
finally { Pop-Location }

# 2. UI：Flutter 发布构建
$out = Join-Path $repo 'desktop\build\windows\x64\runner\Release'
Remove-Item -Force -ErrorAction SilentlyContinue (Join-Path $out 'LocalSubProxy.exe')
Push-Location (Join-Path $repo 'desktop')
try { flutter --no-version-check build windows --release; if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE } }
finally { Pop-Location }

# 3. 整合：DLL 放到 LocalSubProxy.exe 同目录（native_api.dart 的加载路径），CLI 一并输出
Copy-Item -Force (Join-Path $kernel 'localsubproxy.dll') $out
Copy-Item -Force (Join-Path $kernel 'localsubproxy.exe') (Join-Path $out 'localsubproxy-cli.exe')
Write-Output "Built $out"
