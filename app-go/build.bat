@echo off
rem ============================================================
rem  待办事项优先级提示窗 Go 版 —— 一键构建（内部版）
rem  使用项目内置 Go 工具链（..\toolchain\go），无需安装任何东西
rem  产物：dist\待办事项优先级提示窗-Go.exe（连同 assets\ 目录一起分发）
rem  资源：winres\winres-internal.json -> winres\out-internal_windows_amd64.syso
rem ------------------------------------------------------------
rem  重要：本文件必须保持 GBK(936) 编码 + CRLF 行尾。
rem        若以 UTF-8 保存，下面 -o 的中文路径会被 cmd 按 GBK 解读，
rem        产出"锟斤拷..."乱码文件名的 exe（2026-10-19 已实际发生）。
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

echo [2/4] generate resources (icon + VERSIONINFO 1.4.0)
rem 先清掉另一构建通道可能残留的 syso，避免资源串版
if exist rsrc_windows_amd64.syso del /q rsrc_windows_amd64.syso
go-winres make --in winres\winres-internal.json --out winres\out-internal --arch amd64
if errorlevel 1 goto :fail
copy /Y "winres\out-internal_windows_amd64.syso" "rsrc_windows_amd64.syso" >nul
if errorlevel 1 goto :fail

echo [3/4] go build（-s -w 去符号表，-H windowsgui 无控制台窗口）
if not exist dist mkdir dist
go build -trimpath -buildvcs=false -ldflags "-s -w -H windowsgui" -o "dist\待办事项优先级提示窗-Go.exe" .
if errorlevel 1 goto :fail

echo [4/4] sync assets to dist\assets (stale-frontend guard)
xcopy /E /Y /I "assets" "dist\assets" >nul
if errorlevel 1 goto :fail
echo.
echo 构建完成: %~dp0dist\待办事项优先级提示窗-Go.exe
for %%I in ("dist\待办事项优先级提示窗-Go.exe") do echo 体积: %%~zI 字节
echo 注意: 分发时需将 assets\ 目录与 exe 放在一起。
exit /b 0

:fail
echo 构建失败
exit /b 1
