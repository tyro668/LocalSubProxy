# 构建 Go 内核（cmd/localsubproxy_native）为 C 共享库 localsubproxy.dll，
# 供 Flutter UI 通过 FFI 加载。c-shared 构建需要 C 工具链（如 mingw-w64 gcc）。
# 用法: powershell -File scripts/build-kernel.ps1 [-OutDir <目录>]   默认 build\kernel\
param([string]$OutDir = "")
$ErrorActionPreference = 'Stop'
$repo = Split-Path -Parent $PSScriptRoot
if ($OutDir -eq "") { $OutDir = Join-Path $repo 'build\kernel' }
New-Item -ItemType Directory -Force -Path $OutDir | Out-Null
Push-Location $repo
try {
  go build -buildmode=c-shared -o (Join-Path $OutDir 'localsubproxy.dll') ./cmd/localsubproxy_native
  if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
}
finally { Pop-Location }
Write-Output "Kernel built: $(Join-Path $OutDir 'localsubproxy.dll')"
