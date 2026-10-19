# build-installer.ps1 -- one-shot build for the public installer (1.0beta)
# ASCII only on purpose. Run from the installer/ directory:
#   powershell -ExecutionPolicy Bypass -File .\build-install.ps1
$ErrorActionPreference = 'Stop'
$root = $PSScriptRoot
$app  = Join-Path $root '..\app-go'
$tool = Join-Path $root '..\toolchain'

$env:GOROOT   = Join-Path $tool 'go'
$env:GOPATH   = Join-Path $tool 'gopath'
$env:GOCACHE  = Join-Path $tool 'gocache'
$env:GOPROXY  = 'https://goproxy.cn,direct'
$env:GOSUMDB  = 'sum.golang.google.cn'
$env:GOTOOLCHAIN = 'local'
$env:PATH = "$env:GOROOT\bin;$env:GOPATH\bin;$env:PATH"

# 0) beta main program must exist
$betaSrc = Join-Path $app 'dist\build-beta-out.exe'
if (-not (Test-Path $betaSrc)) { throw 'missing app-go\dist\build-beta-out.exe (run build-beta.bat first)' }

# 1) uninstaller
Push-Location $root
go build -trimpath -o ".\uninstall\uninstall-tmp.exe" .\uninstall
if ($LASTEXITCODE -ne 0) { throw 'uninstall build failed' }
Copy-Item ".\uninstall\uninstall-tmp.exe" ".\payload\uninstall.exe" -Force

# 2) payload app + assets
New-Item -ItemType Directory -Force -Path ".\payload",".\dist" | Out-Null
Copy-Item $betaSrc ".\payload\app.exe" -Force
if (Test-Path ".\payload\assets") { Remove-Item ".\payload\assets" -Recurse -Force }
xcopy /E /Y /I (Join-Path $app 'dist\assets') ".\payload\assets" | Out-Null
if ($LASTEXITCODE -ne 0) { throw 'assets copy failed' }

# 2b) installer resource (app icon + VERSIONINFO) -- rsrc_windows_amd64.syso picked up by go build
#     remove old syso first so a failed go-winres run cannot silently keep a stale one
Remove-Item ".\rsrc_windows_amd64.syso" -Force -ErrorAction SilentlyContinue
go-winres simply --arch amd64 --icon ".\payload\assets\icon.ico" `
  --product-version 1.0beta --file-version 1.0.0.0 `
  --product-name "TodoPriorityWidget Installer" --file-description "TodoPriorityWidget 1.0beta installer" `
  --manifest gui --out rsrc
if ($LASTEXITCODE -ne 0 -or -not (Test-Path ".\rsrc_windows_amd64.syso")) { throw 'go-winres failed' }

# 3) installer build (payload embeds app.exe/uninstall.exe/assets)
$outfile = Join-Path $root 'dist\build-installer-out.exe'
go build -trimpath -ldflags '-s -w -H windowsgui' -o $outfile .
if ($LASTEXITCODE -ne 0) { throw 'installer build failed' }

# 4) public Chinese filename (UTF-16-safe rename)
$distDir = Join-Path $root 'dist'
$staged  = Join-Path $distDir 'build-installer-out.exe'
if (-not (Test-Path $staged)) { throw 'staged output missing' }
$publicName = [string]([char]0x5F85+[char]0x529E+[char]0x4E8B+[char]0x9879+[char]0x4F18+[char]0x5148+[char]0x7EA7+[char]0x63D0+[char]0x793A+[char]0x7A97+[char]0x2D+[char]0x5B89+[char]0x88C5+[char]0x5305+[char]0x2D) + '1.0beta.exe'
Move-Item -Force $staged (Join-Path $distDir $publicName)

$final = Join-Path $distDir $publicName
"BUILD OK"
"size: $((Get-Item $final).Length)"
"sha256: $((Get-FileHash $final -Algorithm SHA256).Hash.ToLower())"
Pop-Location
