# 待办事项优先级提示窗

一个 Windows 桌面待办提醒小工具：常驻桌面、支持四角吸附与半透明质感，按优先级着色，到期弹提醒。

用 **Go + WebView2** 实现，单个 exe（约 4.6 MB），无 Electron、无外部运行时依赖。

> 内部版本 **1.3.0**（对外发布版 1.0beta）

---

## 功能

- **四角吸附 / 缩入态**：贴边收起，悬停展开，不占桌面
- **半透明质感**：卡片底板与五档优先级色叠加，透出壁纸
- **五档优先级**：紧急 / 重要 / 普通 / 次要 / 搁置，按色区分并自动排序
- **到期提醒**：到点弹提醒圈与提示音，支持稍后提醒
- **就地编辑 / 删除二次确认**：改内容、改截止时间不跳窗
- **图片查看器**：任务附图支持缩放、拖拽、滚轮
- **桌宠对接**：命名管道 `\\.\pipe\whalechan-todo`，向桌面宠物推送 `state` / `due`，并响应 `open`（见 `app-go/pipe.go`）
- **便携模式**：exe 同级放 `assets/` 与 `data/` 即为绿色版

---

## 构建

需要 **Go 1.27+** 与 **Windows**（Win32 API + WebView2）。

```bat
cd app-go
build.bat
```

产物：`app-go\dist\待办事项优先级提示窗-Go.exe`（需与 `dist\assets\` 一同分发）。

构建脚本做的事：

1. `go-winres` 生成资源（图标 7 帧 + VERSIONINFO + DPI 感知 manifest）
2. `go build -trimpath -ldflags "-s -w -H windowsgui"`
3. 同步 `assets/` 到 `dist/`

> `build.bat` 必须保存为 **GBK + CRLF**：脚本里含中文路径，用 UTF-8 存会让 cmd 解出乱码文件名。

其他入口：

- `build-beta.bat` —— 对外版（`-X main.appVersion=1.0beta`）
- `installer/` —— 安装器与便携版打包器（独立 Go module）
- `toolchain/tools/release_check.py` —— 交付体检脚本（版本/图标/assets 一致性/exe 哈希基准等 9 项）

---

## 目录

```
app-go/                 Go 主程序
  ├─ main.go            入口、窗口与消息循环
  ├─ win32.go           Win32 窗口/吸附/置前/托盘
  ├─ webview.go         WebView2 宿主与桥
  ├─ gdi.go             绘制与 GDI 资源
  ├─ imageviewer.go     图片查看器
  ├─ snap.go            吸附与缩入几何
  ├─ store.go           数据读写（todos.json / settings）
  ├─ pipe.go            桌宠命名管道服务
  ├─ selftest.go        内置自检（124 项断言，--selftest 触发）
  └─ assets/            前端（HTML / CSS / JS）
installer/              安装器 + 便携版打包器
toolchain/
  ├─ relay/src/         AI 会话间接力桥（Go，纯标准库）
  └─ tools/release_check.py
```

---

## 自检

```bat
dist\待办事项优先级提示窗-Go.exe --selftest --selftest-out <输出目录> --selftest-image <测试图>
```

输出 `selftest-report.json`（当前 124 项断言）。**同时要检查退出码**：断言全绿但进程 panic 退出的情况真实发生过。

---

## 仓库范围说明

本仓库只收**源码与构建脚本**。以下内容不纳入版本控制：

| 未纳入 | 原因 |
|---|---|
| `toolchain/go`、`gopath`、`gocache` | 项目内置 Go 工具链与构建缓存（约 1.1 GB） |
| `app/` | 早期 Electron 基线，仅作迁移参照（约 395 MB，含 node_modules） |
| `_verify/`、`backup/` | 测试产物与历史备份（约 1 GB） |
| `output/` | 内部报告与交付物（含 exe/zip） |
| `*.exe`、`*.syso`、`dist/` | 构建产物，请按上方步骤自行构建 |

---

## 许可

暂未指定。如需引用或分发，请先联系作者。
