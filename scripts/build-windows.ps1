# 发布构建：内核（Go FFI DLL + CLI）与 UI（Flutter）分别构建，然后整合到同一输出目录。
# 本地与 GitHub Actions（.github/workflows/desktop-release.yml）共用。
#   -BuildName / -BuildNumber  传给 flutter --build-name/--build-number（默认取 pubspec）
#   -Package                   额外生成 dist\ 下的便携 .zip 与 Inno Setup 安装包（需要 iscc）
#   环境变量 WINDOWS_CERT_PFX_PATH + WINDOWS_CERT_PASSWORD 存在时用 signtool 签名 exe/dll/安装包
param([string]$BuildName = "", [string]$BuildNumber = "", [switch]$Package)
$ErrorActionPreference = 'Stop'
$repo = Split-Path -Parent $PSScriptRoot

function Invoke-Checked([scriptblock]$Block) { & $Block; if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE } }

function Invoke-Sign([string[]]$Files) {
  $pfx = $env:WINDOWS_CERT_PFX_PATH
  if (-not $pfx -or -not (Test-Path $pfx)) { return }
  $signtool = Get-ChildItem "${env:ProgramFiles(x86)}\Windows Kits\10\bin\*\x64\signtool.exe" -ErrorAction SilentlyContinue |
    Sort-Object FullName -Descending | Select-Object -First 1
  if (-not $signtool) { throw 'signtool.exe not found' }
  Invoke-Checked { & $signtool.FullName sign /f $pfx /p $env:WINDOWS_CERT_PASSWORD /fd SHA256 /td SHA256 /tr http://timestamp.digicert.com @Files }
}

# 1. 内核：FFI DLL 与 CLI（server/pairing 等设备侧命令）
$kernel = Join-Path $repo 'build\kernel\release'
& (Join-Path $PSScriptRoot 'build-kernel.ps1') -OutDir $kernel
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
Push-Location $repo
try { Invoke-Checked { go build -trimpath -o (Join-Path $kernel 'avora.exe') ./cmd/avora } }
finally { Pop-Location }

# 2. UI：Flutter 发布构建
$out = Join-Path $repo 'desktop\build\windows\x64\runner\Release'
Remove-Item -Force -ErrorAction SilentlyContinue (Join-Path $out 'Avora.exe')
$flutterArgs = @('--no-version-check', 'build', 'windows', '--release')
if ($BuildName) { $flutterArgs += @('--build-name', $BuildName) }
if ($BuildNumber) { $flutterArgs += @('--build-number', $BuildNumber) }
Push-Location (Join-Path $repo 'desktop')
try { Invoke-Checked { flutter @flutterArgs } }
finally { Pop-Location }
if (-not (Test-Path (Join-Path $out 'Avora.exe'))) { throw 'Flutter build did not produce Avora.exe' }

# 3. 整合：DLL 放到 Avora.exe 同目录（native_api.dart 的加载路径），CLI 一并输出
Copy-Item -Force (Join-Path $kernel 'avora.dll') $out
Copy-Item -Force (Join-Path $kernel 'avora.exe') (Join-Path $out 'avora-cli.exe')
Invoke-Sign @((Join-Path $out 'Avora.exe'), (Join-Path $out 'avora.dll'), (Join-Path $out 'avora-cli.exe'))
Write-Output "Built $out"
if (-not $Package) { exit 0 }

# 4. 打包：便携 zip + Inno Setup 安装包
if (-not $BuildName) {
  $BuildName = ((Select-String -Path (Join-Path $repo 'desktop\pubspec.yaml') -Pattern '^version:\s*([^+\s]+)').Matches[0].Groups[1].Value)
}
$dist = Join-Path $repo 'dist'
New-Item -ItemType Directory -Force -Path $dist | Out-Null
$zip = Join-Path $dist "Avora-$BuildName-windows-x64.zip"
Remove-Item -Force -ErrorAction SilentlyContinue $zip
Compress-Archive -Path (Join-Path $out '*') -DestinationPath $zip

$iscc = (Get-Command iscc.exe -ErrorAction SilentlyContinue).Source
if (-not $iscc) { $iscc = "${env:ProgramFiles(x86)}\Inno Setup 6\ISCC.exe" }
if (-not (Test-Path $iscc)) { throw 'Inno Setup (ISCC.exe) not found; install with: choco install innosetup' }
# 版本号去掉预发布后缀（如 1.2.0-beta.1 -> 1.2.0），Windows 文件版本只接受数字
$numeric = ($BuildName -split '[-+]')[0]
Invoke-Checked { & $iscc "/DAppVersion=$BuildName" "/DAppVersionNumeric=$numeric" "/DSourceDir=$out" "/DOutputDir=$dist" (Join-Path $repo 'packaging\windows\Avora.iss') }
$setup = Join-Path $dist "Avora-$BuildName-windows-x64-setup.exe"
Invoke-Sign @($setup)
Write-Output "Packaged $zip"
Write-Output "Packaged $setup"
