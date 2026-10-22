# installer/ —— 待办事项优先级提示窗 · 对外安装器（1.1beta）

单文件安装器：内嵌主程序（1.1beta 构建）+ `assets/` 全量 + `uninstall.exe`（go:embed），
安装到 `%LOCALAPPDATA%\Programs\TodoPriorityWidget\`（用户级，免管理员），
写 HKCU 卸载注册表项、创建开始菜单快捷方式（桌面可选）。

## 目录结构

```
installer/
  main.go               安装器（GUI + --install 静默通道）
  uninstall/main.go     卸载器（GUI 是/否 + --silent 静默通道）
  makeportable/main.go  便携版 zip 打包工具（T5；确定性打包：排序+固定 modtime）
  go.mod / go.sum       独立 Go 模块（todo-priority-installer，仅依赖 golang.org/x/sys）
  payload/              构建期暂存（app.exe / uninstall.exe / assets\），由构建步生成
  build-install.ps1     一键构建安装器（见下）
  makeportable\portable-staging\   便携版 staging（工具生成）
  dist/                 产物：待办事项优先级提示窗-安装包-1.1beta.exe
                        待办事项优先级提示窗-便携版-1.1beta.zip
  README.md             本文件
```

## 便携版 zip（T5，纯打包）

- 主程序 = T10-β 已终审 beta 产物 `app-go\dist\build-beta-out.exe`（字节不变改名，sha256 `e37b29ec…`）；
  数据目录机制原生支持（exe 同级存在 `data\` 即切换便携模式，`store.go` NewStore + 单测 TestPortableMode）。
- 构建：`makeportable\makeportable.exe -app <beta exe> -assets app-go\dist\assets -out dist\<zip 名>`
  （或 `go run .\makeportable`）。
- 用 **Windows 资源管理器/Shell 解压引擎**解压验证中文名无损（zip 非条目名 UTF-8 标志位由标准库置位）。

## 构建（顺序敏感）

前置：`app-go\dist\build-beta-out.exe` 已由 `app-go\build-beta.bat` 产出（见 app-go/README 或任务书）。
运行 `build-install.ps1`（PowerShell）：

1. 构建卸载器 → `payload\uninstall.exe`
2. 复制 `app-go\dist\build-beta-out.exe` → `payload\app.exe`
3. 复制 `app-go\dist\assets` → `payload\assets\`
4. go-winres 生成安装器资源（图标+VERSIONINFO）
5. `go build`（windowsgui）→ `dist\build-installer-out.exe`
6. 更名 `dist\待办事项优先级提示窗-安装包-1.1beta.exe`

## 安装包使用

- 双击：简洁向导（安装位置只读固定）→ [开始安装] → [完成]（[立即运行] 默认勾选）。
- 覆盖安装：再次运行直接覆盖更新文件/快捷方式/注册表。

## 卸载

- 三种入口任意：开始菜单快捷方式（安装器未创建的「卸载」项 v1 未做）、目标目录 `uninstall.exe`、
  Windows 设置-应用列表。
- 运行中的主程序检测：按**进程名**枚举（FindWindowW 类名跨进程实测失效，见批报告），
  检测到运行仅提示先退出，**绝不强杀**。
- 用户数据 `%APPDATA%\待办事项优先级提示窗-Go` 永不触碰（卸载/覆盖安装均保留）。

## 自动化通道（实测用，披露）

- `安装器.exe --install`：跳过 GUI 直走安装（写路径、注册表、快捷方式同一实现）。
- `uninstall.exe --silent`：跳过确认弹窗（运行检测照常生效）。

## 技术与限制（如实）

- 快捷方式创建：COM `IShellLinkW`+`IPersistFile` 手工 vtable 调用
  （vtable 序已核 SDK/ReactOS：SetPath=20 / SetWorkingDirectory=9 / SetIconLocation=17 / Save=6）。
- 自删：`cmd /c ping…& rd /s /q`（`MoveFileEx DELAY_UNTIL_REBOOT` 需 HKLM 非管理员不可用；
  cmd 属 Windows 系统组件而非打包依赖）。
- 无数字签名：目标机首次运行可能触发 SmartScreen（预期，待证书）。
- 卸载器运行检测若被改回 FindWindowW 方案有风险（跨进程类名查找失效现象，批报告已记录）。
