#!/bin/sh
# 本项目专用的 Go 环境（全部落在项目目录内，不污染用户目录）
# 用法：  . toolchain/goenv.sh
export GOROOT="D:/待办事项优先级提示窗/toolchain/go"
export GOPATH="D:/待办事项优先级提示窗/toolchain/gopath"
export GOCACHE="D:/待办事项优先级提示窗/toolchain/gocache"
export GOPROXY="https://goproxy.cn,direct"
export GOSUMDB="sum.golang.google.cn"
export GOTOOLCHAIN="local"
# Git Bash 需要 POSIX 风格路径才能识别
export PATH="/d/待办事项优先级提示窗/toolchain/go/bin:$PATH"
