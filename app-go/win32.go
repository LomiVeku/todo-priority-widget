//go:build windows

// win32.go —— Win32 封装：窗口类 / 消息循环 / DWM / DPI / 托盘 / 动画 / 计时器。
//
// 与 Electron 版的对应关系：
//
//	new BrowserWindow({ frame:false, transparent:true, alwaysOnTop:true, minimizable:true })
//	  → CreateWindowEx(WS_POPUP|WS_THICKFRAME|WS_MINIMIZEBOX|WS_SYSMENU)
//	    + WM_NCCALCSIZE 返回 0（去掉框架但保留可缩放边缘与任务栏最小化）
//	    + DwmExtendFrameIntoClientArea(-1,-1,-1,-1) + WebView2 背景 A=0
//	    + SetWindowPos(HWND_TOPMOST)
package main

import (
	"fmt"
	"math"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

/* ---------------------------------------------------------------- DLL 与过程 */

var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	shell32  = windows.NewLazySystemDLL("shell32.dll")
	dwmapi   = windows.NewLazySystemDLL("dwmapi.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")
)

var (
	pRegisterClassExW   = user32.NewProc("RegisterClassExW")
	pCreateWindowExW    = user32.NewProc("CreateWindowExW")
	pDefWindowProcW     = user32.NewProc("DefWindowProcW")
	pGetMessageW        = user32.NewProc("GetMessageW")
	pPeekMessageW       = user32.NewProc("PeekMessageW")
	pTranslateMessage   = user32.NewProc("TranslateMessage")
	pDispatchMessageW   = user32.NewProc("DispatchMessageW")
	pPostQuitMessage    = user32.NewProc("PostQuitMessage")
	pPostMessageW       = user32.NewProc("PostMessageW")
	pSendMessageW       = user32.NewProc("SendMessageW")
	pDestroyWindow      = user32.NewProc("DestroyWindow")
	pShowWindow         = user32.NewProc("ShowWindow")
	pSetWindowPos       = user32.NewProc("SetWindowPos")
	pGetWindowRect      = user32.NewProc("GetWindowRect")
	pGetModuleHandleW   = kernel32.NewProc("GetModuleHandleW")
	pCreateMutexW       = kernel32.NewProc("CreateMutexW")
	pSetTimer           = user32.NewProc("SetTimer")
	pKillTimer          = user32.NewProc("KillTimer")
	pLoadCursorW        = user32.NewProc("LoadCursorW")
	pLoadImageW         = user32.NewProc("LoadImageW")
	pGetDpiForWindow    = user32.NewProc("GetDpiForWindow")
	pSetProcessDpiCtx   = user32.NewProc("SetProcessDpiAwarenessContext")
	pSetProcessDPIAware = user32.NewProc("SetProcessDPIAware")
	pSetForegroundWindow = user32.NewProc("SetForegroundWindow")
	pBringWindowToTop    = user32.NewProc("BringWindowToTop")
	pPrintWindow         = user32.NewProc("PrintWindow")
	pGetForegroundWindow = user32.NewProc("GetForegroundWindow")
	pAttachThreadInput        = user32.NewProc("AttachThreadInput")        // T6：桌宠 open 置前的前台锁兜底
	pGetWindowThreadProcessId = user32.NewProc("GetWindowThreadProcessId") // T6：取前台窗口线程 ID
	pIsIconic           = user32.NewProc("IsIconic")
	pIsWindow           = user32.NewProc("IsWindow")
	pIsWindowVisible    = user32.NewProc("IsWindowVisible")
	pFindWindowW        = user32.NewProc("FindWindowW")
	pMonitorFromWindow  = user32.NewProc("MonitorFromWindow")
	pEnumDisplayMonitors = user32.NewProc("EnumDisplayMonitors")
	pGetMonitorInfoW    = user32.NewProc("GetMonitorInfoW")
	pReleaseCapture     = user32.NewProc("ReleaseCapture")
	pGetCursorPos       = user32.NewProc("GetCursorPos")
	pShellNotifyIconW   = shell32.NewProc("Shell_NotifyIconW")
	pCreatePopupMenu    = user32.NewProc("CreatePopupMenu")
	pAppendMenuW        = user32.NewProc("AppendMenuW")
	pTrackPopupMenu     = user32.NewProc("TrackPopupMenu")
	pDestroyMenu        = user32.NewProc("DestroyMenu")
	pGetMenuItemCount   = user32.NewProc("GetMenuItemCount") // 1.2.0 批：托盘菜单结构断言
	pGetMenuStringW     = user32.NewProc("GetMenuStringW")  // 1.2.0 批：托盘菜单结构断言
	pSetWindowRgn       = user32.NewProc("SetWindowRgn")
	pGetWindowRgn       = user32.NewProc("GetWindowRgn")
	pGetWindowLongPtrW  = user32.NewProc("GetWindowLongPtrW") // 1.2.0 批：任务栏样式断言（GWL_EXSTYLE）

	pCreateRoundRectRgn = gdi32.NewProc("CreateRoundRectRgn")
	pCreateRectRgn      = gdi32.NewProc("CreateRectRgn")
	pPtInRegion         = gdi32.NewProc("PtInRegion")

	pDwmExtendFrameIntoClientArea = dwmapi.NewProc("DwmExtendFrameIntoClientArea")
	pDwmSetWindowAttribute        = dwmapi.NewProc("DwmSetWindowAttribute")
)

/* ---------------------------------------------------------------- 常量 */

const (
	WS_POPUP       = 0x80000000
	WS_VISIBLE     = 0x10000000
	WS_THICKFRAME  = 0x00040000
	WS_MINIMIZEBOX = 0x00020000
	WS_SYSMENU     = 0x00080000

	WS_EX_APPWINDOW    = 0x00040000
	WS_EX_TOOLWINDOW   = 0x00000080 // 1.2.0 批：主窗/看图窗改用（不出任务栏、不进 Alt+Tab）

	HWND_TOPMOST   = ^uintptr(0) // (HWND)-1
	HWND_NOTOPMOST = ^uintptr(1) // (HWND)-2

	SWP_NOSIZE     = 0x0001
	SWP_NOMOVE     = 0x0002
	SWP_NOZORDER   = 0x0004
	SWP_NOACTIVATE = 0x0010
	SWP_FRAMECHANGED = 0x0020
	SWP_SHOWWINDOW = 0x0040

	SW_SHOW     = 5
	SW_MINIMIZE = 6
	SW_RESTORE  = 9

	WM_DESTROY       = 0x0002
	WM_SIZE          = 0x0005
	WM_CLOSE         = 0x0010
	WM_GETMINMAXINFO = 0x0024
	WM_DISPLAYCHANGE = 0x007E
	WM_NCCALCSIZE    = 0x0083
	WM_NCHITTEST     = 0x0084
	WM_NCLBUTTONDOWN = 0x00A1
	WM_MOVE          = 0x0003
	WM_MOVING        = 0x0216
	WM_EXITSIZEMOVE  = 0x0232
	WM_DPICHANGED    = 0x02E0
	WM_TIMER         = 0x0113
	WM_APP           = 0x8000

	HTCLIENT      = 1
	HTCAPTION     = 2
	HTLEFT        = 10
	HTRIGHT       = 11
	HTTOP         = 12
	HTTOPLEFT     = 13
	HTTOPRIGHT    = 14
	HTBOTTOM      = 15
	HTBOTTOMLEFT  = 16
	HTBOTTOMRIGHT = 17

	IMAGE_ICON      = 1
	LR_LOADFROMFILE = 0x00000010

	MONITOR_DEFAULTTONEAREST = 2

	IDC_ARROW = 32512

	DWMWA_WINDOW_CORNER_PREFERENCE = 33
	DWMWCP_ROUND                   = 2

	NIM_ADD    = 0
	NIM_MODIFY = 1
	NIM_DELETE = 2
	NIF_MESSAGE = 1
	NIF_ICON   = 2
	NIF_TIP    = 4

	WM_LBUTTONUP = 0x0202
	WM_RBUTTONUP = 0x0205

	TPM_RIGHTBUTTON = 0x0002
	TPM_RETURNCMD   = 0x0100
	TPM_NONOTIFY    = 0x0080
	MF_STRING       = 0x00000000
	MF_SEPARATOR    = 0x00000800
)

// 应用自定义消息
const (
	msgAttachWebView = WM_APP + 1 // WebView2 控制器重试
	msgTrayIcon      = WM_APP + 2 // 托盘回调
	msgDragStart     = WM_APP + 3 // 渲染层请求开始拖动
	msgQuitAll       = WM_APP + 4 // 托盘退出
)

// 固定计时器 ID（WM_TIMER wParam）
const (
	timerSnap     = 1 // 移动停顿 240ms → 吸附判定
	timerGeometry = 2 // 移动/缩放结束 600ms → 保存窗口几何
	timerRetract  = 3 // 吸附后 900ms → 自动缩入
	timerHoverOut = 4 // 鼠标移出 400ms → 缩回
	timerAnim     = 5 // 动画步进 16ms
	timerClose    = 6 // 关闭宽限 300ms（等渲染层最后一次落盘）
)

/* ---------------------------------------------------------------- 结构体 */

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

type rectI struct {
	left, top, right, bottom int32
}

func (r rectI) w() int32 { return r.right - r.left }
func (r rectI) h() int32 { return r.bottom - r.top }

type pointI struct {
	x, y int32
}

type msgStruct struct {
	hwnd     windows.Handle
	message  uint32
	wParam   uintptr
	lParam   uintptr
	time     uint32
	pt       pointI
	lPrivate uint32
}

type minMaxInfo struct {
	ptReserved, ptMaxSize, ptMaxPosition, ptMinTrackSize, ptMaxTrackSize pointI
}

type monitorInfoW struct {
	cbSize    uint32
	rcMonitor rectI
	rcWork    rectI
	dwFlags   uint32
}

type margins struct {
	cxLeftWidth, cxRightWidth, cyTopHeight, cyBottomHeight int32
}

type notifyIconDataW struct {
	cbSize           uint32
	hWnd             windows.Handle
	uID              uint32
	uFlags           uint32
	uCallbackMessage uint32
	hIcon            windows.Handle
	szTip            [128]uint16
	dwState          uint32
	dwStateMask      uint32
	szInfo           [256]uint16
	uVersion         uint32
	szInfoTitle      [64]uint16
	dwInfoFlags      uint32
}

/* ---------------------------------------------------------------- 显示器枚举 */

// Display 对应 Electron 的 screen.Display（bounds / workArea 均为物理像素）
type Display struct {
	ID       uintptr
	Bounds   rectI
	WorkArea rectI
}

func listDisplays() []Display {
	var out []Display
	cb := windows.NewCallback(func(hmon windows.Handle, _ windows.Handle, _ uintptr) uintptr {
		mi := monitorInfoW{cbSize: uint32(unsafe.Sizeof(monitorInfoW{}))}
		if r, _, _ := pGetMonitorInfoW.Call(uintptr(hmon), uintptr(unsafe.Pointer(&mi))); r != 0 {
			out = append(out, Display{ID: uintptr(hmon), Bounds: mi.rcMonitor, WorkArea: mi.rcWork})
		}
		return 1 // 继续枚举
	})
	pEnumDisplayMonitors.Call(0, 0, cb, 0)
	return out
}

func primaryDisplay() Display {
	// 枚举结果里 (0,0) 所在的视为主屏；EnumDisplayMonitors 的顺序通常主屏在前，
	// 但不保证，这里按原点判断兜底。
	ds := listDisplays()
	if len(ds) == 0 {
		return Display{}
	}
	for _, d := range ds {
		if d.Bounds.left <= 0 && d.Bounds.top <= 0 && d.Bounds.right > 0 && d.Bounds.bottom > 0 {
			return d
		}
	}
	return ds[0]
}

func monitorOf(h windows.Handle) *Display {
	r, _, _ := pMonitorFromWindow.Call(uintptr(h), MONITOR_DEFAULTTONEAREST)
	if r == 0 {
		return nil
	}
	for _, d := range listDisplays() {
		if d.ID == r {
			return &d
		}
	}
	return nil
}

/* ---------------------------------------------------------------- 计时器 */

var (
	timerMu sync.Mutex
	timerFn = map[uintptr]func(){}
)

func setTimer(id uintptr, ms int, fn func()) {
	timerMu.Lock()
	timerFn[id] = fn
	timerMu.Unlock()
	go func() {
		time.Sleep(time.Duration(ms) * time.Millisecond)
		if hwnd != 0 {
			pPostMessageW.Call(uintptr(hwnd), msgTimerFired, id, 0)
		}
	}()
}

func killTimer(id uintptr) {
	timerMu.Lock()
	delete(timerFn, id)
	timerMu.Unlock()
	pKillTimer.Call(0, id)
}

func fireTimer(id uintptr) {
	timerMu.Lock()
	fn, ok := timerFn[id]
	timerMu.Unlock()
	if ok {
		killTimer(id)
		fn()
	}
}

// 动态一次性计时器（after）：从 1000 号起分配，避免与固定用途 ID 冲突。
//
// 实现说明：不用 SetTimer——WM_TIMER 在消息队列里优先级最低（排在 posted 消息、
// WM_PAINT 之后），WebView2 的 IPC 消息连续到达时会把它无限饿死。
// 这里用 goroutine sleep + PostMessage：sleep 不触碰 COM（无线程约束），
// 回调经由消息队列回到主线程执行，与窗口/COM 的线程亲和保持一致。
var dynSeq uintptr = 1000

const msgTimerFired = WM_APP + 6

func after(ms int, fn func()) {
	timerMu.Lock()
	dynSeq++
	id := dynSeq
	timerFn[id] = fn
	timerMu.Unlock()
	go func() {
		time.Sleep(time.Duration(ms) * time.Millisecond)
		if hwnd != 0 {
			pPostMessageW.Call(uintptr(hwnd), msgTimerFired, id, 0)
		}
	}()
}

var (
	pGetSystemMetrics = user32.NewProc("GetSystemMetrics")
)

const (
	SM_CXSCREEN = 0
	SM_CYSCREEN = 1
)

/* ---------------------------------------------------------------- 应用状态 */

type dockState struct {
	side      string // left / right / top
	displayID uintptr
	expanded  rectI // 展开状态的目标位置（物理像素）
}

var (
	hwnd        windows.Handle
	dpiScale    = 1.0
	alwaysOnTop = true
	dockEnabled = true
	dock        *dockState
	retracted   bool
	hoverInside bool

	suppressMoveUntil time.Time
	animating         bool

	animFrom rectI
	animTo   rectI
	animEnd  time.Time
	animStart time.Time

	closing   bool
	trayAdded bool
	trayIcon  windows.Handle
	theStore  *Store

	// 1.2.0 批：关闭二次确认状态（仅自检断言消费；行为见 requestClose/beginCloseFlow）
	closeProceeded      bool // requestClose 已被确认路径调用过（真关或自检标记）
	selftestExitPending bool // 自检收尾置位 → requestClose 绕过自检闸真退出
)

const (
	dipW = 380 // 与 Electron 版一致的 DIP 尺寸
	dipH = 580
	dipMinW = 280
	dipMinH = 340
	stripDIP = 10.0   // 缩入后留在屏内的宽度（DIP）
	snapThresholdDIP = 26.0
)

func dipToPhys(v float64) int32 { return int32(math.Round(v * dpiScale)) }

// panelRadiusDIP 与 styles.css 的 --radius-panel 保持一致（14px）。
const panelRadiusDIP = 14.0

// applyRoundRegion 用圆角区域裁掉窗口四角。
//
// 背景：原方案依赖 DwmExtendFrameIntoClientArea(-1,-1,-1,-1) 的「DWM glass」做每像素透明，
// 但该技法自 Windows 8 起失效——DWM 改用默认不透明色填充扩展框，导致四角出现白色楔形、
// 且整窗基色不透明。改用 SetWindowRgn 直接给窗口设圆角区域：DWM 没有「角」可填，白角即消失。
//
// 注意：SetWindowRgn 成功后区域归系统所有，不能再 DeleteObject，否则会留下悬空句柄。
func applyRoundRegion(h windows.Handle) {
	if h == 0 {
		return
	}
	var wr rectI
	pGetWindowRect.Call(uintptr(h), uintptr(unsafe.Pointer(&wr)))
	w := int(wr.right - wr.left)
	hh := int(wr.bottom - wr.top)
	if w <= 0 || hh <= 0 {
		return
	}
	// 圆角直径须为偶数，CreateRoundRectRgn 用左右径+上下径描述椭圆角
	d := dipToPhys(panelRadiusDIP) * 2
	if d < 2 {
		d = 2
	}
	// CreateRoundRectRgn(x1,y1,x2,y2, wEllipse, hEllipse)：右上/右下坐标是「含右不含右」式，
	// 传 0,0,w+1,h+1 覆盖整个窗口客户区。
	rgn, _, _ := pCreateRoundRectRgn.Call(0, 0, uintptr(w+1), uintptr(hh+1),
		uintptr(d), uintptr(d))
	if rgn == 0 {
		return
	}
	// redraw=1：立即重绘窗口（区域变化必须重绘）
	pSetWindowRgn.Call(uintptr(h), rgn, 1)
}

// cornerWedgePoints：返回四角「窗口区内、圆角弧外」的楔形取样点序列（窗口坐标）。
//
// 几何说明：圆角半径 R 的窗口，沿角点对角线，楔形（在窗口矩形内但弧外）的有效区间
// 是 d ∈ (0, R·(1-1/√2)] ≈ (0, 0.293R]。对 R≈18px 即 d ∈ (0, 5.3]。
// 故取 d = 1..R/3 多点扫描（含边界保险），任一落在 region 外即证该角已裁。
func cornerWedgePoints(w, h int, r int) [][2]int {
	if r < 4 {
		r = 4
	}
	maxD := r / 3
	if maxD < 1 {
		maxD = 1
	}
	var out [][2]int
	// 沿对角线取 d = 1..maxD 的中点附近，避开 0（角点本身在弧上）与 >maxD（已入实体）
	for d := 1; d <= maxD; d++ {
		out = append(out,
			[2]int{d, d},                     // 左上
			[2]int{w - 1 - d, d},             // 右上
			[2]int{d, h - 1 - d},             // 左下
			[2]int{w - 1 - d, h - 1 - d},     // 右下
		)
	}
	return out
}

// regionClippedCorners 用 GetWindowRgn + PtInRegion 直接询问系统：
// 「窗口四角的楔形取样点是否属于本窗口区域」。
//
// 这是对修复本身的**结构性验证**，与背景色/DPI/截屏无关：
//   · 已 applyRoundRegion → 楔形点在 region 外 → clipped=4
//   · 未裁圆角（退回完整矩形）→ 楔形点在 region 内 → clipped=0
//
// 返回 (clippedCount, total)；total=0 表示无法判定（句柄缺失等）。
func regionClippedCorners(h windows.Handle) (int, int) {
	if h == 0 {
		return 0, 0
	}
	var wr rectI
	pGetWindowRect.Call(uintptr(h), uintptr(unsafe.Pointer(&wr)))
	w := int(wr.right - wr.left)
	hh := int(wr.bottom - wr.top)
	if w <= 2 || hh <= 2 {
		return 0, 0
	}
	// GetWindowRgn(hwnd, hRgn) 要求 hRgn 是预先创建好的 region 句柄（接收用）。
	probRgn, _, _ := pCreateRectRgn.Call(0, 0, 1, 1)
	if probRgn == 0 {
		return 0, 0
	}
	defer pDeleteObject.Call(probRgn)
	// 成功返回 region 类型：NULLREGION=1 / SIMPLEREGION=2 / COMPLEXREGION=3；
	// 失败返回 ERROR=0（窗口未设区域）。
	typ, _, _ := pGetWindowRgn.Call(uintptr(h), probRgn)
	if typ == 0 {
		return 0, 0
	}
	r := int(dipToPhys(panelRadiusDIP))
	pts := cornerWedgePoints(w, hh, r)
	// 每个角占 pts 的 1/4 段；只要该段内**存在**一个点在 region 外，即证该角已裁。
	per := len(pts) / 4
	clipped := 0
	for c := 0; c < 4; c++ {
		hit := false
		for i := c * per; i < (c+1)*per; i++ {
			in, _, _ := pPtInRegion.Call(probRgn, uintptr(int32(pts[i][0])), uintptr(int32(pts[i][1])))
			if in == 0 {
				hit = true
				break
			}
		}
		if hit {
			clipped++
		}
	}
	return clipped, 4
}

/* ---------------------------------------------------------------- 窗口创建 */

const windowClassName = "TodoWidgetGoWnd"
const windowTitle = "待办事项优先级提示窗"

func enablePerMonitorDPI() {
	// DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2 = ^uintptr(3) ((HANDLE)-4)
	const pmv2 = ^uintptr(3)
	if r, _, _ := pSetProcessDpiCtx.Call(pmv2); r != 0 {
		return
	}
	_, _, _ = pSetProcessDPIAware.Call() // 老系统兜底：系统级感知
}

func loadAppIcon() windows.Handle {
	p := assetPath("icon.ico")
	ptr, err := windows.UTF16PtrFromString(p)
	if err != nil {
		return 0
	}
	h, _, _ := pLoadImageW.Call(0, uintptr(unsafe.Pointer(ptr)), IMAGE_ICON, 0, 0, LR_LOADFROMFILE)
	return windows.Handle(h)
}

func createMainWindow(startX, startY, startW, startH int32, show bool) error {
	hInst, _, _ := pGetModuleHandleW.Call(0)
	className := windows.StringToUTF16Ptr(windowClassName)
	title := windows.StringToUTF16Ptr(windowTitle)
	icon := loadAppIcon()

	wc := wndClassExW{
		cbSize:        uint32(unsafe.Sizeof(wndClassExW{})),
		lpfnWndProc:   windows.NewCallback(wndProc),
		hInstance:     windows.Handle(hInst),
		hIcon:         icon,
		hIconSm:       icon,
		hCursor:       cursorArrow(),
		lpszClassName: className,
		// hbrBackground = 0：不设背景刷，避免系统在透明区域填色
	}
	if atom, _, err := pRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); atom == 0 {
		return fmt.Errorf("RegisterClassExW: %v", err)
	}

	var style uint32 = WS_POPUP | WS_THICKFRAME | WS_MINIMIZEBOX | WS_SYSMENU
	if show {
		style |= WS_VISIBLE
	}
	h, _, err := pCreateWindowExW.Call(
		WS_EX_TOOLWINDOW, // 1.2.0 批：不出任务栏、不进 Alt+Tab；最小化后经托盘还原（原 WS_EX_APPWINDOW）
		uintptr(unsafe.Pointer(className)),
		uintptr(unsafe.Pointer(title)),
		uintptr(style),
		uintptr(int32(startX)), uintptr(int32(startY)), uintptr(int32(startW)), uintptr(int32(startH)),
		0, 0, hInst, 0,
	)
	if h == 0 {
		return fmt.Errorf("CreateWindowExW: %v", err)
	}
	hwnd = windows.Handle(h)

	if icon != 0 {
		// 大小两套图标：1.2.0 批起主窗为 WS_EX_TOOLWINDOW，任务栏/Alt-Tab 中不再出现，
		// 此处设图标供标题栏/托盘沿用资源（语义保持：托盘与窗标题仍可见）
		pSendMessageW.Call(h, 0x0080 /*WM_SETICON*/, 1 /*ICON_BIG*/, uintptr(icon))
	}

	// 每像素透明：整窗交给 DWM 合成
	m := margins{-1, -1, -1, -1}
	if hr, _, _ := pDwmExtendFrameIntoClientArea.Call(h, uintptr(unsafe.Pointer(&m))); hr != 0 {
		appLogf("WARN DwmExtendFrameIntoClientArea 0x%x（非致命）", hr)
	}
	// 圆角（Win11 生效；圆角之外区域本来就是透明的，视觉无影响）
	pref := int32(DWMWCP_ROUND)
	pDwmSetWindowAttribute.Call(h, DWMWA_WINDOW_CORNER_PREFERENCE, uintptr(unsafe.Pointer(&pref)), unsafe.Sizeof(pref))

	// 圆角裁窗：覆写窗口区域，消灭 DWM 在扩展框四角填充的白色楔形（Win8+ 必需）
	applyRoundRegion(windows.Handle(h))

	if r, _, _ := pGetDpiForWindow.Call(h); r != 0 {
		dpiScale = float64(r) / 96.0
	} else {
		// 老系统回退：主屏 LOGPIXELSX
		hdc, _, _ := user32.NewProc("GetDC").Call(0)
		if hdc != 0 {
			r2 := gdiGetDeviceCaps(hdc, 88)
			if r2 > 0 {
				dpiScale = float64(r2) / 96.0
			}
			user32.NewProc("ReleaseDC").Call(0, hdc)
		}
	}
	return nil
}

var gdi32 = windows.NewLazySystemDLL("gdi32.dll")
var pGetDeviceCaps = gdi32.NewProc("GetDeviceCaps")

func gdiGetDeviceCaps(hdc, idx uintptr) int32 {
	r, _, _ := pGetDeviceCaps.Call(hdc, idx)
	return int32(r)
}

func cursorArrow() windows.Handle {
	c, _, _ := pLoadCursorW.Call(0, IDC_ARROW)
	return windows.Handle(c)
}

func windowRect() rectI {
	var r rectI
	pGetWindowRect.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&r)))
	return r
}

func setWindowBounds(b rectI) {
	pSetWindowPos.Call(
		uintptr(hwnd), 0,
		uintptr(b.left), uintptr(b.top), uintptr(b.w()), uintptr(b.h()),
		SWP_NOZORDER|SWP_NOACTIVATE,
	)
}

func setTopmost(top bool) {
	flag := uintptr(HWND_NOTOPMOST)
	if top {
		flag = HWND_TOPMOST
	}
	pSetWindowPos.Call(uintptr(hwnd), flag, 0, 0, 0, 0, SWP_NOMOVE|SWP_NOSIZE|SWP_NOACTIVATE)
}

func showMainWindow() {
	pShowWindow.Call(uintptr(hwnd), SW_SHOW)
	// ★ 窗口创建时是隐藏的，WebView2 控制器的可见性不会随 ShowWindow 自动恢复，
	//   必须显式 PutIsVisible(true)，否则 DOM 在跑但画面不合成（截出来是一块灰玻璃）
	if chromium != nil {
		_ = chromium.Show()
	}
	if alwaysOnTop {
		setTopmost(true)
	}
}

/* ---------------------------------------------------------------- 窗口过程 */

func wndProc(h windows.Handle, msg uint32, wParam, lParam uintptr) uintptr {
	switch msg {
	case WM_NCCALCSIZE:
		if wParam != 0 {
			return 0 // 客户区 = 整个窗口（无边框），保留 WS_THICKFRAME 的缩放能力
		}

	case WM_NCHITTEST:
		// 自行判定缩放边缘（8 物理像素）；缩入状态不响应缩放
		if !retracted {
			var pt pointI
			pt.x = int32(int16(lParam & 0xFFFF))
			pt.y = int32(int16((lParam >> 16) & 0xFFFF))
			wr := windowRect()
			const edge = 8
			onL := pt.x-wr.left < edge
			onR := wr.right-pt.x < edge
			onT := pt.y-wr.top < edge
			onB := wr.bottom-pt.y < edge
			switch {
			case onL && onT:
				return HTTOPLEFT
			case onL && onB:
				return HTBOTTOMLEFT
			case onR && onT:
				return HTTOPRIGHT
			case onR && onB:
				return HTBOTTOMRIGHT
			case onL:
				return HTLEFT
			case onR:
				return HTRIGHT
			case onT:
				return HTTOP
			case onB:
				return HTBOTTOM
			}
		}
		return HTCLIENT

	case WM_GETMINMAXINFO:
		mmi := (*minMaxInfo)(unsafe.Pointer(lParam))
		mmi.ptMinTrackSize = pointI{x: dipToPhys(dipMinW), y: dipToPhys(dipMinH)}
		return 0

	case WM_DPICHANGED:
		newDpi := int(wParam) & 0xFFFF
		dpiScale = float64(newDpi) / 96.0
		sug := (*rectI)(unsafe.Pointer(lParam))
		pSetWindowPos.Call(uintptr(h), 0,
			uintptr(sug.left), uintptr(sug.top), uintptr(sug.w()), uintptr(sug.h()),
			SWP_NOZORDER|SWP_NOACTIVATE)
		applyRoundRegion(h)
		resizeChromium()
		return 0

	case WM_SIZE:
		if wParam != 1 /* SIZE_MINIMIZED */ {
			applyRoundRegion(h)
			resizeChromium()
		}
		if wParam == 0 && !animating && dock != nil {
			// 用户缩放：重算展开位置（与 Electron 一致）
			if d := findDisplay(dock.displayID); d != nil {
				dock.expanded = expandedBoundsFor(dock.side, d.WorkArea, windowRect())
			}
		}
		scheduleGeometrySave()
		return 0

	case WM_MOVE, WM_MOVING:
		if !animating && time.Now().After(suppressMoveUntil) && !retracted {
			// 移动停顿 240ms → 吸附判定（对应 Electron 的 move 去抖）
			setTimer(timerSnap, 240, func() { snapNow() })
		}
		if msg == WM_MOVE {
			scheduleGeometrySave()
		}
		// WM_MOVING 时 lParam 指向拖动中的矩形，交给 DefWindowProc 无碍
		return 0

	case WM_EXITSIZEMOVE:
		killTimer(timerSnap)
		if !animating && time.Now().After(suppressMoveUntil) && !retracted {
			snapNow() // 拖动结束 → 判定吸附
		}
		scheduleGeometrySave()
		return 0

	case WM_TIMER:
		fireTimer(wParam)
		return 0

	case msgTimerFired:
		fireTimer(wParam)
		return 0

	case msgDragStart:
		// 进入系统拖动模态循环（等价于 -webkit-app-region: drag）
		pReleaseCapture.Call()
		pSendMessageW.Call(uintptr(h), WM_NCLBUTTONDOWN, HTCAPTION, 0)
		return 0

	case WM_DISPLAYCHANGE:
		onDisplaysChanged()
		return 0

	case msgTrayIcon:
		handleTray(uintptr(lParam))
		return 0

	case msgQuitAll:
		requestClose()
		return 0

	case msgTodoOpen:
		// T6：桌宠发 open → 置前（管道读协程只 PostMessage，窗口操作在 UI 线程执行，§5.4）
		bringToFront()
		return 0

	case WM_CLOSE:
		// 1.2.0 批：右上角 X / WM_CLOSE / Alt+F4 → 关闭二次确认通路（页面在桥上订阅 win:close-request）；
		// 页面未起（!webviewReady）由 beginCloseFlow 退化为直关，防"关不掉"。
		// 托盘「退出」不借此路（保持直关，tray-exit-direct 断言锚点）。
		beginCloseFlow()
		return 0

	case WM_DESTROY:
		removeTray()
		pPostQuitMessage.Call(0)
		return 0
	}
	r, _, _ := pDefWindowProcW.Call(uintptr(h), uintptr(msg), wParam, lParam)
	return r
}

/* ---------------------------------------------------------------- 关闭（含落盘宽限） */

// requestClose：Electron 版在 beforeunload 里有同步落盘；WebView2 的 postMessage
// 是异步的，所以销毁前留 300ms 宽限继续泵消息，让渲染层去抖中的最后一次保存送达。
// 1.2.0 批自检适配：自检模式下本函数只标记 closeProceeded 不真退（防自检自杀）；
// finishSelftest 收尾先置 selftestExitPending 再调本函数 = 绕过闸真退出。
func requestClose() {
	if closing {
		return
	}
	if selftestMode && !selftestExitPending {
		closeProceeded = true
		return
	}
	closing = true
	closeProceeded = true
	pipeStop() // T6：管道服务随主窗停止（监听句柄/连接/协程退出）
	viewerDestroyForClosing() // 独立看图窗随主窗一并关闭（第二批任务书 2.2）
	persistWindowState()
	killTimer(timerRetract)
	killTimer(timerHoverOut)
	killTimer(timerSnap)
	killTimer(timerAnim)
	animating = false
	setTimer(timerClose, 300, func() {
		pDestroyWindow.Call(uintptr(hwnd))
	})
}

// pipe.go 使用的消息与置前实现：msgTodoOpen（WM_APP+5）→ bringToFront()
const msgTodoOpen = WM_APP + 5

// bringToFront：桌宠 open 事件 → 窗口置前（§5.4，管道读协程经 PostMessage 回 UI 线程后执行）
func bringToFront() {
	if dock != nil {
		if retracted {
			expandNow() // 缩入态：展开
		} else {
			setBoundsProgrammatic(dock.expanded) // 未缩入：回展开位
		}
	}
	pShowWindow.Call(uintptr(hwnd), 9 /*SW_RESTORE*/)
	pShowWindow.Call(uintptr(hwnd), 5 /*SW_SHOW*/)
	if chromium != nil {
		_ = chromium.Show()
	}
	// 前台锁兜底：GetForegroundWindow → GetWindowThreadProcessId 返回值=前台线程 ID（注意：
	// 该函数第二参数是可选的进程 ID 输出，不是线程 ID——首版误把输出参数当线程 ID 用，兜底失效）
	cur := windows.GetCurrentThreadId()
	fgH, _, _ := pGetForegroundWindow.Call()
	fgThread, _, _ := pGetWindowThreadProcessId.Call(fgH, 0)
	attached := fgThread != 0 && fgThread != uintptr(cur)
	if attached {
		pAttachThreadInput.Call(uintptr(cur), uintptr(fgThread), 1)
	}
	pBringWindowToTop.Call(uintptr(hwnd))
	pSetForegroundWindow.Call(uintptr(hwnd))
	if attached {
		pAttachThreadInput.Call(uintptr(cur), uintptr(fgThread), 0)
	}
	focusWebView()
}

// pipeStop 的消息/句柄归属：见 pipe.go（requestClose 真退路径调用 pipeStop()）。

/* ---------------------------------------------------------------- 托盘 */

const trayUID = 1

const (
	trayCmdShow   = 1
	trayCmdUndock = 2
	trayCmdExit   = 3
)

func addTray() {
	if trayIcon == 0 {
		trayIcon = loadAppIcon()
	}
	if trayIcon == 0 {
		return
	}
	ni := notifyIconDataW{
		cbSize:           uint32(unsafe.Sizeof(notifyIconDataW{})),
		hWnd:             hwnd,
		uID:              trayUID,
		uFlags:           NIF_MESSAGE | NIF_ICON | NIF_TIP,
		uCallbackMessage: msgTrayIcon,
		hIcon:            trayIcon,
	}
	tip, _ := windows.UTF16FromString(windowTitle)
	copy(ni.szTip[:], tip)
	if r, _, _ := pShellNotifyIconW.Call(NIM_ADD, uintptr(unsafe.Pointer(&ni))); r != 0 {
		trayAdded = true
	}
}

func removeTray() {
	if !trayAdded {
		return
	}
	ni := notifyIconDataW{cbSize: uint32(unsafe.Sizeof(notifyIconDataW{})), hWnd: hwnd, uID: trayUID}
	pShellNotifyIconW.Call(NIM_DELETE, uintptr(unsafe.Pointer(&ni)))
	trayAdded = false
}

func handleTray(lParam uintptr) {
	switch lParam {
	case WM_LBUTTONUP:
		restoreFromTray()
	case WM_RBUTTONUP:
		showTrayMenu()
	}
}

func restoreFromTray() {
	pShowWindow.Call(uintptr(hwnd), SW_RESTORE)
	pShowWindow.Call(uintptr(hwnd), SW_SHOW)
	pSetForegroundWindow.Call(uintptr(hwnd))
	if dock != nil && !retracted {
		setWindowBounds(dock.expanded)
	}
	focusWebView()
}

// buildTrayMenu：构建托盘右键菜单（1.2.0 批自 showTrayMenu 拆出，供自检直读结构）。
// 返回菜单句柄；项序固定：显示/还原、取消缩入、分隔线、退出。
func buildTrayMenu() uintptr {
	menu, _, _ := pCreatePopupMenu.Call()
	if menu == 0 {
		return 0
	}
	t1, _ := windows.UTF16PtrFromString("显示 / 还原")
	t2, _ := windows.UTF16PtrFromString("取消缩入")
	t4, _ := windows.UTF16PtrFromString("退出")
	pAppendMenuW.Call(menu, MF_STRING, trayCmdShow, uintptr(unsafe.Pointer(t1)))
	pAppendMenuW.Call(menu, MF_STRING, trayCmdUndock, uintptr(unsafe.Pointer(t2)))
	pAppendMenuW.Call(menu, MF_SEPARATOR, 0, 0)
	pAppendMenuW.Call(menu, MF_STRING, trayCmdExit, uintptr(unsafe.Pointer(t4)))
	return menu
}

// menuStringAt：自检断言用——按位置取菜单项文本（MF_BYPOSITION）；取不到返回空串。
func menuStringAt(menu uintptr, pos int) string {
	if menu == 0 {
		return ""
	}
	buf := make([]uint16, 64)
	n, _, _ := pGetMenuStringW.Call(menu, uintptr(pos), uintptr(unsafe.Pointer(&buf[0])),
		uintptr(len(buf)), 0x00000400 /*MF_BYPOSITION*/)
	if n == 0 {
		return ""
	}
	return windows.UTF16ToString(buf)
}

func showTrayMenu() {
	menu := buildTrayMenu()
	if menu == 0 {
		return
	}

	pSetForegroundWindow.Call(uintptr(hwnd))
	var pt pointI
	pGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	cmd, _, _ := pTrackPopupMenu.Call(menu, TPM_RIGHTBUTTON|TPM_RETURNCMD|TPM_NONOTIFY,
		uintptr(pt.x), uintptr(pt.y), 0, uintptr(hwnd), 0)
	pDestroyMenu.Call(menu)

	switch cmd {
	case trayCmdShow:
		restoreFromTray()
	case trayCmdUndock:
		undock()
		restoreFromTray()
	case trayCmdExit:
		trayExit()
	}
}

// trayExit：托盘「退出」唯一实现点——直调 requestClose()，**不弹确认**（1.2.0 批 B4 固化：
// 托盘退出保持直关；tray-exit-direct 断言锚定本函数，防托盘路径被误改走确认通路）。
func trayExit() {
	requestClose()
}

/* ---------------------------------------------------------------- 窗口几何与吸附接线 */

func currentWindowBounds() rectI { return windowRect() }

// setBoundsProgrammatic：程序化移动/缩放，抑制吸附判定（对应 main.js setBoundsProgrammatic）
func setBoundsProgrammatic(b rectI) {
	suppressMoveUntil = time.Now().Add(500 * time.Millisecond)
	wr := windowRect()
	if b.w() == 0 || b.h() == 0 {
		b.right = b.left + wr.w()
		b.bottom = b.top + wr.h()
	}
	minW, minH := dipToPhys(dipMinW), dipToPhys(dipMinH)
	if b.w() < minW {
		b.right = b.left + minW
	}
	if b.h() < minH {
		b.bottom = b.top + minH
	}
	setWindowBounds(b)
}

// expandedBoundsFor：依据吸附方向 + 工作区算出"展开"状态的位置（对齐 main.js：
// 左右吸附时保持当前 y——即 size.top，越界时钳制回工作区内）
func expandedBoundsFor(side string, wa rectI, size rectI) rectI {
	w, h := size.w(), size.h()
	y := clampI(size.top, wa.top, wa.bottom-h)
	switch side {
	case "left":
		return rectI{left: wa.left, top: y, right: wa.left + w, bottom: y + h}
	case "right":
		x := wa.right - w
		return rectI{left: x, top: y, right: x + w, bottom: y + h}
	case "top":
		x := clampI(size.left, wa.left, wa.right-w)
		return rectI{left: x, top: wa.top, right: x + w, bottom: wa.top + h}
	}
	return size
}

func clampI(v, lo, hi int32) int32 {
	if hi < lo {
		return lo
	}
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func findDisplay(id uintptr) *Display {
	for _, d := range listDisplays() {
		if d.ID == id {
			return &d
		}
	}
	return nil
}

// snapNow：吸附判定（对齐 main.js snapNow）
func snapNow() {
	if !dockEnabled {
		return
	}
	b := windowRect()
	found := computeSnap(b, listDisplays(), dipToPhys(snapThresholdDIP))
	if found == nil {
		if dock != nil {
			undock()
		}
		return
	}
	d := findDisplay(found.displayID)
	if d == nil {
		return
	}
	// 修复批：角落归上时 computeSnap 已给出含横向外推 gap 的权威贴边位，直接采用；
	// 常规三边仍走 expandedBoundsFor（维持原行为）
	exp := expandedBoundsFor(found.side, d.WorkArea, windowRect())
	if found.overrideBounds {
		exp = found.bounds
	}
	dock = &dockState{
		side:      found.side,
		displayID: found.displayID,
		expanded:  exp,
	}
	retracted = false
	setBoundsProgrammatic(dock.expanded)
	broadcastDock()
	scheduleRetract(900)
}

// scheduleRetract：吸附后自动缩入（900ms，对齐 main.js RETRACT_DELAY）
func scheduleRetract(ms int) {
	killTimer(timerRetract)
	if dock == nil || !dockEnabled || hoverInside {
		return
	}
	setTimer(timerRetract, ms, func() { retractNow() })
}

// retractNow：缩入侧边，只留 strip 在屏内
func retractNow() {
	if dock == nil || !dockEnabled || hoverInside {
		return
	}
	strip := dipToPhys(stripDIP)
	target := retractedBounds(dock.side, dock.expanded, strip)
	retracted = true
	broadcastDock()
	animateTo(target, 170)
}

// expandNow：悬停 / 手动展开
func expandNow() {
	if dock == nil {
		return
	}
	killTimer(timerRetract)
	if !retracted {
		setBoundsProgrammatic(dock.expanded)
		return
	}
	retracted = false
	broadcastDock()
	animateTo(dock.expanded, 170)
}

func undock() {
	killTimer(timerRetract)
	dock = nil
	retracted = false
	broadcastDock()
}

// animateTo：约 170ms easeOutCubic 动画（SetTimer 步进，不阻塞消息循环）
func animateTo(target rectI, ms int) {
	animFrom = windowRect()
	animTo = target
	animStart = time.Now()
	animEnd = animStart.Add(time.Duration(ms) * time.Millisecond)
	animating = true
	suppressMoveUntil = animEnd.Add(400 * time.Millisecond)
	setTimer(timerAnim, 16, animTick)
}


func animTick() {
	total := float64(animEnd.Sub(animStart))
	t := 1.0
	if total > 0 {
		elapsed := float64(time.Since(animStart)) / total
		if elapsed < 1 {
			t = elapsed
		}
	}
	e := 1 - math.Pow(1-t, 3)
	cur := rectI{
		left:   lerpI(animFrom.left, animTo.left, e),
		top:    lerpI(animFrom.top, animTo.top, e),
		right:  lerpI(animFrom.right, animTo.right, e),
		bottom: lerpI(animFrom.bottom, animTo.bottom, e),
	}
	setWindowBounds(cur)
	if t >= 1 {
		animating = false
		suppressMoveUntil = time.Now().Add(500 * time.Millisecond)
		setWindowBounds(animTo)
		return
	}
	setTimer(timerAnim, 16, animTick)
}

func lerpI(a, b int32, t float64) int32 {
	return int32(math.Round(float64(a) + (float64(b)-float64(a))*t))
}

// ensureVisible：显示器被拔掉后把窗口拉回可见区域（对齐 main.js ensureVisible）
func ensureVisible() {
	b := windowRect()
	for _, d := range listDisplays() {
		ix := minI(b.right, d.Bounds.right) - maxI(b.left, d.Bounds.left)
		iy := minI(b.bottom, d.Bounds.bottom) - maxI(b.top, d.Bounds.top)
		if ix > 40 && iy > 40 {
			return
		}
	}
	pri := primaryDisplay()
	wa := pri.WorkArea
	setBoundsProgrammatic(rectI{
		left: wa.right - b.w() - 40, top: wa.top + 60,
		right: wa.right - b.w() - 40 + b.w(), bottom: wa.top + 60 + b.h(),
	})
	dock = nil
	retracted = false
	broadcastDock()
}

func minI(a, b int32) int32 { if a < b { return a }; return b }
func maxI(a, b int32) int32 { if a > b { return a }; return b }

// onDisplaysChanged：显示器增删 / 变化（对齐 main.js onDisplaysChanged）
func onDisplaysChanged() {
	if dock != nil {
		check := validateDock(dock.side, dock.displayID, listDisplays())
		if !check.valid {
			dock = nil
			retracted = false
			ensureVisible()
			broadcastDock()
		} else {
			dock.expanded = expandedBoundsFor(dock.side, check.display.WorkArea, windowRect())
			if retracted {
				setBoundsProgrammatic(retractedBounds(dock.side, dock.expanded, dipToPhys(stripDIP)))
			} else {
				setBoundsProgrammatic(dock.expanded)
			}
			broadcastDock()
		}
	} else {
		ensureVisible()
	}
	broadcastWinState()
	pushDisplays()
}

// scheduleGeometrySave：移动/缩放结束后 600ms 落盘窗口几何
func scheduleGeometrySave() {
	if closing {
		return
	}
	killTimer(timerGeometry)
	setTimer(timerGeometry, 600, func() { persistWindowState() })
}

// persistWindowState：写独立 window.json（吸附中不写 x/y，下次启动不沿用贴边坐标）
func persistWindowState() {
	if hwnd == 0 {
		return
	}
	b := windowRect()
	st := map[string]interface{}{
		"width":  int(math.Round(float64(b.w()) / dpiScale)),
		"height": int(math.Round(float64(b.h()) / dpiScale)),
		"alwaysOnTop": alwaysOnTop,
		"dockEnabled": dockEnabled,
		"savedAt": time.Now().UnixMilli(),
	}
	if dock == nil {
		// 以 DIP 存（与 Electron 版一致），启动时再乘缩放
		st["x"] = int(math.Round(float64(b.left) / dpiScale))
		st["y"] = int(math.Round(float64(b.top) / dpiScale))
	}
	if theStore != nil {
		theStore.SaveWindowState(st)
	}
}

/* ---------------------------------------------------------------- 单实例 */

func acquireSingleInstance() bool {
	name, _ := windows.UTF16PtrFromString("Local\\" + windowClassName)
	h, _, err := pCreateMutexW.Call(0, 0, uintptr(unsafe.Pointer(name)))
	if h == 0 {
		return false
	}
	// ERROR_ALREADY_EXISTS = 183：已有实例在跑
	if errno, ok := err.(windows.Errno); ok && errno == 183 {
		return false
	}
	return true
}

func focusExistingInstance() {
	cls, _ := windows.UTF16PtrFromString(windowClassName)
	if h, _, _ := pFindWindowW.Call(uintptr(unsafe.Pointer(cls)), 0); h != 0 {
		pShowWindow.Call(h, SW_RESTORE)
		pSetForegroundWindow.Call(h)
	}
}
