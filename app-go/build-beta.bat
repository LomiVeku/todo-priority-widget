@echo off
rem ============================================================
rem  Todo widget Go build -- beta (public) edition
rem  Uses the project-bundled Go toolchain (..\toolchain\go), no install needed
rem  Produces: dist\build-beta-out.exe (ASCII name; the installer embeds it and
rem            extracts as the public Chinese filename via UTF-16 syscalls)
rem  Version: -X main.appVersion=1.0beta (build-time override)
rem  Resources: rsrc_windows_amd64.syso (go-winres: icon + VERSIONINFO + GUI manifest)
rem  NOTE: ASCII only -- cmd.exe parses bat files in the OEM codepage
rem ============================================================
setlocal
cd /d "%~dp0"

set "TOOLCHAIN=%~dp0..\toolchain"
set "GOROOT=%TOOLCHAIN%\go"
set "GOPATH=%TOOLCHAIN%\gopath"
set "GOCACHE=%TOOLCHAIN%\gocache"
set "GOPROXY=https://goproxy.cn,direct"
set "GOSUMDB=sum.golang.google.cn"
set "GOTOOLCHAIN=local"
set "PATH=%GOROOT%\bin;%TOOLCHAIN%\gopath\bin;%PATH%"

echo [1/4] go version
go version
if errorlevel 1 goto :fail

[2/4] generate resources (icon + VERSIONINFO 1.0beta)
go-winres make --in winres\winres-beta.json --out rsrc --arch amd64
if errorlevel 1 goto :fail

echo [3/4] go build (1.0beta + icon syso)
if not exist dist mkdir dist
go build -trimpath -ldflags "-s -w -H windowsgui -X main.appVersion=1.0beta" -o "dist\build-beta-out.exe" .
if errorlevel 1 goto :fail

echo [4/4] sync assets to dist\assets
xcopy /E /Y /I "assets" "dist\assets" >nul 2>&1
if errorlevel 1 goto :fail
echo.
echo BUILD OK: dist\build-beta-out.exe
exit /b 0

:fail
echo BUILD FAILED
exit /b 1
