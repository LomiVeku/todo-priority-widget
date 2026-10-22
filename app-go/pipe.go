//go:build windows

// pipe.go —— 桌宠提醒对接（T6，内部 1.3.0）：命名管道服务
//
// 协议（冻结，逐字实现；歧义问主控不自行发挥）：
//   管道名 \\.\pipe\whalechan-todo；UTF-8 单行 JSON、\n 分帧；单行上限 64KB；
//   上行（桌宠→待办）：hello（仅记日志）/ open（PostMessage 回 UI 线程置前）/ 其它 type 忽略；
//   非法 JSON 忽略该行、不断线；超长行丢弃、不断线。
//   下行（待办→桌宠）：state（全量）/ due（到期事件；同一 id 同一 deadline 只推一次）。
//
// 工程约束（§五，均有原型实测依据）：
//   读：PeekNamedPipe 100ms 轮询，TotalBytesAvail>0 才 ReadFile（同步句柄禁阻塞读，
//       否则把并发 WriteFile 一起卡死——原型 v1 实测）；
//   写：有界队列（cap 16）+ serve 循环独占 WriteFile，send 用 select default 永不阻塞调用方；
//   实例数 1 + FIRST_PIPE_INSTANCE；显式 DACL（当前用户 GA + SYSTEM GA）；
//   创建失败 30s 重试不退出进程；selftestMode 不启动（防抢管道名 + 防推演示数据）。
package main

import (
	"encoding/json"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	pipeName            = `\\.\pipe\whalechan-todo`
	pipeLineLimit       = 64 * 1024 // 单行上限（超长行丢弃、不断线）
	pipeOutboxCap       = 16        // 每连接写队列容量
	pipeMaxItems        = 200       // state.items 上限（超出取前 200，记日志）
	pipeContentMaxRunes = 200       // content 截断（按 rune）
	pipeReadPoll        = 100 * time.Millisecond
	pipeRetryInterval   = 30 * time.Second
	dueTickInterval     = time.Second
	seedMaxBacklog      = 10 * time.Minute // B6 启动种子：超期 ≤10min 补推
	overdueFloorMs      = int64(60000)     // B5：<60s 一律传 0
)

var pipePeek = windows.NewLazySystemDLL("kernel32.dll").NewProc("PeekNamedPipe")

// pipeDue：下行 due 事件
type pipeDue struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	Content   string `json:"content"`
	Priority  string `json:"priority"`
	OverdueBy int64  `json:"overdueBy"`
}

var (
	pipeMu       sync.Mutex
	pipeStarted  bool
	pipeStopped  bool
	pipeDoneCh   = make(chan struct{}) // 包级初始化（修 selftestMode 下 close(nil) panic；T6 故障定性 §二）
	pipeStopOnce sync.Once
	pipeListH    windows.Handle // 监听实例句柄
	pipeConnH    windows.Handle // 当前连接句柄（0 = 无客户端）
	pipeThreadH  windows.Handle // 监听协程线程真句柄（DuplicateHandle 转换；CancelSynchronousIo 用，T8）
	pipeOutbox   chan []byte    // 有界写队列（§5.2）
	pipeSnapshot []byte         // 最近一次序列化的 state（B2 去重）
	pipeTodos    []interface{}  // 最近一次的 todos（due tick 数据源）
	pipeNotified map[string]int64
	pipeSeeded   bool // B6 种子是否已处理
)

// T8（P0 退出卡死）：CancelSynchronousIo——取消线程上的同步 I/O（阻塞式 ConnectNamedPipe）
var pipeCancelSIO = windows.NewLazySystemDLL("kernel32.dll").NewProc("CancelSynchronousIo")
var pipeGetCurThread = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetCurrentThread")

// ---- 纯函数（selftest 与单测直接调用，不依赖 GUI） ----

// pipeNormalizePriority：协议 §3.2——未知值归一为 normal
func pipeNormalizePriority(p string) string {
	switch p {
	case "urgent", "important", "normal", "minor", "shelved":
		return p
	}
	return "normal"
}

// pipeTruncate：按 rune 截断（不切坏多字节）
func pipeTruncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// pipeBuildStateItems：todos → state.items。
// 规则：id 缺失/为空丢弃；priority 未知归一 normal；content 截 200 rune；
// deadline 数字毫秒或 null；上限 200 条（超出取前 200）。
func pipeBuildStateItems(todos []interface{}) []map[string]interface{} {
	items := []map[string]interface{}{}
	for _, t := range todos {
		m, ok := t.(map[string]interface{})
		if !ok {
			continue
		}
		id, _ := m["id"].(string)
		if id == "" {
			continue
		}
		content, _ := m["content"].(string)
		pri, _ := m["priority"].(string)
		completed, _ := m["completed"].(bool)
		item := map[string]interface{}{
			"id":        id,
			"content":   pipeTruncate(content, pipeContentMaxRunes),
			"priority":  pipeNormalizePriority(pri),
			"deadline":  nil,
			"completed": completed,
		}
		if dl, ok := m["deadline"].(float64); ok {
			item["deadline"] = dl
		}
		items = append(items, item)
		if len(items) >= pipeMaxItems {
			appLogf("pipe: todos 超过 %d 条，仅推送前 %d 条", pipeMaxItems, pipeMaxItems)
			break
		}
	}
	return items
}

// pipeBuildState：整条 state 行（含 \n）
func pipeBuildState(todos []interface{}) []byte {
	b, _ := json.Marshal(map[string]interface{}{"type": "state", "items": pipeBuildStateItems(todos)})
	return append(b, '\n')
}

type pipeTodoFacts struct {
	id        string
	content   string
	priority  string
	deadline  int64
	hasDl     bool
	completed bool
}

func pipeFactsOf(t interface{}) (pipeTodoFacts, bool) {
	m, ok := t.(map[string]interface{})
	if !ok {
		return pipeTodoFacts{}, false
	}
	f := pipeTodoFacts{}
	f.id, _ = m["id"].(string)
	if f.id == "" {
		return f, false
	}
	f.content, _ = m["content"].(string)
	pri, _ := m["priority"].(string)
	f.priority = pipeNormalizePriority(pri)
	f.completed, _ = m["completed"].(bool)
	if dl, ok := m["deadline"].(float64); ok {
		f.deadline, f.hasDl = int64(dl), true
	}
	return f, true
}

// pipeDueCompute：B3-B8 到期状态机（纯函数）。
// 返回本 tick 应推的 due 事件；notified 原地维护（B4 标记 / B6 超积压标记 / B8 清理）。
// seeded=false 表示首次快照（B6 种子：超期 ≤10min 补推，>10min 仅标记）。
func pipeDueCompute(todos []interface{}, now time.Time, notified map[string]int64, seeded bool) []pipeDue {
	nowMs := now.UnixMilli()
	events := []pipeDue{}
	seen := map[string]bool{}
	for _, t := range todos {
		f, ok := pipeFactsOf(t)
		if !ok {
			continue
		}
		seen[f.id] = true
		if f.completed || !f.hasDl || f.priority == "shelved" {
			// B7：shelved 不进 due；B8：deadline 变 null/缺失时清理 notified
			delete(notified, f.id)
			continue
		}
		if nowMs < f.deadline {
			continue // 未到期
		}
		if notified[f.id] == f.deadline {
			continue // B4：同一 id 同一 deadline 只推一次
		}
		if !seeded && nowMs-f.deadline > seedMaxBacklog.Milliseconds() {
			notified[f.id] = f.deadline // B6：启动积压超 10 分钟，直接标记不补推
			continue
		}
		overdue := nowMs - f.deadline
		if overdue < overdueFloorMs {
			overdue = 0 // B5
		}
		events = append(events, pipeDue{
			Type: "due", ID: f.id,
			Content:   pipeTruncate(f.content, pipeContentMaxRunes),
			Priority:  f.priority,
			OverdueBy: overdue,
		})
		notified[f.id] = f.deadline
	}
	// B8：已删除的项清理 notified（防 map 无界增长）
	for id := range notified {
		if !seen[id] {
			delete(notified, id)
		}
	}
	return events
}

// ---- 服务端 ----

var pipeWinRes = windows.NewLazySystemDLL("kernel32.dll")

// pipeCurrentUserSA：显式 DACL——D:P(A;;GA;;;<当前用户SID>)(A;;GA;;;SY)（原型已验通）
func pipeCurrentUserSA() (*windows.SecurityAttributes, error) {
	tok := windows.GetCurrentProcessToken()
	tu, err := tok.GetTokenUser()
	if err != nil {
		return nil, err
	}
	sid := tu.User.Sid.String()
	sd, err := windows.SecurityDescriptorFromString("D:P(A;;GA;;;" + sid + ")(A;;GA;;;SY)")
	if err != nil {
		return nil, err
	}
	return &windows.SecurityAttributes{
		Length:             uint32(unsafe.Sizeof(windows.SecurityAttributes{})),
		SecurityDescriptor: sd,
	}, nil
}

// pipeStart：启动管道服务。selftestMode 不启动（§5.5：防抢管道名 + 防推演示数据）。
// pipeDoneCh 已包级初始化（不再重新 make——selftestMode 下 pipeStop 仍会被真退出路径
// 调用，close nil 会 panic；重开服务须先 pipeStop 复位 stopped 标志）。
func pipeStart() {
	if selftestMode || pipeStarted || pipeStopped {
		return
	}
	pipeStartInternal()
}

// pipeStartInternal：实际启动（与挡板分离——V9/V10 自检测试入口经 pipeResetForTest 后调用）。
// tick 协程在 selftest 中无 todos 数据源 → 不推任何数据（对真实环境零干扰）。
func pipeStartInternal() {
	pipeStarted = true
	pipeNotified = map[string]int64{}
	pipeOutbox = make(chan []byte, pipeOutboxCap)
	appLogf("pipe: 服务启动 %s", pipeName)
	go pipeListenLoop()
	go pipeTickLoop()
}

// pipeResetForTest：V9/V10 连跑间的状态复位（自检专用；真实进程无重开场景）。
// sync.Once 不可重置 → 整体替换；pipeDoneCh 换新（已 close 的不能再收）。
func pipeResetForTest() {
	pipeMu.Lock()
	pipeStarted = false
	pipeStopped = false
	pipeThreadH = 0
	pipeListH = 0
	pipeConnH = 0
	pipeMu.Unlock()
	pipeStopOnce = sync.Once{}
	pipeDoneCh = make(chan struct{})
	pipeOutbox = nil
}

// pipeStop：停止服务（requestClose 真退出路径调用）。
// T8 修（P0 退出卡死）：旧版在锁外 CloseHandle(pipeListH)——当监听协程正阻塞在同步
// ConnectNamedPipe(h, nil)（无客户端时永不返回）时，CloseHandle 要等该同步 I/O 结束
// → UI 线程与 pipe 协程互锁、进程永不退出（_verify\quit-hang-20261022 3/3 复现）。
// 现版：①CancelSynchronousIo(pipeThreadH) 先行——把协程线程上的同步 I/O（ConnectNamedPipe，
// 以及 serve 可能卡住的 WriteFile）一并打 ERROR_OPERATION_ABORTED；
// ②**停止方不再 CloseHandle 任何句柄**——listen 协程在 ABORTED 返回路径自关、
// serve 在退出 defer 自关（conn）——每句柄单一关闭方，消除双重 CloseHandle
//（停止方关闭后句柄值可能已被内核复用，双关属未定义行为；旧代码双关路径从未真实
// 走过是因为 stop 时协程先卡死了）。
func pipeStop() {
	pipeStopOnce.Do(func() {
		pipeStopped = true
		close(pipeDoneCh)
		pipeMu.Lock()
		th := pipeThreadH
		pipeListH, pipeConnH, pipeThreadH = 0, 0, 0
		pipeMu.Unlock()
		if th != 0 {
			pipeCancelSIO.Call(uintptr(th))
		}
	})
}

// pipeSend：对外推送入口（有界队列，永不阻塞调用方；队列满 → 断开当前连接，B9）
func pipeSend(line []byte) {
	pipeMu.Lock()
	conn := pipeConnH
	pipeMu.Unlock()
	if conn == 0 {
		return // 无客户端：静默丢弃（重连后 B1 全量补齐）
	}
	select {
	case pipeOutbox <- line:
	default:
		appLogf("pipe: 写队列满，断开当前连接（B9）")
		pipeKickConn()
	}
}

func pipeKickConn() {
	pipeMu.Lock()
	h := pipeConnH
	pipeConnH = 0
	pipeMu.Unlock()
	if h != 0 {
		windows.CloseHandle(h)
	}
}

// pipeUpdateFromData：data:load / data:save 后调用（webview.go 各一处）。
// 更新快照与 due 数据源；序列化与上次相同则不推（B2）。
func pipeUpdateFromData(data map[string]interface{}) {
	pipeMu.Lock()
	todos, _ := data["todos"].([]interface{})
	pipeTodos = todos
	snap := pipeBuildState(todos)
	changed := !bytesEqual(snap, pipeSnapshot)
	pipeSnapshot = snap
	pipeMu.Unlock()
	if changed {
		pipeSend(snap)
	}
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// pipeListenLoop：建管道 → 等连接 → serve → 循环；创建失败 30s 重试不退出进程。
func pipeListenLoop() {
	// T8：取自身线程真句柄（GetCurrentThread 返回伪句柄，需 DuplicateHandle 转真才能被
	// pipeStop 所在线程用于 CancelSynchronousIo）。写入与 pipeListH 同一临界区——
	// happens-before 保证：pipeStop 拿到锁时，要么尚无句柄（无需取消），要么两者齐备。
	myThreadH := pipeDupCurrentThread()
	pipeFirstCreateDone := false // 首次创建带 FIRST_PIPE_INSTANCE（防抢注）；重建时不带
	                              // ——修复"上一实例刚关、内核清理未完时 0ms 重连失败"盲区（T6 终审发现）
	for !pipeStopped {
		sa, err := pipeCurrentUserSA()
		if err != nil {
			appLogf("pipe: DACL 构建失败 %v（30s 后重试）", err)
			pipeSleepRetry()
			continue
		}
		name, _ := windows.UTF16FromString(pipeName)
		openMode := uint32(windows.PIPE_ACCESS_DUPLEX)
		if !pipeFirstCreateDone {
			openMode |= windows.FILE_FLAG_FIRST_PIPE_INSTANCE
		}
		h, cerr := windows.CreateNamedPipe(&name[0],
			openMode,
			windows.PIPE_TYPE_BYTE|windows.PIPE_READMODE_BYTE|windows.PIPE_WAIT|windows.PIPE_REJECT_REMOTE_CLIENTS,
			1, 65536, 65536, 0, sa)
		if cerr != nil {
			appLogf("pipe: CreateNamedPipe 失败 %v（2s 后重试，不退出进程）", cerr)
			pipeSleepRetry()
			continue
		}
		pipeFirstCreateDone = true
		pipeMu.Lock()
		pipeListH = h
		pipeThreadH = myThreadH
		pipeMu.Unlock()
		appLogf("pipe: listening %s", pipeName)

		// 等客户端连接（阻塞；pipeStop 关句柄会唤醒并返回错误）
		err = windows.ConnectNamedPipe(h, nil)
		if pipeStopped {
			windows.CloseHandle(h)
			return
		}
		if err != nil && err != windows.ERROR_PIPE_CONNECTED {
			windows.CloseHandle(h)
			appLogf("pipe: ConnectNamedPipe 失败 %v", err)
			pipeSleepRetry()
			continue
		}
		appLogf("pipe: 桌宠已连接")
		pipeServeConn(h) // 阻塞直到断开 / 停止
		appLogf("pipe: 桌宠连接结束")
	}
}

// pipeDupCurrentThread：把当前线程伪句柄转成可跨线程使用的真句柄；失败返回 0
//（pipeStop 将跳过取消——此时协程尚未触达 ConnectNamedPipe，或环境异常，行为退回 T6 前状态）。
func pipeDupCurrentThread() windows.Handle {
	pseudo, _, _ := pipeGetCurThread.Call()
	var out windows.Handle
	if err := windows.DuplicateHandle(
		windows.CurrentProcess(), windows.Handle(pseudo), windows.CurrentProcess(), &out,
		0, false, windows.DUPLICATE_SAME_ACCESS,
	); err != nil {
		appLogf("pipe: DuplicateHandle 失败 %v（CancelSynchronousIo 将不可用，stop 可能退回互锁）", err)
		return 0
	}
	return out
}

func pipeSleepRetry() {
	// 2s（原 30s）：同进程实例重建会撞"旧实例内核清理未完"的短窗口（maxInstances=1），
	// 30s 会让断线后的重连盲区过长；防抢注语义由 FIRST_PIPE_INSTANCE 标志保留（T6 终审 E4 实证）
	for i := 0; i < 20 && !pipeStopped; i++ {
		time.Sleep(100 * time.Millisecond)
	}
}

// pipeServeConn：单连接服务循环——独占 WriteFile（§5.2）；Peek 轮询读（§5.1）。
// 连接建立立即推全量 state（B1）。
func pipeServeConn(conn windows.Handle) {
	defer func() {
		pipeMu.Lock()
		if pipeConnH == conn {
			pipeConnH = 0
		}
		pipeMu.Unlock()
		windows.CloseHandle(conn)
	}()
	pipeMu.Lock()
	pipeConnH = conn
	snap := pipeSnapshot
	pipeMu.Unlock()
	if snap != nil {
		if err := pipeWriteLine(conn, snap); err != nil {
			return // B1 推送即失败 → 断开（B9）
		}
	}

	rbuf := make([]byte, 65536)
	var acc []byte
	poll := time.NewTicker(pipeReadPoll)
	defer poll.Stop()
	for {
		select {
		case <-pipeDoneCh:
			return
		case b := <-pipeOutbox:
			if err := pipeWriteLine(conn, b); err != nil {
				appLogf("pipe: 写失败 %v → 断开（B9）", err)
				return
			}
		case <-poll.C:
			var avail uint32
			r, _, _ := pipePeek.Call(uintptr(conn), 0, 0, 0, uintptr(unsafe.Pointer(&avail)), 0)
			if r == 0 {
				return // 管道已断（客户端关闭 / 异常）
			}
			if avail == 0 {
				continue
			}
			var n uint32
			if err := windows.ReadFile(conn, rbuf, &n, nil); err != nil {
				return // B9：读错误 → 断开，不重试当前连接
			}
			acc = append(acc, rbuf[:n]...)
			for {
				i := indexBytePipe(acc, '\n')
				if i < 0 {
					if len(acc) > pipeLineLimit {
						acc = acc[:0] // 超长行丢弃（不断线，E9）
					}
					break
				}
				line := acc[:i]
				acc = acc[i+1:]
				if len(line) > pipeLineLimit {
					continue // 超长完整行：丢弃
				}
				pipeHandleLine(string(line))
			}
		}
	}
}

// pipeHandleLine：上行分帧处理（协议 §3.1）
func pipeHandleLine(line string) {
	var m struct {
		Type string `json:"type"`
	}
	if json.Unmarshal([]byte(line), &m) != nil {
		return // 非法 JSON：忽略该行、不断线
	}
	switch m.Type {
	case "hello":
		var extra struct {
			From    string `json:"from"`
			Version int    `json:"version"`
		}
		_ = json.Unmarshal([]byte(line), &extra)
		appLogf("pipe: hello from=%s version=%d", extra.From, extra.Version)
	case "open":
		// §5.4：必须回 UI 线程——读协程只 PostMessage，不碰窗口
		pPostMessageW.Call(uintptr(hwnd), uintptr(msgTodoOpen), 0, 0)
	default:
		// 其它 type：忽略
	}
}

func indexBytePipe(b []byte, c byte) int {
	for i := range b {
		if b[i] == c {
			return i
		}
	}
	return -1
}

func pipeWriteLine(conn windows.Handle, line []byte) error {
	buf := make([]byte, 0, len(line)+1)
	buf = append(buf, line...)
	buf = append(buf, '\n')
	var n uint32
	return windows.WriteFile(conn, buf, &n, nil)
}

// pipeTickLoop：1s tick（B3）——由最近一次的 todos 计算到期事件，经 pipeSend 推送。
// B6 种子：首次拿到非空 todos 时做一次种子扫描（超期 ≤10min 补推 / >10min 仅标记）。
func pipeTickLoop() {
	t := time.NewTicker(dueTickInterval)
	defer t.Stop()
	for {
		select {
		case <-pipeDoneCh:
			return
		case <-t.C:
			pipeMu.Lock()
			todos := pipeTodos
			notified := pipeNotified
			seeded := pipeSeeded
			pipeMu.Unlock()
			if todos == nil {
				continue // 尚未收到首次数据
			}
			events := pipeDueCompute(todos, time.Now(), notified, seeded)
			pipeMu.Lock()
			pipeSeeded = true
			pipeMu.Unlock()
			for _, ev := range events {
				line, err := json.Marshal(ev)
				if err == nil {
					pipeSend(append(line, '\n'))
				}
			}
		}
	}
}
