//go:build windows

// webview.go —— WebView2 宿主：嵌入 / 透明背景 / 桥接通道 / 取回数据的泵等待。
package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"path/filepath"
	"sync"
	"time"
	"unsafe"

	"github.com/jchv/go-webview2/pkg/edge"
	"golang.org/x/sys/windows/registry"
)

var chromium *edge.Chromium

func fileURLOf(path string) string {
	u := url.URL{Scheme: "file", Path: "/" + filepath.ToSlash(path)}
	return u.String()
}

func assetPath(rel string) string {
	return filepath.Join(appAssetsDir, rel)
}

// attachWebView：控制器就绪后设置透明背景并加载页面（控制器异步创建，需要重试）
func attachWebView() {
	ctrl := chromium.GetController()
	if ctrl == nil {
		go func() {
			time.Sleep(250 * time.Millisecond)
			pPostMessageW.Call(uintptr(hwnd), msgAttachWebView, 0, 0)
		}()
		return
	}

	// ★ 关键：WebView2 默认背景设为全透明（ARGB=0），配合 DWM glass 才有每像素透明
	if ctrl2 := ctrl.GetICoreWebView2Controller2(); ctrl2 != nil {
		if err := ctrl2.PutDefaultBackgroundColor(edge.COREWEBVIEW2_COLOR{A: 0, R: 0, G: 0, B: 0}); err != nil {
			appLogf("FAIL PutDefaultBackgroundColor: %v", err)
		} else {
			appLogf("OK WebView2 背景已设为透明 (A=0)")
		}
	} else {
		appLogf("FAIL 取 ICoreWebView2Controller2 失败，透明背景不可用")
	}

	resizeChromium()
	chromium.Navigate(fileURLOf(assetPath("index.html")))
	appLogf("OK 已加载本地页面 %s", fileURLOf(assetPath("index.html")))
}

func resizeChromium() {
	if chromium != nil {
		chromium.Resize()
	}
}

func focusWebView() {
	if chromium != nil {
		chromium.Focus()
	}
}

/* ---------------------------------------------------------------- Go → JS 取回数据 */

var (
	evalMu     sync.Mutex
	evalSeq    int
	evalChans  = map[int]chan string{}
)

// evalJSON：在页面里执行 `JSON.stringify(<expr>)` 并取回结果。
// 原理：注入的脚本回执 eval-reply，这里用 PeekMessage 泵等待（不阻塞消息循环）。
// 只能在主线程（消息循环线程）调用。
func evalJSON(expr string, timeoutMs int) (string, bool) {
	evalMu.Lock()
	evalSeq++
	id := evalSeq
	ch := make(chan string, 1)
	evalChans[id] = ch
	evalMu.Unlock()

	// 数据用 base64 → TextDecoder 解码，规避任何引号/转义/行分隔符问题
	script := fmt.Sprintf(
		`window.__wbEvalReply(%d, (function(){try{return JSON.stringify(%s)}catch(e){return JSON.stringify({__err:String(e)})}})())`,
		id, expr)
	chromium.Eval(script)

	deadline := time.Now().Add(time.Duration(timeoutMs) * time.Millisecond)
	for {
		select {
		case s := <-ch:
			return s, true
		default:
		}
		if time.Now().After(deadline) {
			evalMu.Lock()
			delete(evalChans, id)
			evalMu.Unlock()
			return "", false
		}
		pumpOnce(8)
	}
}

// evalExpr：evalJSON 的别名语义（执行表达式并取回 JSON.stringify 结果）
func evalExpr(expr string, timeoutMs int) (string, bool) {
	return evalJSON(expr, timeoutMs)
}

// pumpOnce：泵一次消息循环（最多等 d 毫秒）。自检期间动画/计时器也要继续走。
func pumpOnce(dMs int) {
	var m msgStruct
	deadline := time.Now().Add(time.Duration(dMs) * time.Millisecond)
	for {
		r, _, _ := pPeekMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0, 1 /*PM_REMOVE*/)
		if r == 0 {
			if time.Now().After(deadline) {
				time.Sleep(3 * time.Millisecond)
				return
			}
			time.Sleep(2 * time.Millisecond)
			continue
		}
		if m.hwnd == 0 && m.message == WM_TIMER {
			fireTimer(m.wParam)
		} else {
			pTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
			pDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
		}
		return
	}
}

// webviewReady：chromium.Embed() 完成前，WebView 控制器尚未创建。
// 该窗口期内若向渲染层推送（例如启动时吸附定时器早于 Embed 完成触发 →
// broadcastDock → Eval），会因空指针直接 panic（2026-10-13 实测崩溃）。
// 就绪前的推送一律丢弃：页面就绪时 onPageReady 会全量补推。
var webviewReady bool

// pushToRenderer：把事件推给渲染层（__wbPush 里做 base64 解码）
func pushToRenderer(t string, data interface{}) {
	if !webviewReady || chromium == nil {
		return
	}
	payload, err := json.Marshal(data)
	if err != nil {
		return
	}
	b64 := base64.StdEncoding.EncodeToString(payload)
	chromium.Eval(fmt.Sprintf(`window.__wbPush(%q, %q);`, t, b64))
}

// beginCloseFlow：1.2.0 批关闭二次确认统一入口。
// 右上角 X（win:close）与 WM_CLOSE / Alt+F4 都推 win:close-request，由页面弹确认；
// 托盘「退出」与 msgQuitAll 不走此路（保持直关）。页面未起时退化为直关，防"关不掉"。
func beginCloseFlow() {
	if webviewReady {
		pushToRenderer("win:close-request", nil)
	} else {
		requestClose()
	}
}

/* ---------------------------------------------------------------- 消息接收 */

func handleWebMessage(s string) {
	var m struct {
		T       string          `json:"t"`
		ID      int             `json:"id"`
		Type    string          `json:"type"`
		Payload json.RawMessage `json:"payload"`
		Data    string          `json:"data"`
	}
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		appLogf("bridge: 无法解析消息 %q", s)
		return
	}
	switch m.T {
	case "eval-reply":
		evalMu.Lock()
		ch, ok := evalChans[m.ID]
		if ok {
			delete(evalChans, m.ID)
		}
		evalMu.Unlock()
		if ok {
			ch <- m.Data
		}
	case "req":
		handleRequest(m.ID, m.Type, m.Payload)
	case "event":
		handleEvent(m.Type, m.Payload)
	}
}

func reply(id int, data interface{}) {
	payload, err := json.Marshal(data)
	if err != nil {
		payload = []byte(`{"__err":"marshal"}`)
	}
	b64 := base64.StdEncoding.EncodeToString(payload)
	chromium.Eval(fmt.Sprintf(`window.__wbResolve(%d, true, JSON.parse(new TextDecoder("utf-8").decode(Uint8Array.from(atob(%q), function(c){return c.charCodeAt(0)}))))`, id, b64))
}

func replyErr(id int, msg string) {
	payload, _ := json.Marshal(map[string]string{"error": msg})
	b64 := base64.StdEncoding.EncodeToString(payload)
	chromium.Eval(fmt.Sprintf(`window.__wbResolve(%d, false, JSON.parse(new TextDecoder("utf-8").decode(Uint8Array.from(atob(%q), function(c){return c.charCodeAt(0)}))))`, id, b64))
}

func handleRequest(id int, typ string, payload json.RawMessage) {
	switch typ {
	case "data:load":
		data, _ := theStore.Load()
		pipeUpdateFromData(data) // T6：载入后同步管道快照（B1/B2）
		refs := extractImageRefs(data["todos"])
		info := theStore.Info()
		info["webview2"] = webView2Version()
		info["electron"] = nil
		info["node"] = nil
		reply(id, map[string]interface{}{
			"todos":     data["todos"],
			"settings":  data["settings"],
			"mediaBase": fileURLOf(theStore.mediaDir),
			"loadOk":    theStore.lastLoadOk,
			"loadError": data["loadError"],
			"info":      info,
			// Go 侧不再自行 GC，由渲染层在 loadOk 时发起（与 Electron 行为一致）
			"_refs": refs,
		})

	case "data:save", "data:save-sync":
		var data map[string]interface{}
		if err := json.Unmarshal(payload, &data); err != nil {
			replyErr(id, err.Error())
			return
		}
		ok := theStore.Save(data)
		if ok {
			pipeUpdateFromData(data) // T6：保存成功后同步管道快照（B2）
		}
		reply(id, map[string]bool{"ok": ok})

	case "image:add-paths":
		var paths []string
		json.Unmarshal(payload, &paths)
		out := []map[string]interface{}{}
		for _, p := range paths {
			name, err := theStore.ImportImage(p)
			if err != nil {
				out = append(out, map[string]interface{}{"ok": false, "path": p, "error": err.Error()})
			} else {
				out = append(out, map[string]interface{}{"ok": true, "name": name, "url": fileURLOf(theStore.mediaPath(name))})
			}
		}
		reply(id, out)

	case "image:add-dataurls":
		var list []string
		json.Unmarshal(payload, &list)
		out := []map[string]interface{}{}
		for _, d := range list {
			name, err := theStore.ImportDataUrl(d)
			if err != nil {
				out = append(out, map[string]interface{}{"ok": false, "error": err.Error()})
			} else {
				out = append(out, map[string]interface{}{"ok": true, "name": name, "url": fileURLOf(theStore.mediaPath(name))})
			}
		}
		reply(id, out)

	case "image:remove":
		var name string
		json.Unmarshal(payload, &name)
		reply(id, map[string]bool{"ok": theStore.RemoveImage(name)})

	case "image:gc":
		var refs []string
		json.Unmarshal(payload, &refs)
		reply(id, theStore.GcImages(refs, 7*24*time.Hour))

	case "app:info":
		reply(id, map[string]interface{}{
			"name":     windowTitle,
			"version":  appVersion,
			"portable": theStore.portable,
			"dataDir":  theStore.baseDir,
			"webview2": webView2Version(),
			"electron": nil,
			"node":     nil,
		})

	case "image:view":
		// 独立看图窗（Win32+GDI，无 WebView2）：把名字解析进看图窗并 reply 打开结果
		var name string
		_ = json.Unmarshal(payload, &name)
		reply(id, openImageViewer(name))

	case "win:toggle-top":
		alwaysOnTop = !alwaysOnTop
		setTopmost(alwaysOnTop)
		broadcastWinState()
		reply(id, alwaysOnTop)

	case "win:set-dock":
		var v bool
		json.Unmarshal(payload, &v)
		dockEnabled = v
		if !dockEnabled {
			killTimer(timerRetract)
			expandNow()
			undock()
		}
		broadcastWinState()
		reply(id, dockEnabled)

	case "win:set-width":
		var wDip float64
		json.Unmarshal(payload, &wDip)
		b := windowRect()
		w := clampI(dipToPhys(wDip), dipToPhys(dipMinW), dipToPhys(1200))
		nb := rectI{left: b.left, top: b.top, right: b.left + w, bottom: b.bottom}
		if dock != nil && dock.side == "right" {
			shift := b.w() - w
			nb.left = b.right - w
			nb.right = nb.left + w
			_ = shift
		}
		setBoundsProgrammatic(nb)
		if dock != nil {
			if d := findDisplay(dock.displayID); d != nil {
				dock.expanded = expandedBoundsFor(dock.side, d.WorkArea, windowRect())
			}
		}
		reply(id, true)

	case "win:get-state":
		var dockOut interface{}
		if dock != nil {
			dockOut = map[string]interface{}{"side": dock.side, "displayId": dock.displayID}
		}
		reply(id, map[string]interface{}{
			"alwaysOnTop": alwaysOnTop,
			"dockEnabled": dockEnabled,
			"dock":        dockOut,
			"retracted":   retracted,
			"displays":    displaysJSON(),
		})

	default:
		replyErr(id, "unknown request: "+typ)
	}
}

func handleEvent(typ string, payload json.RawMessage) {
	switch typ {
	case "win:minimize":
		pShowWindow.Call(uintptr(hwnd), SW_MINIMIZE)
	case "win:close":
		// 1.2.0 批：X 按钮（btn-close → win.close()）也走确认通路；托盘「退出」不经页面直关
		beginCloseFlow()
	case "win:close-confirmed":
		// 1.2.0 批：页面确认弹窗「退出」→ 走既有 requestClose()（300ms 宽限落盘链不动）
		requestClose()
	case "win:drag-start":
		pPostMessageW.Call(uintptr(hwnd), msgDragStart, 0, 0)
	case "win:hover":
		// 自检期间屏蔽：页面 JS 上报的真实鼠标进出会覆盖 hoverInside 并触发
		// expandNow / 400ms retractNow，把自检断言中的窗口位置搅乱
		// （2026-10-13 实测：右吸附态被鼠标进出触发的 2085→2547 缩回动画，
		// 两个角落用例读到动画帧值）。自检用 Go 侧 hoverInside 直控，不依赖本事件。
		if selftestMode {
			break
		}
		var inside bool
		json.Unmarshal(payload, &inside)
		if inside {
			hoverInside = true
			killTimer(timerHoverOut)
			expandNow()
		} else {
			hoverInside = false
			killTimer(timerHoverOut)
			setTimer(timerHoverOut, 400, func() { retractNow() })
		}
	case "win:undock":
		killTimer(timerRetract)
		undock()
	case "win:expand":
		expandNow()
	case "win:retract":
		retractNow()
	case "app:open-data-dir":
		openInExplorer(theStore.baseDir)
	}
}

/* ---------------------------------------------------------------- 广播 */

func broadcastWinState() {
	if chromium == nil {
		return
	}
	pushToRenderer("win:state", map[string]interface{}{
		"alwaysOnTop": alwaysOnTop,
		"dockEnabled": dockEnabled,
		"displays":    len(listDisplays()),
	})
}

func broadcastDock() {
	if chromium == nil {
		return
	}
	var side interface{}
	var handleSide interface{}
	if dock != nil {
		side = dock.side
		handleSide = handleSideOf(dock.side)
	}
	pushToRenderer("win:dock", map[string]interface{}{
		"side":       side,
		"retracted":  retracted,
		"handleSide": handleSide,
	})
}

func pushDisplays() {
	if chromium == nil {
		return
	}
	pushToRenderer("win:displays", displaysJSON())
}

func displaysJSON() []map[string]interface{} {
	out := []map[string]interface{}{}
	for _, d := range listDisplays() {
		out = append(out, map[string]interface{}{
			"id":     d.ID,
			"bounds": d.Bounds,
			"workArea": d.WorkArea,
		})
	}
	return out
}

/* ---------------------------------------------------------------- 小工具 */

func extractImageRefs(todos interface{}) []string {
	var refs []string
	if list, ok := todos.([]interface{}); ok {
		for _, item := range list {
			if m, ok := item.(map[string]interface{}); ok {
				if imgs, ok := m["images"].([]interface{}); ok {
					for _, n := range imgs {
						if s, ok := n.(string); ok {
							refs = append(refs, s)
						}
					}
				}
			}
		}
	}
	return refs
}

func openInExplorer(dir string) {
	cmd := execCommand("explorer", dir)
	_ = cmd.Start()
}

func webView2Version() string {
	// 该依赖包未封装 ICoreWebView2Environment::GetBrowserVersionString，
	// 从注册表读 WebView2 Runtime 版本（拿不到就空着，仅展示用）。
	keys := []string{
		`SOFTWARE\WOW6432Node\Microsoft\EdgeUpdate\Clients\{F3017226-FE2A-4295-8BDF-00C3A9A7E4C5}`,
		`SOFTWARE\Microsoft\EdgeUpdate\Clients\{F3017226-FE2A-4295-8BDF-00C3A9A7E4C5}`,
	}
	for _, k := range keys {
		key, err := registry.OpenKey(registry.LOCAL_MACHINE, k, registry.QUERY_VALUE)
		if err != nil {
			continue
		}
		v, _, err := key.GetStringValue("pv")
		key.Close()
		if err == nil && v != "" {
			return v
		}
	}
	return ""
}
