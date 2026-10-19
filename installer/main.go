//go:build windows

// 待办事项优先级提示窗 · 安装器（对外 1.0beta）
//
// 交付形态：单文件安装器 exe（windowsgui、零控制台、零运行时依赖）。
// 载荷（主程序 exe + assets 全量 + uninstall.exe）由 go:embed 打包进本体。
// 安装动作（v1，全部用户级）：
//   1. 解出 %LOCALAPPDATA%\Programs\TodoPriorityWidget\（主程序以中文名落盘）
//   2. 写 HKCU 卸载注册表项（DisplayName / DisplayVersion / UninstallString 等，registry 包）
//   3. 开始菜单快捷方式（IShellLinkW+IPersistFile；vtable 序已按 SDK/ReactOS 核对：
//      SetWorkingDirectory=9 / SetIconLocation=17 / SetPath=20 / IPersistFile::Save=6）
//   4. 桌面快捷方式（可选勾选，默认否）
//   5. 完成后：[立即运行]（默认勾）+ [完成]
// 覆盖安装：文件/注册表/快捷方式直接覆写（v1 不做版本比较）。
package main

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

//go:embed payload/app.exe
var appBin []byte

//go:embed payload/uninstall.exe
var uninstallBin []byte

//go:embed all:payload/assets
var assetsFS embed.FS

var (
	appDisplayName = `待办事项优先级提示窗`
	appVersionStr  = "1.0beta"
	uninstallRegKey = `Software\Microsoft\Windows\CurrentVersion\Uninstall\TodoPriorityWidget`
)

// ---- Win32 侧（照 app-go/win32.go 手法） ----

var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")
	shell32  = windows.NewLazySystemDLL("shell32.dll")
	ole32    = windows.NewLazySystemDLL("ole32.dll")
)

var (
	pRegisterClassExW = user32.NewProc("RegisterClassExW")
	pCreateWindowExW  = user32.NewProc("CreateWindowExW")
	pDefWindowProcW   = user32.NewProc("DefWindowProcW")
	pGetMessageW      = user32.NewProc("GetMessageW")
	pTranslateMessage = user32.NewProc("TranslateMessage")
	pDispatchMessageW = user32.NewProc("DispatchMessageW")
	pPostQuitMessage  = user32.NewProc("PostQuitMessage")
	pSendMessageW     = user32.NewProc("SendMessageW")
	pSetForegroundWin = user32.NewProc("SetForegroundWindow")
	pDestroyWindow    = user32.NewProc("DestroyWindow")
	pSetFocus         = user32.NewProc("SetFocus")

	pGetModuleHandleW = kernel32.NewProc("GetModuleHandleW")
	pLoadImageW       = kernel32.NewProc("LoadImageW")
	pGetModuleFileW   = kernel32.NewProc("GetModuleFileNameW")

	pShellExecuteW = shell32.NewProc("ShellExecuteW")
	pSHGetFolderP  = shell32.NewProc("SHGetFolderPathW")
)

const (
	wmSetText  = 0x000C
	wmCommand  = 0x0111
	wmSetIcon  = 0x0080
	wmClose    = 0x0010
	wmDestroy  = 0x0002
	bmSetCheck = 0x00F1
	bmGetCheck = 0x00F0

	wsCaption   = 0x00C00000
	wsSysMenu   = 0x00080000
	wsVisible   = 0x10000000
	wsChild     = 0x40000000
	wsMinBox    = 0x00020000
	wsTabstop   = 0x00010000
	wsGrp       = 0x00020000
	bsDefpush   = 0x00000001
	bsPushbtn   = 0x00000000
	bsCheck     = 0x00000002
	esAutoLeft  = 0x00000080
	esReadonly  = 0x00000800

	bstUncheckd = 0x0000
	bstChk      = 0x0002

	imageIcon      = 1
	lrLoadFromFile = 0x00000010

	csidlDesktop = 0x0000
	csidlStartM  = 0x000B
)

type wndClassExW struct {
	cbSize        uint32
	style         uint32
	lpfnWndProc   uintptr
	cbClsExtra    int32
	cbWndExtra    int32
	hInstance     windows.Handle
	hIcon         windows.Handle
	hCursor       windows.Handle
	hbrBackground windows.Handle
	lpszMenuName  *uint16
	lpszClassName *uint16
	hIconSm       windows.Handle
}

type msgStruct struct {
	hwnd    windows.Handle
	message uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	pt      point256
	lPriv   uint32
}

type point256 struct{ x, y int32 }

var (
	installClass = "TodoInstallerWnd"
	hwndMain     windows.Handle
	statStatus   windows.Handle
	btnInst      windows.Handle
	btnDone      windows.Handle
	chkRun       windows.Handle
	chkDesk      windows.Handle
	installed    bool
	targetDst    string
)

func utf16ptr(s string) uintptr {
	p, _ := windows.UTF16FromString(s)
	return uintptr(unsafe.Pointer(&p[0]))
}

func setStatus(s string) {
	pSendMessageW.Call(uintptr(statStatus), uintptr(wmSetText), 0, utf16ptr(s))
	time.Sleep(90 * time.Millisecond)
}

// ---- 文件落盘（windows.CreateFile / windows.WriteFile 封装，免手搓 proc） ----

func writeFile(path string, data []byte) error {
	p, _ := windows.UTF16FromString(path)
	h, err := windows.CreateFile(&p[0], windows.GENERIC_WRITE, 0, nil,
		windows.CREATE_ALWAYS, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	defer windows.CloseHandle(h)
	if len(data) > 0 {
		if _, err = windows.Write(h, data); err != nil {
			return fmt.Errorf("write %s: %w", path, err)
		}
	}
	return nil
}

func mkDir(path string) error { return os.MkdirAll(path, 0o755) }

// ---- 注册表（x/sys/windows/registry 封装） ----

func writeRegistry(exePath, dir string) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, uninstallRegKey, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("CreateKey: %w", err)
	}
	defer k.Close()
	k.SetStringValue("DisplayName", appDisplayName)
	k.SetStringValue("DisplayVersion", appVersionStr)
	k.SetStringValue("Publisher", appDisplayName)
	k.SetStringValue("DisplayIcon", exePath)
	k.SetStringValue("InstallLocation", dir)
	k.SetStringValue("UninstallString", filepath.Join(dir, "uninstall.exe"))
	total := len(appBin) + len(uninstallBin) + int(payloadAssets())
	k.SetDWordValue("EstimatedSize", uint32(total/1024))
	k.SetDWordValue("NoModify", 1)
	k.SetDWordValue("NoRepair", 1)
	return nil
}

// ---- 快捷方式（COM；vtable 序已核对：SetWorkingDirectory=9 / SetIconLocation=17 / SetPath=20 / Save=6） ----

var (
	clsidShellLink  = windows.GUID{Data1: 0x00021401, Data2: 0x0000, Data3: 0x0000, Data4: [8]byte{0xC0, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x46}}
	iidShellLinkW   = windows.GUID{Data1: 0x000214F9, Data2: 0x0000, Data3: 0x0000, Data4: [8]byte{0xC0, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x46}}
	iidIPersistFile = windows.GUID{Data1: 0x0000010B, Data2: 0x0000, Data3: 0x0000, Data4: [8]byte{0xC0, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x46}}
)

func createShortcut(linkPath, targetPath, workDir string) error {
	co, _, _ := ole32.NewProc("CoInitializeEx").Call(0, 0x2 /*APARTMENTTHREADED*/) // S_OK=0 / S_FALSE=1 均可用
	_ = co
	defer ole32.NewProc("CoUninitialize").Call()

	var link unsafe.Pointer
	hr, _, err := ole32.NewProc("CoCreateInstance").Call(
		uintptr(unsafe.Pointer(&clsidShellLink)), 0, 0x1 /*INPROC_SERVER*/,
		uintptr(unsafe.Pointer(&iidShellLinkW)), uintptr(unsafe.Pointer(&link)))
	if hr != 0 {
		return fmt.Errorf("CoCreateInstance: hr=0x%x (%v)", hr, err)
	}
	// COM 双跳：&link→objaddr；obj 首字段→vtaddr；fn 表从 vtaddr 起。
	obj := *(*unsafe.Pointer)(unsafe.Pointer(&link))
	vt := (*[24]uintptr)(unsafe.Pointer(*(*uintptr)(obj)))

	syscall.SyscallN(vt[9], uintptr(obj), utf16ptr(workDir))              // SetWorkingDirectory
	syscall.SyscallN(vt[17], uintptr(obj), utf16ptr(targetPath), 0)       // SetIconLocation(exe, 0)
	hr, _, _ = syscall.SyscallN(vt[20], uintptr(obj), utf16ptr(targetPath)) // SetPath
	if hr != 0 {
		syscall.SyscallN(vt[2], uintptr(obj)) // Release
		return fmt.Errorf("SetPath: hr=0x%x", hr)
	}

	var pf unsafe.Pointer
	hr, _, _ = syscall.SyscallN(vt[0], uintptr(obj), uintptr(unsafe.Pointer(&iidIPersistFile)), uintptr(unsafe.Pointer(&pf)))
	if hr != 0 {
		syscall.SyscallN(vt[2], uintptr(obj))
		return fmt.Errorf("QI(IPersistFile): hr=0x%x", hr)
	}
	pfObj := *(*unsafe.Pointer)(unsafe.Pointer(&pf))
	pfvt := (*[10]uintptr)(unsafe.Pointer(*(*uintptr)(pfObj)))
	defer syscall.SyscallN(pfvt[2], uintptr(pfObj)) // Release
	hr, _, _ = syscall.SyscallN(pfvt[6], uintptr(pfObj), utf16ptr(linkPath), 1 /*fRemember*/)
	if hr != 0 {
		return fmt.Errorf("IPersistFile::Save: hr=0x%x (%v)", hr, err)
	}
	syscall.SyscallN(vt[2], uintptr(obj)) // Release
	return nil
}

// ---- 安装主流程 ----

func payloadAssets() int64 {
	var total int64
	_ = fs.WalkDir(assetsFS, ".", func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if fi, e := fs.Stat(assetsFS, path); e == nil {
				total += fi.Size()
			}
		}
		return nil
	})
	return total
}

func doInstall() error {
	dir := targetDst
	base := "payload/assets"
	exePath := filepath.Join(dir, appDisplayName+".exe")

	setStatus("0/5 创建安装目录…")
	if err := mkDir(dir); err != nil {
		return fmt.Errorf("创建目录: %w", err)
	}
	setStatus(fmt.Sprintf("1/5 写入主程序（%.2f MB）…", float64(len(appBin))/1048576))
	if err := writeFile(exePath, appBin); err != nil {
		return fmt.Errorf("写主程序: %w", err)
	}

	setStatus("2/5 写入 assets…")
	_ = fs.WalkDir(assetsFS, base, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(base, p)
		dst := filepath.Join(dir, "assets", rel)
		if d.IsDir() {
			return mkDir(dst)
		}
		data, e := assetsFS.ReadFile(p)
		if e != nil {
			return e
		}
		return writeFile(dst, data)
	})

	setStatus("3/5 写入卸载器…")
	if err := writeFile(filepath.Join(dir, "uninstall.exe"), uninstallBin); err != nil {
		return fmt.Errorf("写卸载器: %w", err)
	}

	setStatus("4/5 写注册表卸载项…")
	if err := writeRegistry(exePath, dir); err != nil {
		return fmt.Errorf("写注册表: %w", err)
	}

	setStatus("5/5 创建快捷方式…")
	sm := shFolder(csidlStartM)
	if sm != "" {
		if err := createShortcut(filepath.Join(sm, "Programs", appDisplayName+".lnk"), exePath, dir); err != nil {
			return fmt.Errorf("开始菜单快捷方式: %w", err)
		}
	}
	if isDlgButtonChecked(chkDesk) {
		dt := shFolder(csidlDesktop)
		if dt != "" {
			if err := createShortcut(filepath.Join(dt, appDisplayName+".lnk"), exePath, dir); err != nil {
				return fmt.Errorf("桌面快捷方式: %w", err)
			}
		}
	}
	return nil
}

func shFolder(csidl int) string {
	var buf [260]uint16
	hr, _, _ := pSHGetFolderP.Call(0, uintptr(csidl), 0, 0, uintptr(unsafe.Pointer(&buf[0])))
	if hr != 0 {
		return ""
	}
	return windows.UTF16ToString(buf[:])
}

// ---- 界面 ----

func setChecked(h windows.Handle, on bool) {
	pSendMessageW.Call(uintptr(h), uintptr(bmSetCheck), b2u(on), 0)
}

func isDlgButtonChecked(h windows.Handle) bool {
	r, _, _ := pSendMessageW.Call(uintptr(h), uintptr(bmGetCheck), 0, 0)
	return r == bstChk
}

func b2u(on bool) uintptr {
	if on {
		return bstChk
	}
	return bstUncheckd
}

func disableBtn(h windows.Handle) { pSendMessageW.Call(uintptr(h), 0x000A /*WM_ENABLE*/, 0, 0) }

func selfDir() string {
	var buf [420]uint16
	w, _, _ := pGetModuleFileW.Call(0, uintptr(unsafe.Pointer(&buf[0])), 420)
	p := windows.UTF16ToString(buf[:w])
	i := strings.LastIndexAny(p, `\/`)
	if i < 0 {
		return "."
	}
	return p[:i]
}

func loadIconFromFile(path string) windows.Handle {
	r, _, _ := pLoadImageW.Call(0, utf16ptr(path), uintptr(imageIcon), 0, 0, uintptr(lrLoadFromFile))
	return windows.Handle(r)
}

func instWndProc(h windows.Handle, msg uint32, wParam, lParam uintptr) uintptr {
	switch msg {
	case wmCommand:
		switch wParam & 0xFFFF {
		case 1001: // [开始安装]
			if installed {
				return 0
			}
			disableBtn(btnInst)
			go func() {
				err := doInstall()
				installed = err == nil
				if err != nil {
					setStatus("❌ 安装失败：" + err.Error())
					return
				}
				setStatus("✔ 安装完成：" + targetDst)
				// 显示完成区
				pShowWindowIFace(btnDone)
				setStatus("✔ 安装完成。点 [完成] 结束（立即运行已按勾选执行）。")
				if isDlgButtonChecked(chkRun) {
					pShellExecuteW.Call(0, utf16ptr("open"), utf16ptr(filepath.Join(targetDst, appDisplayName+".exe")), 0,
						utf16ptr(targetDst), 5 /*SW_SHOW*/)
				}
			}()
			return 0
		case 1002: // [完成]
			pDestroyWindow.Call(uintptr(h))
			return 0
		}
	case wmClose:
		pDestroyWindow.Call(uintptr(h))
		return 0
	case wmDestroy:
		pPostQuitMessage.Call(0)
		return 0
	}
	r, _, _ := pDefWindowProcW.Call(uintptr(h), uintptr(msg), wParam, lParam)
	return r
}

func pShowWindowIFace(h windows.Handle) {
	user32.NewProc("ShowWindow").Call(uintptr(h), 5 /*SW_SHOW*/)
	pSetFocus.Call(uintptr(h))
}

func createCtrl(cls, text string, style uintptr, x, y, w, h int32, id int) windows.Handle {
	hc, _, _ := pCreateWindowExW.Call(0, utf16ptr(cls), utf16ptr(text), style,
		uintptr(x), uintptr(y), uintptr(w), uintptr(h), uintptr(hwndMain), uintptr(id), 0, 0)
	return windows.Handle(hc)
}

func buildUI(hInst windows.Handle) {
	// 标题
	createCtrl("STATIC", appDisplayName+"  "+appVersionStr, wsChild|wsVisible|0x10 /*SS_NOTIFY? 实为 SS_LEFT 0x0 + 样式*/, 20, 18, 300, 24, 0)
	// 安装位置
	createCtrl("STATIC", "安装位置（用户级）：", wsChild|wsVisible, 20, 52, 140, 20, 0)
	userData := utf16ptr(targetDst)
	pCreateWindowExW.Call(0, utf16ptr("EDIT"), userData, uintptr(wsChild|wsVisible|esAutoLeft|esReadonly),
		160, 50, 336, 22, uintptr(hwndMain), uintptr(200), 0, 0)
	// 进度/状态
	statStatus = createCtrl("STATIC", "准备好后点 [开始安装]。", wsChild|wsVisible, 20, 92, 470, 40, 0)
	// 按钮
	btnInst = createCtrl("BUTTON", "开始安装", uintptr(wsChild|wsVisible|wsTabstop|bsDefpush), 20, 148, 148, 30, 1001)
	btnDone = createCtrl("BUTTON", "完成", uintptr(wsChild|wsTabstop|bsPushbtn), 340, 148, 120, 30, 1002)
	chkRun = createCtrl("BUTTON", "立即运行", uintptr(wsChild|wsVisible|wsTabstop|wsGrp|bsCheck), 20, 186, 120, 22, 1003)
	chkDesk = createCtrl("BUTTON", "在桌面创建快捷方式", uintptr(wsChild|wsVisible|wsTabstop|bsCheck), 150, 186, 190, 22, 1004)
	setChecked(chkRun, true)
	setChecked(chkDesk, false)
	pSendMessageW.Call(uintptr(btnDone), uintptr(wmSetText), 0, utf16ptr("完成"))
}

func main() {
	// --install：自动化实测静默通道（GUI 双击路径不受影响），仅用户级、与 GUI 同一实现
	silent := false
	for _, a := range os.Args {
		if a == "--install" {
			silent = true
		}
	}
	if silent {
		targetDst = targetDir()
		if targetDst == "" {
			fmt.Println("no LOCALAPPDATA")
			os.Exit(1)
		}
		if err := doInstall(); err != nil {
			fmt.Println("install error:", err)
			os.Exit(1)
		}
		fmt.Println("install ok:", targetDst)
		return
	}

	targetDst = targetDir()
	if targetDst == "" {
		// 无 LOCALAPPDATA 的极端环境：仍然允许以程序目录展示（安装按钮会报错）
		exe, _ := os.Executable()
		targetDst = filepath.Join(selfDir(), "Programs", "TodoPriorityWidget")
		_ = exe
	}

	hInstRaw, _, _ := pGetModuleHandleW.Call(0)
	hInst := windows.Handle(hInstRaw)

	clsName, _ := windows.UTF16FromString(installClass)
	var wc wndClassExW
	wc.cbSize = uint32(unsafe.Sizeof(wc))
	wc.lpfnWndProc = windows.NewCallback(instWndProc)
	wc.hInstance = hInst
	wc.hCursor = cursorArrow()
	wc.lpszClassName = &clsName[0]
	if atom, _, err := pRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); atom == 0 {
		failBox(fmt.Sprintf("RegisterClassExW: %v", err))
		return
	}

	// 图标：从内嵌 assets 提取 icon.ico 到 %TEMP% 再 LoadImage（LR_LOADFROMFILE）
	icoPath := filepath.Join(os.TempDir(), "todo-widget-installer.ico")
	if tmpIcon, err := assetsFS.ReadFile("payload/assets/icon.ico"); err == nil {
		if werr := writeFile(icoPath, tmpIcon); werr == nil {
			defer os.Remove(icoPath)
		}
	}
	icon := loadIconFromFile(icoPath)

	wnd, _, err := pCreateWindowExW.Call(0, utf16ptr(installClass),
		utf16ptr(appDisplayName+" "+appVersionStr+" 安装"),
		uintptr(wsCaption|wsSysMenu|wsMinBox|wsVisible),
		440, 220, 530, 270,
		0, 0, uintptr(hInst), 0)
	if wnd == 0 {
		failBox(fmt.Sprintf("CreateWindowExW: %v", err))
		return
	}
	hwndMain = windows.Handle(wnd)
	if icon != 0 {
		pSendMessageW.Call(wnd, uintptr(wmSetIcon), 1 /*ICON_BIG*/, uintptr(icon))
		pSendMessageW.Call(wnd, uintptr(wmSetIcon), 2 /*ICON_SMALL*/, uintptr(icon))
	}
	buildUI(hInst)
	pSetFocus.Call(uintptr(btnInst))
	pSetForegroundWin.Call(wnd)

	var m msgStruct
	for {
		r, _, _ := pGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(r) <= 0 {
			break
		}
		pTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		pDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}
}

func cursorArrow() windows.Handle {
	hc, _, _ := user32.NewProc("LoadCursorW").Call(0, uintptr(0x00007F00 /*IDC_ARROW*/))
	return windows.Handle(hc)
}

func failBox(text string) {
	user32.NewProc("MessageBoxW").Call(0, utf16ptr(text), utf16ptr(appDisplayName), uintptr(0x30|0x0))
}

func targetDir() string {
	local := os.Getenv("LOCALAPPDATA")
	if local == "" {
		return ""
	}
	return local + `\Programs\TodoPriorityWidget`
}
