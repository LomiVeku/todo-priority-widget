//go:build windows

// imageviewer.go —— 独立看图窗口（第二批任务书）。
//
// 设计要点（按任务书红线）：
//   - 原生 Win32 窗口 + GDI 绘制，**不带 WebView2**（内存增量远低于一个浏览器实例）
//   - 与主窗共用既有的消息循环（同一线程）；不另起线程泵消息
//   - 近黑实底（mimo 前置审美方向，透明底会干扰看图本身）
//   - 底部状态条复用灯箱 lightbox-bar 的视觉族（RGB 24,27,34），显示 `文件名 · WxHpx · zoom%`
//   - 交互：左键拖动平移（光标 SIZEALL）、滚轮以鼠标为锚点缩放（10%~800%）、
//     双击切换「适应窗口 / 100%」、Esc 关闭、可拉伸、带系统标题栏、非置顶
//   - png/jpg/gif 标准库解码；其余格式返回失败（渲染层 toast 提示）
//   - 主窗关闭时一并销毁看图窗并释放位图
package main

import (
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"math"
	"os"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

/* ---------------------------------------------------------------- 常量与过程（本文件专用，不与 win32.go 重复定义） */

const (
	WS_OVERLAPPEDWINDOW = 0x00CF0000 // CAPTION|MAXIMIZEBOX|MINIMIZEBOX|SYSMENU|THICKFRAME
	WM_PAINT            = 0x000F
	WM_ERASEBKGND       = 0x0014
	WM_KEYDOWN          = 0x0100
	WM_SETCURSOR        = 0x0020
	WM_MOUSEMOVE        = 0x0200
	WM_PRINT            = 0x0317
	WM_LBUTTONDOWN      = 0x0201
	WM_LBUTTONDBLCLK    = 0x0203
	WM_MOUSEWHEEL       = 0x020A
	CS_DBLCLKS          = 0x0008

	HALFTONE = 4

	IDC_SIZEALL = 32646
	IDC_HAND    = 32649
	VK_ESCAPE   = 0x1B

	DT_SINGLELINE = 0x0020
	DT_VCENTER    = 0x0004
	DT_END_ELLIPSIS = 0x8000
	DT_RIGHT      = 0x0002
	DT_LEFTX      = 0x0000

	TRANSPARENT_BKMODE = 4
	DEFAULT_CHARSET    = 1
	FW_NORMAL          = 400
	FW_SEMIBOLD        = 600
	CLEARTYPE_QUALITY  = 5
)

var (
	gdi32sys = windows.NewLazySystemDLL("gdi32.dll")
	psapisys = windows.NewLazySystemDLL("psapi.dll")

	pBeginPaint         = user32.NewProc("BeginPaint")
	pEndPaint           = user32.NewProc("EndPaint")
	pFillRect           = user32.NewProc("FillRect")
	pGetClientRect      = user32.NewProc("GetClientRect")
	pSetWindowTextW     = user32.NewProc("SetWindowTextW")
	pSetCapture         = user32.NewProc("SetCapture")
	pInvalidateRect     = user32.NewProc("InvalidateRect")
	pSetFocus          = user32.NewProc("SetFocus")
	pGetFocus          = user32.NewProc("GetFocus")
	pDrawTextW          = user32.NewProc("DrawTextW")
	pSetCursorP         = user32.NewProc("SetCursor")
	pCreateSolidBrush   = gdi32sys.NewProc("CreateSolidBrush")
	pSetStretchBltMode  = gdi32sys.NewProc("SetStretchBltMode")
	pStretchBlt         = gdi32sys.NewProc("StretchBlt")
	pSetTextColor       = gdi32sys.NewProc("SetTextColor")
	pSetBkMode          = gdi32sys.NewProc("SetBkMode")
	pGetProcessMemoryInfo = psapisys.NewProc("GetProcessMemoryInfo")
	pCreateFontW           = gdi32sys.NewProc("CreateFontW")
	pGetTextExtentPoint32W = gdi32sys.NewProc("GetTextExtentPoint32W")
)

const viewerClassName = "TodoGoImageWindow"
const viewerStatusH = 26.0
const viewerHintText = "滚轮缩放 · 双击切适应/100% · Esc 关闭"

type viewerBMIHeader struct {
	biSize          uint32
	biWidth         int32
	biHeight        int32
	biPlanes        uint16
	biBitCount      uint16
	biCompression   uint32
	biSizeImage     uint32
	biXPelsPerMeter int32
	biYPelsPerMeter int32
	biClrUsed       uint32
	biClrImportant  uint32
}

type viewerBMI struct {
	header viewerBMIHeader
}

type viewerPaintStruct struct {
	hdc        windows.Handle
	fErase     uint32
	rcPaint    rectI
	fRestore   uint32
	fIncUpdate uint32
	reserved   [96]byte // PAINTSTRUCT 官方布局留 32 字节 rgbReserved，取大余量避免越界
}

// PROCESS_MEMORY_COUNTERS（x64）
type processMemoryCounters struct {
	cb                  uint32
	pageFaultCount      uint32
	peakWorkingSetSize  uintptr
	workingSetSize      uintptr
	quotaPeakPagedUsage uintptr
	quotaPagedUsage     uintptr
	quotaPeakNonPaged   uintptr
	quotaNonPagedUsage  uintptr
	pagefileUsage       uintptr
	peakPagefileUsage   uintptr
	privUsage           uintptr // PrivateUsage：私有提交字节（≈私有内存）
}

/* ---------------------------------------------------------------- 状态 */

type imageViewer struct {
	hwnd     windows.Handle
	bgBmp    windows.Handle // 预合成的 32bpp BGRX（近黑底）
	imgW     int32
	imgH     int32
	name     string
	zoom     float64
	fitMode  bool
	panX     float64 // 图片原点相对客户区的偏移（物理像素）
	panY     float64
	clientW  int32
	clientH  int32
	dragging bool
	lastX    int32
	lastY    int32

	// 底部状态条布局（第四批：可见提示行 + 可点 zoom）
	statusLeft   rectI  // 左侧 名称·尺寸 文本 rect（绘制与省略判定用）
	statusZoom   rectI  // 右侧 zoom% 可点 rect（高=状态条整条 ≥24px）
	statusHint   rectI  // 提示文本 rect（窄窗省略）
	hintVisible  bool
	hoverZoom    bool // 鼠标悬停 zoom% → 颜色增亮 + 手型
	mx, my       int32 // 最近一次客户端鼠标位置（hover 判定）
	lastZoomClick time.Time // 双击防抖：两次快速点击只生效一次
	statusFont   windows.Handle  // 13px 状态条字体（缓存）
	hintFont     windows.Handle  // 11px 提示字体
}

/** 状态条字体（13px/11px 各一，按 dpi 缓存；WM_DESTROY 时清理） */
func viewerEnsureFont(px int32) windows.Handle {
	hp := -int32(math.Round(float64(px) * float64(dpiScale)))
	cache := &viewer.statusFont
	if px <= 11 {
		cache = &viewer.hintFont
	}
	if *cache != 0 {
		return *cache
	}
	f, _, _ := pCreateFontW.Call(
		uintptr(hp), 0, 0, 0, FW_NORMAL, 0, 0, 0, DEFAULT_CHARSET, 0, 0, CLEARTYPE_QUALITY, 0, 0)
	*cache = windows.Handle(f)
	return *cache
}

/* 状态条布局纯函数（自检可直测）：
   zoom% 固定右锚 56px 宽；提示文本紧贴其左（DT_RIGHT）；左侧名称优先占位。
   空间不足时**先省提示**（任务书：提示可省略，名称/像素/缩放完整可见优先）。 */
func layoutStatus(clientW, clientH int32, zoomW, hintW float64) (zoom, hint rectI, hintVisible bool) {
	statusH := int32(math.Round(viewerStatusH * float64(dpiScale)))
	top := clientH - statusH
	if zoomW <= 0 {
		zoomW = 56
	}
	zoom = rectI{
		left:   clientW - 8 - int32(math.Round(zoomW)),
		top:    top,
		right:  clientW - 8,
		bottom: clientH,
	}
	if hintW <= 0 {
		return zoom, rectI{left: 0, top: top, right: zoom.left - 12, bottom: clientH}, false
	}
	// 名称至少要放得下 ~220px（含边距），再算提示空间
	const nameMin = 220
	avail := zoom.left - 12 - nameMin
	hintVisible = hintW <= float64(avail) && avail > 60
	hint = rectI{
		left:   zoom.left - 12 - int32(math.Round(hintW)),
		top:    top,
		right:  zoom.left - 12,
		bottom: clientH,
	}
	if !hintVisible {
		hint = rectI{}
	}
	return zoom, hint, hintVisible
}

/** zoom% 命中判定（第四批任务书 2：可点缩放） */
func viewerHitsZoom(mx, my int32) bool {
	r := viewer.statusZoom
	return mx >= r.left && mx < r.right && my >= r.top && my < r.bottom
}

var viewer imageViewer

/* viewerInputLocked：自检期间置 true，使看图窗的**真实鼠标输入**（移动/按键/滚轮/双击）
   全部被忽略。原因：看图窗是独立 Win32 窗口，自检运行时自然人鼠标若恰好悬停其上，
   OS 会把真实的 WM_MOUSEMOVE/WM_MOUSEWHEEL 投递给它，其 lParam 携带真实光标坐标 →
   直接改写 viewer.zoom/panX/panY，污染后续断言的确定性（且不可复现）。
   与 webview.go 里屏蔽页面真实 hover 事件的 hoverInside 同源思路：自检只信 Go 侧
   直接调用（viewerZoomAt/viewerClampPan/viewerToggleFit），不信硬件输入。 */
var viewerInputLocked bool
var viewerClassRegistered bool
var viewerCursorSizeAll windows.Handle
var viewerCursorHand windows.Handle

/* ---------------------------------------------------------------- 对外入口 */

/** 渲染层 bridge 调用：把媒体目录里的 name 文件开到独立看图窗（多图复用同一窗） */
func openImageViewer(name string) bool {
	if !viewerClassRegistered {
		if err := registerViewerClass(); err != nil {
			appLogf("viewer: 注册窗口类失败 %v", err)
			return false
		}
	}
	if viewerCursorSizeAll == 0 {
		c, _, _ := pLoadCursorW.Call(0, IDC_SIZEALL)
		viewerCursorSizeAll = windows.Handle(c)
		ch2, _, _ := pLoadCursorW.Call(0, IDC_HAND)
		viewerCursorHand = windows.Handle(ch2)
	}
	src := theStore.mediaPath(name)
	bmp, w, h, err := decodeToOpaqueBGRA(src)
	if err != nil {
		appLogf("viewer: 解码失败 %s: %v", name, err)
		return false
	}
	oldBmp := viewer.bgBmp
	viewer.bgBmp = bmp
	viewer.imgW = w
	viewer.imgH = h
	viewer.name = name
	if oldBmp != 0 {
		pDeleteObject.Call(uintptr(oldBmp))
	}
	if viewer.hwnd != 0 {
		if isWin, _, _ := pIsWindow.Call(uintptr(viewer.hwnd)); isWin != 0 {
			// 已开着：换图并重置为适应窗口
			t, _ := windows.UTF16FromString(viewerTitle())
			pSetWindowTextW.Call(uintptr(viewer.hwnd), uintptr(unsafe.Pointer(&t[0])))
			if iconic, _, _ := pIsIconic.Call(uintptr(viewer.hwnd)); iconic != 0 {
				pShowWindow.Call(uintptr(viewer.hwnd), SW_RESTORE)
			}
			viewerResetFit()
			pInvalidateRect.Call(uintptr(viewer.hwnd), 0, 0)
			return true
		}
	}

	hInst, _, _ := pGetModuleHandleW.Call(0)
	title, _ := windows.UTF16FromString(viewerTitle())
	clsName, _ := windows.UTF16FromString(viewerClassName)
	// 主窗在屏幕左上（吸附流），看图窗放右侧偏移，避免遮住主窗
	pr := primaryDisplay().WorkArea
	x := pr.left + (pr.w()-840)/2 + 220
	y := pr.top + 90
	h2, _, cerr := pCreateWindowExW.Call(
		WS_EX_TOOLWINDOW, // 1.2.0 批：看图窗同步不出任务栏（原 0，不带扩展样式）
		uintptr(unsafe.Pointer(&clsName[0])),
		uintptr(unsafe.Pointer(&title[0])),
		uintptr(uint32(WS_OVERLAPPEDWINDOW|WS_VISIBLE)), // 系统标题栏 / 最小化 / 最大化 / 关闭
		uintptr(int32(x)), uintptr(int32(y)), 840, 620,
		0, 0, hInst, 0,
	)
	if h2 == 0 {
		appLogf("viewer: CreateWindowExW 失败 %v", cerr)
		return false
	}
	viewer.hwnd = windows.Handle(h2)
	appLogf("viewer: 窗口已创建 hwnd=0x%x bmp=0x%x img=%vx%v", viewer.hwnd, viewer.bgBmp, viewer.imgW, viewer.imgH)
	// 修复批：窗口创建后即取焦点（滚轮/Esc 依赖焦点；否则消息被 WebView2 接走）
	pSetFocus.Call(uintptr(viewer.hwnd))
	// 初始化显式跑一次：WM_SIZE 会重设 client，但初始必须 fit（WM_SIZE 分支按 fitMode 分派，
	// 初始 fitMode=false 会只走 clamp——所以这里显式把初始档定为「适应窗口」）
	var cr rectI
	pGetClientRect.Call(uintptr(viewer.hwnd), uintptr(unsafe.Pointer(&cr)))
	viewer.clientW = cr.w()
	viewer.clientH = cr.h()
	viewerResetFit()
	appLogf("viewer: 初始 fit zoom=%.4f client=%vx%v", viewer.zoom, viewer.clientW, viewer.clientH)
	pInvalidateRect.Call(uintptr(viewer.hwnd), 0, 0)
	return true
}

func viewerTitle() string {
	return "图片查看 · " + viewer.name
}

/* ---------------------------------------------------------------- 窗口类 */

func registerViewerClass() error {
	hInst, _, _ := pGetModuleHandleW.Call(0)
	clsName, _ := windows.UTF16FromString(viewerClassName)
	wc := wndClassExW{
		cbSize:        uint32(unsafe.Sizeof(wndClassExW{})),
		style:         CS_DBLCLKS, // 需要接收双击消息
		lpfnWndProc:   windows.NewCallback(imageViewerWndProc),
		hInstance:     windows.Handle(hInst),
		hIcon:         loadAppIcon(),
		hCursor:       cursorArrow(),
		// hbrBackground = 0：创建时不擦灰底，WM_PAINT 里铺近黑实底
		lpszClassName: &clsName[0],
	}
	if atom, _, err := pRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); atom == 0 {
		return err
	}
	viewerClassRegistered = true
	return nil
}

func viewerClientRect() rectI {
	var r rectI
	pGetClientRect.Call(uintptr(viewer.hwnd), uintptr(unsafe.Pointer(&r)))
	return r
}

func viewerWindowRect() rectI {
	var r rectI
	pGetWindowRect.Call(uintptr(viewer.hwnd), uintptr(unsafe.Pointer(&r)))
	return r
}

/* ---------------------------------------------------------------- 缩放 / 平移（纯逻辑；自检直接调用这些函数验证） */

func viewerDisplaySize() (float64, float64) {
	return float64(viewer.imgW) * viewer.zoom, float64(viewer.imgH) * viewer.zoom
}

/** 适应窗口（10%~800% 内），随即居中 */
func viewerResetFit() {
	availW := float64(viewer.clientW)
	availH := float64(viewer.clientH) - viewerStatusH*float64(dpiScale)
	z := 1.0
	if viewer.imgW > 0 && availH > 0 && availW > 0 {
		zx := availW / float64(viewer.imgW)
		zy := availH / float64(viewer.imgH)
		z = math.Min(zx, zy)
	}
	viewer.zoom = viewerSanitizeZoom(z)
	viewer.fitMode = true
	viewerClampPan()
	appLogf("viewer: resetFit zoom=%.4f fit=%v client=%vx%v img=%vx%v", viewer.zoom, viewer.fitMode, viewer.clientW, viewer.clientH, viewer.imgW, viewer.imgH)
}

/** 缩放限制 10%–800%（任务书） */
func viewerSanitizeZoom(z float64) float64 {
	if z < 0.1 {
		z = 0.1
	}
	if z > 8.0 {
		z = 8.0
	}
	return z
}

/** 边界约束：图片小于窗口→强制居中（拖不出去）；大于窗口→clamp 在 [cw-d, 0] 内，不露空洞 */
func viewerClampPan() {
	dw, dh := viewerDisplaySize()
	availH := float64(viewer.clientH) - viewerStatusH*float64(dpiScale)
	if dw <= float64(viewer.clientW) || viewer.clientW <= 0 {
		viewer.panX = (float64(viewer.clientW) - dw) / 2
	} else {
		if viewer.panX > 0 {
			viewer.panX = 0
		}
		if viewer.panX < float64(viewer.clientW)-dw {
			viewer.panX = float64(viewer.clientW) - dw
		}
	}
	if dh <= availH || availH <= 0 {
		viewer.panY = (availH - dh) / 2
	} else {
		if viewer.panY > 0 {
			viewer.panY = 0
		}
		if viewer.panY < availH-dh {
			viewer.panY = availH - dh
		}
	}
}

/** 以 (mx,my)（客户区坐标）为锚点缩放：鼠标下的图片点保持在原屏幕位置 */
func viewerZoomAt(mx, my float64, targetZoom float64) {	targetZoom = viewerSanitizeZoom(targetZoom)
	old := viewer.zoom
	if math.Abs(targetZoom-old) < 1e-9 || old <= 0 || viewer.imgW == 0 {
		return
	}
	viewer.panX = mx - (mx-viewer.panX)*targetZoom/old
	viewer.panY = my - (my-viewer.panY)*targetZoom/old
	viewer.zoom = targetZoom
	viewer.fitMode = false
	viewerClampPan()
}

/** 双击：在「适应窗口」与「100% 原始大小」间切换 */
func viewerToggleFit() {
	if viewer.fitMode {
		viewer.fitMode = false
		viewer.zoom = 1.0
		viewerClampPan()
	} else {
		viewerResetFit()
	}
	pInvalidateRect.Call(uintptr(viewer.hwnd), 0, 0)
}

/** viewerWheelAt：自检专用合成滚轮入口——与 WM_MOUSEWHEEL 同语义但锚点由调用方给定，
   不经真实鼠标。viewerInputLocked 生效时，这是唯一能驱动滚轮缩放的路径。 */
func viewerWheelAt(clientMx, clientMy float64, delta int16) {
	factor := math.Pow(1.2, float64(delta)/120.0)
	viewerZoomAt(clientMx, clientMy, viewer.zoom*factor)
	pInvalidateRect.Call(uintptr(viewer.hwnd), 0, 0)
}

/* ---------------------------------------------------------------- 位图构建 */

// decodeToOpaqueBGRA：解码 png/jpg/gif → 一次性合成为 32bpp 顶向下 BGRX（近黑底），
// 返回 HBITMAP，供 WM_PAINT 里 SelectObject + StretchBlt（HALFTONE）。
// 非透明位图（合成为近黑实底）→ 无需 AlphaBlend，单层 Blt 即可。
func decodeToOpaqueBGRA(path string) (windows.Handle, int32, int32, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, 0, err
	}
	defer f.Close()
	img, _, err := image.Decode(f)
	if err != nil {
		return 0, 0, 0, err
	}
	bounds := img.Bounds()
	w, h := bounds.Dx(), bounds.Dy()
	if w <= 0 || h <= 0 {
		return 0, 0, 0, fmt.Errorf("空图像")
	}

	const bgR, bgG, bgB = 18, 18, 20 // 近黑实底（mimo 方向）
	hdr := viewerBMIHeader{
		biSize:        uint32(unsafe.Sizeof(viewerBMIHeader{})),
		biWidth:       int32(w),
		biHeight:      -int32(h), // 负高度 = 顶向下行序（与 image.Image 一致）
		biPlanes:      1,
		biBitCount:    32,
		biCompression: 0, // BI_RGB
	}
	bmi := viewerBMI{header: hdr}
	var bits uintptr
	hdcScreen, _, _ := pGetDC.Call(0)
	defer pReleaseDC.Call(0, hdcScreen)
	bmp, _, err2 := pCreateDIBSection.Call(hdcScreen, uintptr(unsafe.Pointer(&bmi)), 0, uintptr(unsafe.Pointer(&bits)), 0, 0)
	if bmp == 0 || bits == 0 {
		return 0, 0, 0, fmt.Errorf("CreateDIBSection: %v", err2)
	}
	// 逐像素 BGRX 合成（<1000 万像素，一次性开销可忽略）
	dst := ((*[1 << 28]byte)(unsafe.Pointer(bits)))[: w*4*h : w*4*h]
	for y := 0; y < h; y++ {
		row := dst[y*w*4 : (y+1)*w*4]
		for x := 0; x < w; x++ {
			r0, g0, b0, a0 := img.At(bounds.Min.X+x, bounds.Min.Y+y).RGBA() // 0..65535（预乘）
			i := x * 4
			a := float64(a0) / 65535.0
			row[i+0] = byte(uint32(float64(b0>>8)*a) + uint32(float64(bgB)*(1-a)))
			row[i+1] = byte(uint32(float64(g0>>8)*a) + uint32(float64(bgG)*(1-a)))
			row[i+2] = byte(uint32(float64(r0>>8)*a) + uint32(float64(bgR)*(1-a)))
			row[i+3] = 0xFF
		}
	}
	return windows.Handle(bmp), int32(w), int32(h), nil
}

/* ---------------------------------------------------------------- 窗口过程 */

func imageViewerWndProc(h windows.Handle, msg uint32, wParam, lParam uintptr) uintptr {
	switch msg {
	case WM_GETMINMAXINFO:
		mmi := (*minMaxInfo)(unsafe.Pointer(lParam))
		mmi.ptMinTrackSize = pointI{x: 320, y: 240} // 任务书建议的最小尺寸
		return 0

	case WM_SIZE:
		var cr rectI
		pGetClientRect.Call(uintptr(h), uintptr(unsafe.Pointer(&cr)))
		appLogf("viewer: WM_SIZE wParam=%v client=%vx%v bg=0x%x fit=%v", wParam, cr.w(), cr.h(), viewer.bgBmp, viewer.fitMode)
		viewer.clientW = cr.w()
		viewer.clientH = cr.h()
		if wParam == 0 && viewer.bgBmp != 0 {
			if viewer.fitMode {
				viewerResetFit() // 按当前缩放档重算布局
			} else {
				viewerClampPan()
			}
		}
		pInvalidateRect.Call(uintptr(h), 0, 0)
		return 0

	case WM_ERASEBKGND:
		return 1 // 禁默认擦除（WM_PAINT 全铺，防闪烁）

	case WM_PAINT:
		var ps viewerPaintStruct
		pBeginPaint.Call(uintptr(h), uintptr(unsafe.Pointer(&ps)))
		viewerPaint(ps.hdc)
		pEndPaint.Call(uintptr(h), uintptr(unsafe.Pointer(&ps)))
		return 0

	case WM_PRINT:
		// PrintWindow 契约：把完整客户区画到给定 DC（PW_RENDERFULLCONTENT 兜底也用同一绘制路径）
		appLogf("viewer: WM_PRINT 进入 hdc=0x%x", uintptr(lParam))
		viewerPaint(windows.Handle(lParam))
		return 1

	case WM_SETCURSOR:
		if viewer.dragging && viewerCursorSizeAll != 0 {
			pSetCursorP.Call(uintptr(viewerCursorSizeAll))
			return 1
		}
		// zoom% 可点区 → 手型（第四批：可点缩放的视觉反馈）
		if viewerHitsZoom(viewer.mx, viewer.my) && viewerCursorHand != 0 {
			pSetCursorP.Call(uintptr(viewerCursorHand))
			return 1
		}

	case WM_LBUTTONDOWN:
		if viewerInputLocked {
			return 0
		}
		viewer.mx = int32(int16(lParam & 0xFFFF))
		viewer.my = int32(int16((lParam >> 16) & 0xFFFF))
		viewer.lastX, viewer.lastY = viewer.mx, viewer.my
		// zoom% 点击 → 切「适应窗口 ↔ 100%」；400ms 防抖防双击双切
		if viewerHitsZoom(viewer.mx, viewer.my) {
			if time.Since(viewer.lastZoomClick) > 400*time.Millisecond {
				viewer.lastZoomClick = time.Now()
				viewerToggleFit()
				pInvalidateRect.Call(uintptr(h), 0, 0)
			}
			return 0
		}
		viewer.dragging = true
		pSetCapture.Call(uintptr(h))
		if viewerCursorSizeAll != 0 {
			pSetCursorP.Call(uintptr(viewerCursorSizeAll)) // 拖动中光标变 SIZEALL
		}
		return 0

	case WM_MOUSEMOVE:
		if viewerInputLocked {
			return 0
		}
		// 悬停补焦（修复批）：滚轮/Esc 都发给焦点窗口；看图窗创建后不取焦点、点按才聚焦——
		// 用户未先点击时滚轮会被 WebView2 接走。悬停即补焦（滚轮与 Esc 立即可用）。
		if f, _, _ := pGetFocus.Call(); f != uintptr(h) {
			pSetFocus.Call(uintptr(h))
		}
		viewer.mx = int32(int16(lParam & 0xFFFF))
		viewer.my = int32(int16((lParam >> 16) & 0xFFFF))
		if viewer.dragging {
			viewer.panX += float64(viewer.mx - viewer.lastX)
			viewer.panY += float64(viewer.my - viewer.lastY)
			viewer.lastX, viewer.lastY = viewer.mx, viewer.my
			viewerClampPan()
			if viewer.hoverZoom {
				viewer.hoverZoom = false // 拖动离开可点态
			}
			pInvalidateRect.Call(uintptr(h), 0, 0)
			return 0
		}
		// hover 变化 → zoom 色切换（颜色反馈，无动画——任务书另一项已排除）
		if hoverNow := viewerHitsZoom(viewer.mx, viewer.my); hoverNow != viewer.hoverZoom {
			viewer.hoverZoom = hoverNow
			pInvalidateRect.Call(uintptr(h), 0, 0)
		}
		return 0

	case WM_LBUTTONUP:
		if viewerInputLocked {
			return 0
		}
		if viewer.dragging {
			viewer.dragging = false
			pReleaseCapture.Call()
			pSetCursorP.Call(uintptr(cursorArrow()))
			return 0
		}

	case WM_LBUTTONDBLCLK:
		if viewerInputLocked {
			return 0
		}
		viewerToggleFit()
		return 0

	case WM_MOUSEWHEEL:
		// 自检期间屏蔽真实滚轮（其 lParam 锚点 = 真实光标位置，会污染 viewer 状态）。
		// 自检的合成滚轮用例改走 viewerWheelHook（见 selftest.go），不复用本分支。
		if viewerInputLocked {
			return 0
		}
		// wParam 高 16 位 = delta；lParam = 屏幕坐标锚点
		// 修复批：原实现缺 InvalidateRect —— 状态变了但界面从不更新（滚轮不生效根因之一）
		delta := int16((wParam >> 16) & 0xFFFF)
		wr := viewerWindowRect()
		clientMx := float64(int32(int16(lParam&0xFFFF)) - wr.left)
		clientMy := float64(int32(int16((lParam>>16)&0xFFFF)) - wr.top)
		factor := math.Pow(1.2, float64(delta)/120.0)
		viewerZoomAt(clientMx, clientMy, viewer.zoom*factor)
		appLogf("viewer: wheel delta=%d zoom=%.4f", delta, viewer.zoom)
		pInvalidateRect.Call(uintptr(h), 0, 0)
		return 0

	case WM_KEYDOWN:
		if viewerInputLocked {
			return 0
		}
		if wParam == VK_ESCAPE {
			pDestroyWindow.Call(uintptr(h))
			return 0
		}

	case WM_CLOSE:
		pDestroyWindow.Call(uintptr(h))
		return 0

	case WM_DESTROY:
		if viewer.bgBmp != 0 {
			pDeleteObject.Call(uintptr(viewer.bgBmp))
			viewer.bgBmp = 0
		}
		if viewer.statusFont != 0 {
			pDeleteObject.Call(uintptr(viewer.statusFont))
			viewer.statusFont = 0
		}
		if viewer.hintFont != 0 {
			pDeleteObject.Call(uintptr(viewer.hintFont))
			viewer.hintFont = 0
		}
		viewer.hwnd = 0
		viewer.dragging = false
		return 0
	}
	r, _, _ := pDefWindowProcW.Call(uintptr(h), uintptr(msg), wParam, lParam)
	return r
}

func viewerStatusText() string {
	return fmt.Sprintf("%s · %dx%d px · %d%%", viewer.name, viewer.imgW, viewer.imgH, int(math.Round(viewer.zoom*100)))
}

/** 看图窗完整绘制（近黑底 → HALFTONE拉伸位图 → 灯箱族状态条；WM_PAINT / WM_PRINT 共用） */
func viewerPaint(hdc windows.Handle) {
	var cr rectI
	pGetClientRect.Call(uintptr(viewer.hwnd), uintptr(unsafe.Pointer(&cr)))
	full := rectI{left: 0, top: 0, right: cr.w(), bottom: cr.h()}
	// 近黑实底全铺
	brush, _, _ := pCreateSolidBrush.Call(0x00141212) // COLORREF=0x00BBGGRR → RGB(18,18,20)
	pFillRect.Call(uintptr(hdc), uintptr(unsafe.Pointer(&full)), brush)
	pDeleteObject.Call(brush)
	if viewer.bgBmp != 0 && viewer.imgW > 0 {
		memDC, _, _ := pCreateCompatibleDC.Call(uintptr(hdc))
		oldObj, _, _ := pSelectObject.Call(uintptr(memDC), uintptr(viewer.bgBmp))
		pSetStretchBltMode.Call(uintptr(hdc), HALFTONE)
		dw, dh := viewerDisplaySize()
		pStretchBlt.Call(
			uintptr(hdc),
			uintptr(int32(math.Round(viewer.panX))), uintptr(int32(math.Round(viewer.panY))),
			uintptr(int32(math.Round(dw))), uintptr(int32(math.Round(dh))),
			uintptr(memDC), 0, 0, uintptr(viewer.imgW), uintptr(viewer.imgH), SRCCOPY,
		)
		pSelectObject.Call(uintptr(memDC), oldObj)
		pDeleteDC.Call(uintptr(memDC))
	}
	// 底部状态条（复用灯箱 lightbox-bar 视觉族：RGB(24,27,34)）
	statusTop := cr.h() - int32(math.Round(viewerStatusH*float64(dpiScale)))
	statusRect := rectI{left: 0, top: statusTop, right: cr.w(), bottom: cr.h()}
	barBrush, _, _ := pCreateSolidBrush.Call(0x00221B18)
	pFillRect.Call(uintptr(hdc), uintptr(unsafe.Pointer(&statusRect)), barBrush)
	pDeleteObject.Call(barBrush)
	pSetBkMode.Call(uintptr(hdc), TRANSPARENT_BKMODE)

	// ---- 第四批：三段布局（左=名称·尺寸；右=zoom%可点；中=提示行，窄窗省略） ----
	leftText, _ := windows.UTF16FromString(fmt.Sprintf("%s · %dx%d px", viewer.name, viewer.imgW, viewer.imgH))
	zoomText, _ := windows.UTF16FromString(fmt.Sprintf("%d%%", int(math.Round(viewer.zoom*100))))
	hintText, _ := windows.UTF16FromString(viewerHintText)

	// 测量：提示按实际宽回退（GetTextExtentPoint32W 需要 4 参：hdc/text/count/LPSIZE）
	f13 := viewerEnsureFont(13)
	f11 := viewerEnsureFont(11)
	type sizePoint struct{ x, y int32 }
	hintWp := 0.0
	oldF, _, _ := pSelectObject.Call(uintptr(hdc), uintptr(f11))
	var hintSz sizePoint
	if okExt, _, _ := pGetTextExtentPoint32W.Call(uintptr(hdc), uintptr(unsafe.Pointer(&hintText[0])), uintptr(len(hintText)-1), uintptr(unsafe.Pointer(&hintSz))); okExt != 0 {
		hintWp = float64(hintSz.x)
	}
	pSelectObject.Call(uintptr(hdc), oldF)

	zoomRect, hintRect, hintVisible := layoutStatus(cr.w(), cr.h(), 56, hintWp)
	viewer.statusZoom = zoomRect
	viewer.statusHint = hintRect
	viewer.hintVisible = hintVisible

	// 左：名称·尺寸（--text 系浅灰白；末位省略号兜底）
	viewerLeft := rectI{left: 8, top: statusRect.top, right: zoomRect.left - 12, bottom: cr.h()}
	pSelectObject.Call(uintptr(hdc), uintptr(f13))
	pSetTextColor.Call(uintptr(hdc), 0x00E8E6E6)
	pDrawTextW.Call(uintptr(hdc), uintptr(unsafe.Pointer(&leftText[0])), uintptr(len(leftText)-1),
		uintptr(unsafe.Pointer(&viewerLeft)), DT_SINGLELINE|DT_VCENTER|DT_END_ELLIPSIS)

	// 中：提示行（暗灰层级，等效 --text-dim2；窄窗省略）
	if hintVisible {
		pSelectObject.Call(uintptr(hdc), uintptr(f11))
		pSetTextColor.Call(uintptr(hdc), 0x00A29B8E) // RGB(142,155,162) 暗层级
		pDrawTextW.Call(uintptr(hdc), uintptr(unsafe.Pointer(&hintText[0])), uintptr(len(hintText)-1),
			uintptr(unsafe.Pointer(&hintRect)), DT_RIGHT|DT_SINGLELINE|DT_VCENTER)
	}

	// 右：zoom%（可点；常态暗灰蓝，hover 增亮 accent 蓝）
	zoomColor := uintptr(0x00A29B8E)
	if viewer.hoverZoom {
		zoomColor = 0x00FEA86E
	}
	pSelectObject.Call(uintptr(hdc), uintptr(f13))
	pSetTextColor.Call(uintptr(hdc), zoomColor)
	rz := viewer.statusZoom
	pDrawTextW.Call(uintptr(hdc), uintptr(unsafe.Pointer(&zoomText[0])), uintptr(len(zoomText)-1),
		uintptr(unsafe.Pointer(&rz)), DT_RIGHT|DT_SINGLELINE|DT_VCENTER)
}

/** 主窗关闭时一并销毁看图窗并释放位图（requestClose 里调用） */
func viewerDestroyForClosing() {
	if viewer.hwnd != 0 {
		if isWin, _, _ := pIsWindow.Call(uintptr(viewer.hwnd)); isWin != 0 {
			pDestroyWindow.Call(uintptr(viewer.hwnd))
		}
	}
	if viewer.bgBmp != 0 {
		pDeleteObject.Call(uintptr(viewer.bgBmp))
		viewer.bgBmp = 0
	}
}

/** 进程私有内存（PrivateUsage / 私有提交字节）MB —— 自检内存对比用 */
func processPrivateMB() float64 {
	var pmc processMemoryCounters
	pmc.cb = uint32(unsafe.Sizeof(pmc))
	hProc := windows.CurrentProcess()
	r, _, _ := pGetProcessMemoryInfo.Call(uintptr(hProc), uintptr(unsafe.Pointer(&pmc)), uintptr(pmc.cb))
	if r == 0 {
		return -1
	}
	return float64(pmc.privUsage) / 1e6
}
