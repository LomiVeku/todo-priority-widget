//go:build windows

// main.go —— 入口：把窗口 / WebView2 / 桥 / 吸附 / 存储 / 托盘组装起来。
//
// 运行模式：
//   正常      —— 常驻悬浮窗
//   --selftest —— 运行时自检（灌演示数据 → 逐项检查 → 截图 → selftest-report.json → 自动退出）
//   --gpu     —— 逃生门：跳过内置的 --disable-gpu --in-process-gpu 内存优化，保留硬件加速
package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"time"
	"unsafe"

	"github.com/jchv/go-webview2/pkg/edge"
	"golang.org/x/sys/windows"
)

// 版本规则见 版本登记-内部.md：修 bug / UI L3 微调 +0.0.1；新功能 / UI L1+L2 大调 +0.1.0；升 0.1.0 清零 0.0.1。
// T6 桌宠提醒对接为新功能 → 内部 1.3.0；对外打包构建期覆盖：-ldflags "-X main.appVersion=1.0beta"
var appVersion = "1.3.0"

var (
	appAssetsDir  string
	selftestMode  bool
	selftestOut   string
	selftestImage string
	gpuEscape     bool // --gpu：跳过内置浏览器参数，保留硬件加速（逃生门）
)

var logFile *os.File

func appLogf(format string, a ...interface{}) {
	line := fmt.Sprintf("[app] "+format+"\n", a...)
	fmt.Print(line)
	if logFile != nil {
		logFile.WriteString(line)
	}
}

func randToken() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func execCommand(name string, args ...string) *exec.Cmd {
	return exec.Command(name, args...)
}

/* ---------------------------------------------------------------- 目录 */

func resolveAssetsDir() string {
	exe, err := os.Executable()
	if err == nil {
		cand := filepath.Join(filepath.Dir(exe), "assets")
		if st, err2 := os.Stat(cand); err2 == nil && st.IsDir() {
			return cand
		}
	}
	// go run / 开发目录兜底：必须转绝对路径——相对路径会被拼成
	// file:///assets/... 指向盘根，页面 404 后桥注入必然失效。
	if abs, err := filepath.Abs("assets"); err == nil {
		if st, err2 := os.Stat(abs); err2 == nil && st.IsDir() {
			return abs
		}
		return abs
	}
	return "assets"
}

func defaultDataDir() string {
	appdata := os.Getenv("APPDATA")
	return filepath.Join(appdata, "待办事项优先级提示窗-Go")
}

/* ---------------------------------------------------------------- 启动 */

func main() {
	// 自检参数
	for i, a := range os.Args {
		if a == "--selftest" {
			selftestMode = true
		}
		if a == "--selftest-out" && i+1 < len(os.Args) {
			selftestOut = os.Args[i+1]
		}
		if a == "--selftest-image" && i+1 < len(os.Args) {
			selftestImage = os.Args[i+1]
		}
		if a == "--gpu" {
			gpuEscape = true
		}
	}

	// DPI 感知（Per-Monitor V2，老系统回退系统级）
	enablePerMonitorDPI()

	// 日志：stderr 之外同时落一份文件，便于诊断无控制台场景
	{
		lf := filepath.Join(os.TempDir(), "todo-go.log")
		if f, err := os.OpenFile(lf, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
			logFile = f
			f.WriteString("\n==== launch " + time.Now().Format(time.RFC3339) + " ====\n")
		}
	}

	// 内存优化：软件光栅化 + 合并 GPU 进程（DeepSeek 2026-10-12 实测私有内存 288→221MB，−23%）
	// 必须在 WebView2 环境创建（chromium.Embed）之前生效；本应用无 GPU 密集场景，
	// 截图像素级比对为亚感知差异。逃生门：命令行加 --gpu 保留硬件加速；用户已自行
	// 设置 WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS 时尊重用户值，便于排查问题。
	if envArgs := os.Getenv("WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS"); envArgs != "" {
		appLogf("内存优化未启用：检测到用户已设置 WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS=%q", envArgs)
	} else if gpuEscape {
		appLogf("内存优化未启用：检测到 --gpu 逃生门（保留硬件加速）")
	} else {
		_ = os.Setenv("WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS", "--disable-gpu --in-process-gpu")
		appLogf("内存优化已启用：--disable-gpu --in-process-gpu（--gpu 可临时关闭）")
	}

	// WebView2 的 COM 对象与消息循环必须待在同一个 OS 线程上
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	appLogf("trace: lock thread")
	_ = windows.CoInitializeEx(0, windows.COINIT_APARTMENTTHREADED)
	appLogf("trace: com init")

	appAssetsDir = resolveAssetsDir()
	appLogf("trace: assets=%s", appAssetsDir)

	// 单实例（自检模式豁免，避免与正在运行的正式版互斥）
	if !selftestMode {
		if !acquireSingleInstance() {
			appLogf("已有实例在运行，聚焦之")
			focusExistingInstance()
			return
		}
	}

	// 数据目录（自检用临时目录，避免污染真实数据）
	// 注意：本机 Go 的 os.RemoveAll 对含 WebView2 配置的目录树会挂死（bash rm -rf 却秒删），
	// 故自检目录改为每次运行唯一命名，不做整体删除。
	appLogf("trace: data dir begin")
	userData := defaultDataDir()
	if selftestMode {
		userData = filepath.Join(os.TempDir(), "todo-go-selftest-"+strconv.FormatInt(time.Now().UnixMilli(), 10))
		shotDir = selftestOut
		if shotDir == "" {
			shotDir = filepath.Join(os.TempDir(), "todo-go-selftest-shots")
		}
		_ = os.MkdirAll(shotDir, 0o755)
	}
	appLogf("trace: data dir chosen = %s", userData)
	theStore = NewStore(userData, portableRoot())
	appLogf("数据目录 %s（portable=%v）", theStore.baseDir, theStore.portable)
	appLogf("trace: store ready")

	// 窗口几何（window.json 以 DIP 存储，与 Electron 版一致）
	saved := theStore.LoadWindowState()
	var wDip, hDip, xDip, yDip float64 = dipW, dipH, -1, -1
	wOK, hOK := false, false
	if v, ok := saved["width"].(float64); ok && v >= dipMinW {
		wDip = v
		wOK = true
	}
	if v, ok := saved["height"].(float64); ok && v >= dipMinH {
		hDip = v
		hOK = true
	}
	// x/y 仅在宽高均合法时才采用：历史版本的关闭存档曾把"已销毁窗口"的
	// 全零几何写入 window.json（width/height/x/y 全为 0 的伪存档）。
	// 若沿用 x=0,y=0，窗口会落在屏幕边角并在启动期触发贴边吸附——
	// 配合 Embed 未完成的窗口期构成 2026-10-13 启动崩溃的诱因。
	// 全零伪存档一律回落默认位置（xDip/yDip 保持 -1）。
	if wOK && hOK {
		if v, ok := saved["x"].(float64); ok {
			xDip = v
		}
		if v, ok := saved["y"].(float64); ok {
			yDip = v
		}
	}
	if v, ok := saved["alwaysOnTop"].(bool); ok {
		alwaysOnTop = v
	}
	if v, ok := saved["dockEnabled"].(bool); ok {
		dockEnabled = v
	}

	// 先按估算尺寸建窗口（隐藏），拿到真实 DPI 后再摆到目标位置
	pri := primaryDisplay()
	if err := createMainWindow(pri.WorkArea.right-100, pri.WorkArea.top+50,
		dipToPhys(wDip), dipToPhys(hDip), false); err != nil {
		appLogf("FAIL 建窗口失败: %v", err)
		return
	}
	if xDip < 0 || yDip < 0 {
		xDip = float64(pri.WorkArea.right-pri.WorkArea.left)/dpiScale - wDip - 40
		yDip = 60
	}
	setWindowBounds(rectI{
		left: int32(xDip * dpiScale), top: int32(yDip * dpiScale),
		right:  int32(xDip*dpiScale) + int32(wDip*dpiScale),
		bottom: int32(yDip*dpiScale) + int32(hDip*dpiScale),
	})

	addTray()
	pipeStart() // T6：桌宠提醒对接（selftestMode 内部自会跳过；§5.5）

	// ---- WebView2 ----
	chromium = edge.NewChromium()
	chromium.DataPath = filepath.Join(theStore.baseDir, "webview2")
	chromium.MessageCallback = handleWebMessage
	chromium.NavigationCompletedCallback = func(_ *edge.ICoreWebView2, _ *edge.ICoreWebView2NavigationCompletedEventArgs) {
		onPageReady()
	}

	if !chromium.Embed(uintptr(hwnd)) {
		appLogf("FAIL WebView2 Embed 失败")
		return
	}
	appLogf("OK WebView2 已嵌入")

	// ★ Embed 之后必须 Resize（把控件边界设成客户区），否则整窗全透明
	resizeChromium()

	// 文档创建即注入：模块垫片 + meta.js/parser.js 原文 + window.api 桥
	chromium.Init(buildBootScript())

	attachWebView()
	webviewReady = true // WebView2 控制器已创建、桥已注入，允许向渲染层推送

	// 消息循环（必须在锁定线程上）
	var m msgStruct
	for {
		r, _, _ := pGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(r) <= 0 {
			break
		}
		if m.hwnd == 0 && m.message == WM_TIMER {
			fireTimer(m.wParam)
			continue
		}
		pTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		pDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}

	// 收尾：仅异常路径兜底落盘。正常关闭时 requestClose() 已在销毁前落盘；
	// 若此处对已销毁的 hwnd 再落一次，GetWindowRect 失败会得到全零矩形，
	// 把正确存档覆盖成伪存档（2026-10-13 查实）。
	if !closing {
		persistWindowState()
	}
	appLogf("消息循环结束")
}

func portableRoot() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	return filepath.Dir(exe)
}

/* ---------------------------------------------------------------- 页面就绪 */

var pageReadyOnce bool

func onPageReady() {
	if pageReadyOnce {
		return
	}
	pageReadyOnce = true
	appLogf("trace: page navigation completed")

	after(300, func() {
		appLogf("trace: boot check begin")
		// 桥可用性（CSP 等意外会让注入失效，这里必须显式验证）
		s, ok := evalExpr("!!window.api && !!window.api.meta && !!window.api.helpers && !!window.__wbModules['parser']", 8000)
		appLogf("trace: boot check result ok=%v s=%q", ok, s)
		if !ok || s != "true" {
			appLogf("FAIL 桥注入未生效（window.api 不可用）——页面 CSP 可能拦截了文档创建脚本")
			writeBootFailure()
			return
		}
		appLogf("OK 桥已就绪（window.api 可用，meta/parser 模块已加载）")

		showMainWindow()
		broadcastWinState()
		broadcastDock()
		pushDisplays()

		if selftestMode {
			after(900, runSelftest)
		}
	})
}

func writeBootFailure() {
	if selftestOut != "" {
		_ = os.MkdirAll(selftestOut, 0o755)
		rep, _ := json.MarshalIndent(map[string]interface{}{
			"startedAt": time.Now().Format(time.RFC3339),
			"fatal":     "bridge-not-ready",
		}, "", "  ")
		_ = os.WriteFile(filepath.Join(selftestOut, "selftest-report.json"), rep, 0o644)
	}
	go func() {
		time.Sleep(500 * time.Millisecond)
		// 1.2.0 批：桥未就绪时页面 JS 不存在，若走 WM_CLOSE→确认通路，推送会落进虚空、窗口关不掉——
		// 故此处直调 requestClose()（不走确认），保证退出。
		requestClose()
	}()
}
