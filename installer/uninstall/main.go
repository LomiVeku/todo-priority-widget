//go:build windows

// 卸载器（uninstall.exe）——待办事项优先级提示窗 · 1.0beta 对外版
//
// 行为（v1）：
//   1. 检测主程序运行中（窗口类 TodoWidgetGoWnd）→ 提示先退出，不强杀
//   2. 是/否确认 → 删除：HKCU 卸载注册表项、快捷方式（开始菜单/桌面）、安装目录
//   3. 自删：本进程退出后由隐藏 cmd 延迟 rd /s /q（非管理员的可行自删方案）
//   4. 用户数据在 %APPDATA%\待办事项优先级提示窗-Go（安装目录之外）——永不触碰
//
// 技术约束：纯 Win32 syscall，零外部运行时依赖，windowsgui 无控制台。
package main

import (
	"fmt"
	"os"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	advapi32 = windows.NewLazySystemDLL("advapi32.dll")
	shell32  = windows.NewLazySystemDLL("shell32.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")
)

var (
	pFindWindowW      = user32.NewProc("FindWindowW")
	pMessageBoxW      = user32.NewProc("MessageBoxW")
	pRegDeleteTreeW   = advapi32.NewProc("RegDeleteTreeW")
	pSHGetFolderPathW = shell32.NewProc("SHGetFolderPathW")
	pSHFileOperationW = shell32.NewProc("SHFileOperationW")
	pGetEnvironmentVariableW = kernel32.NewProc("GetEnvironmentVariableW")
	pCreateProcessW   = kernel32.NewProc("CreateProcessW")
	pCreateToolhelp32Snapshot = kernel32.NewProc("CreateToolhelp32Snapshot")
	pProcess32FirstW  = kernel32.NewProc("Process32FirstW")
	pProcess32NextW   = kernel32.NewProc("Process32NextW")
)

const (
	hkeyCurrentUser = 0x80000001
	mbIconQuestion  uint32 = 0x00000020
	mbIconWarning   uint32 = 0x00000030
	mbIconInfo      uint32 = 0x00000040
	mbYesNo         uint32 = 0x00000004
	mbOK            uint32 = 0x00000000
	idYes           = 6
	appClass        = "TodoWidgetGoWnd"
	uninstallKey    = `Software\Microsoft\Windows\CurrentVersion\Uninstall\TodoPriorityWidget`
	installDirName  = `TodoPriorityWidget`
	appName         = `待办事项优先级提示窗`
)

func utf16ptr(s string) uintptr {
	p, _ := windows.UTF16FromString(s)
	return uintptr(unsafe.Pointer(&p[0]))
}

func msgBox(text string, flags uint32) int {
	r, _, _ := pMessageBoxW.Call(0, utf16ptr(text), utf16ptr(appName), uintptr(flags))
	return int(r)
}

// processEntryW：native PROCESSENTRY32W 的精确布局（Win64）——vtable 风险同 COM，错序=静默读错线程内存
type processEntryW struct {
	dwSize              uint32
	cntUsage            uint32
	th32ProcessID       uint32
	_                   uint32
	th32DefaultHeapID   uintptr
	th32ModuleID        uint32
	cntThreads          uint32
	th32ParentProcessID uint32
	pcPriClassBase      int32
	dwFlags             uint32
	szExeFile           [260]uint16
}

// runningApp：按进程名检测主程序（FindWindowW 类名检测在实测中跨进程返回 0，
// 现象已记录；进程枚举更稳，且动力质量与窗口可见性无关）。
// 纪律：检测到运行 → 提示/退出，绝不强杀。
func runningApp() bool {
	const th32csSnapProcess = 0x00000002
	snap, _, _ := pCreateToolhelp32Snapshot.Call(th32csSnapProcess, 0)
	if snap == ^uintptr(0) || snap == 0 {
		return false
	}
	defer func() {
		kernel32.NewProc("CloseHandle").Call(snap)
	}()
	pe := processEntryW{dwSize: uint32(unsafe.Sizeof(processEntryW{}))}
	r, _, _ := pProcess32FirstW.Call(snap, uintptr(unsafe.Pointer(&pe)))
	for r != 0 {
		name := windows.UTF16ToString(pe.szExeFile[:])
		if strings.EqualFold(name, appName+".exe") {
			return true
		}
		r, _, _ = pProcess32NextW.Call(snap, uintptr(unsafe.Pointer(&pe)))
	}
	return false
}

func targetDir() string {
	local := os.Getenv("LOCALAPPDATA")
	if local == "" {
		return ""
	}
	return local + `\Programs\` + installDirName
}

const (
	csidlDesktopDirectory = 0x0000
	csidlStartMenu        = 0x000B
)

func shFolder(csidl uint32) string {
	var buf [260]uint16
	hr, _, _ := pSHGetFolderPathW.Call(0, uintptr(csidl), 0, 0, uintptr(unsafe.Pointer(&buf[0])))
	if hr != 0 {
		return ""
	}
	return windows.UTF16ToString(buf[:])
}

// shfileopstructw：native SHFILEOPSTRUCTW 的精确字段布局（Win64）
// hwnd(8) wFunc(4)+pad(4) pFrom(8) pTo(8) fFlags(2)+pad(6) fAnyOpAborted(4)+pad(4) hNameMaps(8) lpszProgress(8)
type shfileopstructw struct {
	hwnd               uintptr
	wFunc              uint32
	_                  uint32
	pFrom              uintptr
	pTo                uintptr
	fFlags             uint16
	_                  [6]byte
	fAnyOperationsAborted uint32
	_                  uint32
	hNameMaps          uintptr
	lpszProgressTitle  uintptr
}

// shellDelete：SHFileOperationW 整目录/文件删除（静默；pFrom 需双 NUL 结尾）
func shellDelete(path string) bool {
	p, _ := windows.UTF16FromString(path)
	buf := make([]uint16, len(p)+1) // UTF16FromString 已含 1 个 NUL，再补 1 = 双 NUL
	copy(buf, p)
	op := shfileopstructw{
		hwnd:   0,
		wFunc:  3, // FO_DELETE
		pFrom:  uintptr(unsafe.Pointer(&buf[0])),
		pTo:    0,
		fFlags: 0x0004 | 0x0010 | 0x0400, // FOF_NOCONFIRMATION | FOF_SILENT | FOF_NOERRORUI
	}
	r, _, _ := pSHFileOperationW.Call(uintptr(unsafe.Pointer(&op)))
	return r == 0
}

// regUninstallDelete：删 HKCU 卸载项整树
func regUninstallDelete() bool {
	r, _, _ := pRegDeleteTreeW.Call(uintptr(hkeyCurrentUser), utf16ptr(uninstallKey))
	return r == 0
}

// spawnDelayedRmdir：本进程退出后由隐藏 cmd 延迟删除安装目录（含自身 exe）。
// cmd = Windows 系统自带组件（非打包级外部依赖，README 披露）；非管理员下这是
// 唯一可靠的自删路径（MoveFileEx DELAY_UNTIL_REBOOT 需写 HKLM，被 UAC 挡住）。
func spawnDelayedRmdir(dir string) {
	var comspec [260]uint16
	w, _, _ := pGetEnvironmentVariableW.Call(utf16ptr("COMSPEC"), uintptr(unsafe.Pointer(&comspec[0])), 260)
	if w == 0 {
		return
	}
	cmd := windows.UTF16ToString(comspec[:w])
	line := fmt.Sprintf("/c ping -n 2 -w 700 127.0.0.1 > nul & rd /s /q \"%s\"", dir)
	cmdline, _ := windows.UTF16FromString(`"` + cmd + `" ` + line)
	const createNoWindow = 0x08000000
	var si syscall.StartupInfo
	si.Cb = uint32(unsafe.Sizeof(si))
	var pi syscall.ProcessInformation
	pCreateProcessW.Call(0, uintptr(unsafe.Pointer(&cmdline[0])), 0, 0, 0, createNoWindow, 0, 0,
		uintptr(unsafe.Pointer(&si)), uintptr(unsafe.Pointer(&pi)))
}

func main() {
	// --silent：自动化实测静默通道（跳过确认弹窗；运行检测照常——不杀进程原则不变）
	silent := false
	for _, a := range os.Args {
		if a == "--silent" {
			silent = true
		}
	}
	dir := targetDir()
	if dir == "" {
		if silent {
			fmt.Println("no LOCALAPPDATA")
		}
		os.Exit(1)
	}
	if runningApp() {
		if silent {
			fmt.Println("app running")
		} else {
			msgBox("待办事项优先级提示窗正在运行。\n请先退出（右上角 × → 确认退出）后再卸载。", mbIconWarning|mbOK)
		}
		os.Exit(2)
	}
	if !silent {
		if msgBox("确定卸载待办事项优先级提示窗（1.0beta）吗？\n\n注：用户数据（%APPDATA%\\"+appName+"-Go）不会被删除。",
			mbIconQuestion|mbYesNo) != idYes {
			os.Exit(0)
		}
	}

	regUninstallDelete()
	_ = shellDelete(shFolder(csidlStartMenu) + `\Programs\` + appName + `.lnk`)
	_ = shellDelete(shFolder(csidlDesktopDirectory) + `\` + appName + `.lnk`)
	if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
		_ = shellDelete(dir)
	}

	spawnDelayedRmdir(dir)
	if !silent {
		msgBox("卸载完成。\n\n注册表项、快捷方式与程序文件已清除；\n您的待办数据保留在 %APPDATA%\\"+appName+"-Go，原样不动。", mbIconInfo|mbOK)
	} else {
		fmt.Println("uninstall ok")
	}
}
