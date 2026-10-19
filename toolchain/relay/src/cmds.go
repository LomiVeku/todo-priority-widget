package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// App 汇集一次调用所需的运行期上下文。
type App struct {
	cfg     *Config
	mb      *Mailbox
	oc      *OClient
	jsonOut bool
}

func (a *App) client() (*OClient, error) {
	if a.oc != nil {
		return a.oc, nil
	}
	oc, err := newOClient(a.cfg)
	if err != nil {
		return nil, err
	}
	a.oc = oc
	return a.oc, nil
}

// peerNames 返回排序后的对端名字，保证输出顺序稳定（map 遍历本身无序）。
func (a *App) peerNames() []string {
	names := make([]string, 0, len(a.cfg.Peers))
	for k := range a.cfg.Peers {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// ---------- 输出辅助 ----------

// normPath 统一斜杠方向与大小写，使 --dir 用 / 或 \ 都能匹配。
func normPath(s string) string {
	return strings.ToLower(strings.ReplaceAll(s, "\\", "/"))
}

func dispWidth(s string) int {
	w := 0
	for _, r := range s {
		switch {
		case r >= 0x1100 && r <= 0x115f,
			r >= 0x2e80 && r <= 0xa4cf,
			r >= 0xac00 && r <= 0xd7a3,
			r >= 0xf900 && r <= 0xfaff,
			r >= 0xfe30 && r <= 0xfe6f,
			r >= 0xff00 && r <= 0xff60,
			r >= 0xffe0 && r <= 0xffe6:
			w += 2
		default:
			w++
		}
	}
	return w
}

func pad(s string, w int) string {
	if d := dispWidth(s); d < w {
		return s + strings.Repeat(" ", w-d)
	}
	return s
}

func clip(s string, w int) string {
	if dispWidth(s) <= w {
		return s
	}
	var b strings.Builder
	cur := 0
	for _, r := range s {
		rw := dispWidth(string(r))
		if cur+rw > w-1 {
			break
		}
		b.WriteRune(r)
		cur += rw
	}
	return b.String() + "~"
}

func fmtMS(ms int64) string {
	if ms <= 0 {
		return "-"
	}
	return time.UnixMilli(ms).Format("01-02 15:04:05")
}

func kindCN(k string) string {
	switch k {
	case "task":
		return "任务"
	case "reply":
		return "回复"
	case "note":
		return "通知"
	default:
		return k
	}
}

func printJSON(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

// ---------- 参数解析 ----------

var valueFlags = map[string]bool{
	"timeout": true, "file": true, "subject": true, "kind": true,
	"dir": true, "desc": true, "since": true, "session": true,
	"agent": true, "model": true, "config": true, "limit": true,
	"peer": true, "ref": true, "title": true, "body": true,
	"self": true, "mailbox": true,
}

func parseArgs(args []string) (pos []string, flags map[string]string) {
	flags = map[string]string{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "--") {
			name := a[2:]
			if j := strings.IndexByte(name, '='); j >= 0 {
				flags[name[:j]] = name[j+1:]
				continue
			}
			if valueFlags[name] && i+1 < len(args) {
				flags[name] = args[i+1]
				i++
				continue
			}
			flags[name] = "true"
			continue
		}
		pos = append(pos, a)
	}
	return
}

func flagBool(flags map[string]string, name string) bool {
	v, ok := flags[name]
	return ok && v != "false" && v != "0"
}

func flagInt(flags map[string]string, name string, def int) int {
	if v, ok := flags[name]; ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return n
		}
	}
	return def
}

// ---------- 子命令 ----------

func cmdInit(a *App, pos []string, flags map[string]string) error {
	if v, ok := flags["self"]; ok && v != "" {
		a.cfg.Self = v
	}
	if v, ok := flags["mailbox"]; ok && v != "" {
		a.cfg.Mailbox = v
	}
	if err := a.cfg.Save(); err != nil {
		return err
	}
	fmt.Printf("配置已写入: %s\n", a.cfg.Path())
	fmt.Printf("  身份(self) : %s\n", a.cfg.Self)
	fmt.Printf("  邮箱(mailbox): %s\n", a.cfg.Mailbox)
	return nil
}

func cmdSelf(a *App, pos []string, flags map[string]string) error {
	exe, _ := os.Executable()
	info := map[string]any{
		"self":    a.cfg.Self,
		"mailbox": a.cfg.Mailbox,
		"config":  a.cfg.Path(),
		"exe":     exe,
	}
	if a.jsonOut {
		printJSON(info)
		return nil
	}
	fmt.Printf("身份      : %s\n", a.cfg.Self)
	fmt.Printf("邮箱目录  : %s\n", a.cfg.Mailbox)
	fmt.Printf("配置文件  : %s\n", a.cfg.Path())
	fmt.Printf("程序位置  : %s\n", exe)
	url, pw, err := a.cfg.resolveService()
	if err != nil {
		fmt.Printf("opencode  : 不可用 (%v)\n", err)
	} else {
		fmt.Printf("opencode  : %s  凭据已发现(%d 字符)\n", url, len(pw))
	}
	if len(a.cfg.Peers) == 0 {
		fmt.Printf("对端      : 尚未配置，用 relay bind <名字> <sessionID> 添加\n")
	} else {
		fmt.Printf("对端      :\n")
		for _, k := range a.peerNames() {
			p := a.cfg.Peers[k]
			fmt.Printf("  %-10s %s  %s\n", k, orDash(p.Session), p.Dir)
		}
	}
	return nil
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func cmdLs(a *App, pos []string, flags map[string]string) error {
	oc, err := a.client()
	if err != nil {
		return err
	}
	list, err := oc.Sessions()
	if err != nil {
		return err
	}
	dirFilter := flags["dir"]
	if a.jsonOut {
		printJSON(list)
		return nil
	}
	fmt.Printf("%-32s %-8s %-22s %-13s %-22s %s\n", "会话 ID", "状态", "标题", "目录", "Agent/模型", "最后活动")
	fmt.Println(strings.Repeat("-", 122))
	for _, s := range list {
		if dirFilter != "" && !strings.Contains(normPath(s.Location.Directory), normPath(dirFilter)) {
			continue
		}
		agent := orDash(s.Agent)
		if s.Model != nil && s.Model.ID != "" {
			agent = fmt.Sprintf("%s/%s", orDash(s.Agent), s.Model.ID)
		}
		busy := "空闲"
		if s.Time.Updated > s.Time.Idle && s.Time.Idle > 0 {
			busy = "运行中"
		}
		fmt.Printf("%-32s %-8s %-22s %-13s %-22s %s\n",
			s.ID, busy, clip(s.Title, 20), clip(filepath.Base(s.Location.Directory), 11),
			clip(agent, 20), fmtMS(s.Time.Updated))
	}
	return nil
}

func cmdBind(a *App, pos []string, flags map[string]string) error {
	if len(pos) < 1 {
		return fmt.Errorf("用法: relay bind <名字> <sessionID|auto> [--dir D:\\path] [--desc 说明]\n" +
			"      relay bind glm --latest --dir \"D:\\待办事项优先级提示窗\"   自动选该目录下最新会话")
	}
	name := pos[0]
	sid := ""
	if len(pos) >= 2 {
		sid = pos[1]
	}
	dir := flags["dir"]

	// auto / --latest：按目录筛选出最近活动的会话，省去手工抄 ID
	if sid == "" || strings.EqualFold(sid, "auto") || flagBool(flags, "latest") {
		oc, err := a.client()
		if err != nil {
			return err
		}
		list, err := oc.Sessions()
		if err != nil {
			return err
		}
		var best *Session
		for i := range list {
			s := list[i]
			if strings.Contains(strings.ToLower(s.Title), "relay") {
				continue // 跳过中转自检类会话
			}
			if dir != "" && !strings.Contains(normPath(s.Location.Directory), normPath(dir)) {
				continue
			}
			if best == nil || s.Time.Updated > best.Time.Updated {
				best = &list[i]
			}
		}
		if best == nil {
			return fmt.Errorf("没有找到匹配的会话（--dir %q），先用 relay ls 查看", dir)
		}
		sid = best.ID
		if dir == "" {
			dir = best.Location.Directory
		}
		fmt.Printf("自动选中会话: %s  %s  %s\n", best.ID, clip(best.Title, 30), best.Location.Directory)
	}

	p := a.cfg.Peers[name]
	p.Session = sid
	if dir != "" {
		p.Dir = dir
	}
	if v := flags["desc"]; v != "" {
		p.Desc = v
	}
	if v := flags["agent"]; v != "" {
		p.Agent = v
	}
	if v := flags["model"]; v != "" {
		p.Model = v
	}
	a.cfg.Peers[name] = p
	if err := a.cfg.Save(); err != nil {
		return err
	}
	fmt.Printf("已绑定对端 %s -> %s\n", name, sid)
	return nil
}

func cmdUnbind(a *App, pos []string, flags map[string]string) error {
	if len(pos) < 1 {
		return fmt.Errorf("用法: relay unbind <名字>")
	}
	name := pos[0]
	if _, ok := a.cfg.Peers[name]; !ok {
		return fmt.Errorf("对端 %s 不存在", name)
	}
	delete(a.cfg.Peers, name)
	if err := a.cfg.Save(); err != nil {
		return err
	}
	fmt.Printf("已解绑对端 %s\n", name)
	return nil
}

func cmdPeers(a *App, pos []string, flags map[string]string) error {
	if a.jsonOut {
		printJSON(a.cfg.Peers)
		return nil
	}
	if len(a.cfg.Peers) == 0 {
		fmt.Println("尚未配置对端。用 relay ls 查看会话，再 relay bind <名字> <sessionID>")
		return nil
	}
	oc, _ := a.client()
	fmt.Printf("%-10s %-32s %-9s %-14s %s\n", "名字", "会话 ID", "状态", "目录", "说明")
	fmt.Println(strings.Repeat("-", 105))
	for _, k := range a.peerNames() {
		p := a.cfg.Peers[k]
		st := "-"
		if oc != nil && p.Session != "" {
			if s, err := oc.Session(p.Session); err == nil {
				st = "空闲"
				if s.Time.Updated > s.Time.Idle && s.Time.Idle > 0 {
					st = "运行中"
				}
			} else {
				st = "不可达"
			}
		}
		fmt.Printf("%-10s %-32s %-9s %-14s %s\n", k, orDash(p.Session), st,
			clip(filepath.Base(p.Dir), 12), clip(p.Desc, 30))
	}
	return nil
}

// buildPromptText 构造注入到对端会话的文本。
// 关键：显式告知对端如何回传，形成闭环。
func buildPromptText(m *Msg) string {
	exe, _ := os.Executable()
	var b strings.Builder
	fmt.Fprintf(&b, "[relay] 来自 %s 的%s (id=%s)\n", m.From, kindCN(m.Kind), m.ID)
	if m.Subject != "" {
		fmt.Fprintf(&b, "主题: %s\n", m.Subject)
	}
	if m.Ref != "" {
		fmt.Fprintf(&b, "关联: %s\n", m.Ref)
	}
	b.WriteString("--------------------------------\n")
	b.WriteString(m.Body)
	b.WriteString("\n--------------------------------\n")
	b.WriteString("完成后请执行以下命令回传结果：\n")
	fmt.Fprintf(&b, "  \"%s\" reply %s \"<结果摘要>\" --ref %s\n", exe, m.From, m.ID)
	return b.String()
}

// doSend 是 send/reply 的公共实现：先落邮箱，再镜像到 ai-bridge 桥，最后尝试注入对端会话。
// 三个落点按可靠性排序：邮箱（必成功）> 桥文件（本地写）> 会话注入（依赖服务）。
func doSend(a *App, to, kind, subject, body, ref string, noPush bool) (*Msg, string, bool, string, error) {
	name, peer, ok := a.cfg.LookupPeer(to)
	if !ok {
		return nil, "", false, "", fmt.Errorf("对端 %q 未配置。先用 relay bind %s <sessionID> 绑定，或用 relay peers 查看", to, to)
	}
	m := NewMsg(a.cfg.Self, name, kind, subject, body, ref)
	path, err := a.mb.Put(m)
	if err != nil {
		return nil, "", false, "", err
	}
	bridgePath := a.bridgeWriteFor(name, m)
	pushed := false
	if !noPush && peer.Session != "" {
		oc, err := a.client()
		if err == nil {
			if _, err := oc.Prompt(peer.Session, buildPromptText(m)); err == nil {
				pushed = true
				m.Pushed = true
				_ = a.mb.Update(m)
			}
		}
	}
	return m, path, pushed, bridgePath, nil
}

func cmdSend(a *App, pos []string, flags map[string]string) error {
	if len(pos) < 1 {
		return fmt.Errorf("用法: relay send <对端> <内容...> | --file <路径> [--subject 主题] [--kind task|note]")
	}
	to := pos[0]
	body := strings.Join(pos[1:], " ")
	if f := flags["file"]; f != "" {
		data, err := os.ReadFile(f)
		if err != nil {
			return fmt.Errorf("读取文件失败: %w", err)
		}
		body = string(data)
	} else if b := flags["body"]; b != "" {
		body = b
	}
	if strings.TrimSpace(body) == "" {
		return fmt.Errorf("消息内容为空")
	}
	kind := flags["kind"]
	if kind == "" {
		kind = "task"
	}
	m, path, pushed, bridgePath, err := doSend(a, to, kind, flags["subject"], body, flags["ref"], flagBool(flags, "no-push"))
	if err != nil {
		return err
	}
	if a.jsonOut {
		printJSON(map[string]any{"id": m.ID, "file": path, "pushed": pushed, "bridge": bridgePath})
		return nil
	}
	fmt.Printf("[已发送] %s  %s -> %s  (%s)\n", m.ID, a.cfg.Self, m.To, kindCN(m.Kind))
	fmt.Printf("  邮箱: %s\n", path)
	if bridgePath != "" {
		fmt.Printf("  桥接: %s\n", bridgePath)
	}
	if pushed {
		fmt.Printf("  投递: 已注入会话 %s\n", a.cfg.Peers[m.To].Session)
	} else if !flagBool(flags, "no-push") {
		p := a.cfg.Peers[m.To]
		if p.Session == "" {
			fmt.Printf("  投递: 仅落邮箱（对端未绑定会话）\n")
		} else {
			fmt.Printf("  投递: 注入失败，消息已保存在邮箱，稍后可用 relay push %s 重试\n", m.To)
		}
	}
	return nil
}

func cmdReply(a *App, pos []string, flags map[string]string) error {
	if len(pos) < 1 {
		return fmt.Errorf("用法: relay reply <对端> <内容...> | --file <路径> [--ref <消息ID>]")
	}
	if flags["kind"] == "" {
		flags["kind"] = "reply"
	}
	return cmdSend(a, pos, flags)
}

func cmdPush(a *App, pos []string, flags map[string]string) error {
	if len(pos) < 1 {
		return fmt.Errorf("用法: relay push <对端> [--all]")
	}
	to := pos[0]
	name, peer, ok := a.cfg.LookupPeer(to)
	if !ok {
		return fmt.Errorf("对端 %q 未配置", to)
	}
	if peer.Session == "" {
		return fmt.Errorf("对端 %s 未绑定会话，无法注入", name)
	}
	oc, err := a.client()
	if err != nil {
		return err
	}
	list, err := a.mb.Sent(a.cfg.Self)
	if err != nil {
		return err
	}
	n := 0
	for _, m := range list {
		if !strings.EqualFold(m.To, name) || m.Pushed {
			continue
		}
		if !flagBool(flags, "all") && n >= 1 {
			break
		}
		if _, err := oc.Prompt(peer.Session, buildPromptText(m)); err != nil {
			return err
		}
		m.Pushed = true
		_ = a.mb.Update(m)
		fmt.Printf("已补投 %s\n", m.ID)
		n++
	}
	if n == 0 {
		fmt.Println("没有待补投的消息")
	}
	return nil
}

func cmdInbox(a *App, pos []string, flags map[string]string) error {
	// 先从 ai-bridge 桥导入对端新消息（幂等），使 relay inbox 成为统一收件入口
	if n := a.bridgeImportAll(); n > 0 {
		fmt.Printf("从 ai-bridge 桥导入 %d 条新消息\n", n)
	}
	unreadOnly := !flagBool(flags, "all")
	from := flags["peer"]
	list, err := a.mb.Inbox(a.cfg.Self, unreadOnly, from)
	if err != nil {
		return err
	}
	if a.jsonOut {
		printJSON(list)
		return nil
	}
	if len(list) == 0 {
		if unreadOnly {
			fmt.Printf("%s 的收件箱没有未读消息（用 --all 查看全部）\n", a.cfg.Self)
		} else {
			fmt.Printf("%s 的收件箱为空\n", a.cfg.Self)
		}
		return nil
	}
	fmt.Printf("%s 的收件箱（%s）\n", a.cfg.Self, map[bool]string{true: "仅未读", false: "全部"}[unreadOnly])
	fmt.Printf("%-3s %-22s %-11s %-12s %-8s %s\n", "", "消息 ID", "来自", "时间", "类型", "摘要")
	fmt.Println(strings.Repeat("-", 100))
	for _, m := range list {
		mark := " "
		if m.Unread {
			mark = "*"
		}
		t, _ := time.Parse(time.RFC3339, m.Created)
		sub := m.Subject
		if sub == "" {
			sub = clip(strings.ReplaceAll(m.Body, "\n", " "), 40)
		}
		fmt.Printf("%-3s %-22s %-11s %-12s %-8s %s\n", mark, m.ID, m.From,
			t.Local().Format("01-02 15:04"), kindCN(m.Kind), clip(sub, 40))
	}
	fmt.Println("\n(* = 未读；relay read <消息ID> 查看全文，relay ack <消息ID> 标记已读)")
	return nil
}

func cmdRead(a *App, pos []string, flags map[string]string) error {
	if len(pos) < 1 {
		return fmt.Errorf("用法: relay read <消息ID>")
	}
	m, _, err := a.mb.Get(pos[0])
	if err != nil {
		return err
	}
	if a.jsonOut {
		printJSON(m)
		return nil
	}
	t, _ := time.Parse(time.RFC3339, m.Created)
	fmt.Printf("消息 ID : %s\n", m.ID)
	fmt.Printf("发件人  : %s\n", m.From)
	fmt.Printf("收件人  : %s\n", m.To)
	fmt.Printf("类型    : %s\n", kindCN(m.Kind))
	if m.Subject != "" {
		fmt.Printf("主题    : %s\n", m.Subject)
	}
	if m.Ref != "" {
		fmt.Printf("关联    : %s\n", m.Ref)
	}
	fmt.Printf("时间    : %s\n", t.Local().Format("2006-01-02 15:04:05"))
	fmt.Printf("已投递  : %v\n", m.Pushed)
	fmt.Println(strings.Repeat("-", 70))
	fmt.Println(m.Body)
	return nil
}

func cmdAck(a *App, pos []string, flags map[string]string) error {
	var targets []*Msg
	if flagBool(flags, "all") {
		list, err := a.mb.Inbox(a.cfg.Self, true, "")
		if err != nil {
			return err
		}
		targets = list
	} else {
		if len(pos) < 1 {
			return fmt.Errorf("用法: relay ack <消息ID...> | --all")
		}
		for _, id := range pos {
			m, _, err := a.mb.Get(id)
			if err != nil {
				fmt.Fprintf(os.Stderr, "跳过 %s: %v\n", id, err)
				continue
			}
			targets = append(targets, m)
		}
	}
	if len(targets) == 0 {
		fmt.Println("没有可标记的消息")
		return nil
	}
	ids := make([]string, 0, len(targets))
	for _, m := range targets {
		ids = append(ids, m.ID)
	}
	n, err := a.mb.MarkRead(ids)
	if err != nil {
		return err
	}
	// 同步写 ai-bridge 的 ack 回执，让对端知道自己已被读到
	for _, m := range targets {
		a.bridgeAck(m)
	}
	fmt.Printf("已标记 %d 条为已读\n", n)
	return nil
}

func cmdWait(a *App, pos []string, flags map[string]string) error {
	if len(pos) < 1 {
		return fmt.Errorf("用法: relay wait <对端> [--timeout 600] [--since <消息ID>]")
	}
	name, peer, ok := a.cfg.LookupPeer(pos[0])
	if !ok {
		return fmt.Errorf("对端 %q 未配置", pos[0])
	}
	if peer.Session == "" {
		return fmt.Errorf("对端 %s 未绑定会话，无法等待", name)
	}
	oc, err := a.client()
	if err != nil {
		return err
	}
	since := time.Now().UnixMilli()
	if sid := flags["since"]; sid != "" {
		if m, _, err := a.mb.Get(sid); err == nil {
			if t, err := time.Parse(time.RFC3339, m.Created); err == nil {
				since = t.UnixMilli()
			}
		}
	}
	timeout := time.Duration(flagInt(flags, "timeout", 600)) * time.Second
	fmt.Printf("等待 %s (%s) 完成，超时 %s ...\n", name, peer.Session, timeout)
	res, err := oc.WaitDone(peer.Session, since, timeout)
	if err != nil {
		return err
	}
	if res.TimedOut {
		fmt.Printf("[超时] 已等待 %s（轮询 %d 次），期间收到 %d 条回复\n",
			res.Elapsed.Round(time.Second), res.PollCount, len(res.Replies))
	}
	if txt := res.ReplyText(); txt != "" {
		fmt.Println(strings.Repeat("-", 70))
		fmt.Println(txt)
	} else {
		fmt.Println("(未收到文本回复)")
	}
	return nil
}

func cmdAsk(a *App, pos []string, flags map[string]string) error {
	if len(pos) < 2 {
		return fmt.Errorf("用法: relay ask <对端> <内容...> [--timeout 600]")
	}
	to := pos[0]
	body := strings.Join(pos[1:], " ")
	if f := flags["file"]; f != "" {
		data, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		body = string(data)
	}
	kind := flags["kind"]
	if kind == "" {
		kind = "task"
	}
	m, path, pushed, bridgePath, err := doSend(a, to, kind, flags["subject"], body, flags["ref"], false)
	if err != nil {
		return err
	}
	fmt.Printf("[已发送] %s -> %s  邮箱: %s  投递: %v\n", m.ID, m.To, path, pushed)
	if bridgePath != "" {
		fmt.Printf("  桥接: %s\n", bridgePath)
	}
	if !pushed {
		return fmt.Errorf("消息未注入对端会话，无法等待回复；请确认对端已绑定且服务可用")
	}
	name, peer, _ := a.cfg.LookupPeer(to)
	oc, err := a.client()
	if err != nil {
		return err
	}
	timeout := time.Duration(flagInt(flags, "timeout", 900)) * time.Second
	// 以消息创建时间为起点，确保能捕获到本次回复
	t0, _ := time.Parse(time.RFC3339, m.Created)
	fmt.Printf("等待 %s (%s) 完成，超时 %s ...\n", name, peer.Session, timeout)
	res, err := oc.WaitDone(peer.Session, t0.UnixMilli(), timeout)
	if err != nil {
		return err
	}
	if res.TimedOut {
		fmt.Printf("[超时] 已等待 %s\n", res.Elapsed.Round(time.Second))
	}
	txt := res.ReplyText()
	if txt == "" {
		txt = "(未收到文本回复)"
	}
	fmt.Println(strings.Repeat("=", 70))
	fmt.Println(txt)
	fmt.Println(strings.Repeat("=", 70))
	// 将对端回复归档到邮箱，形成可追溯的双向记录
	if res.ReplyText() != "" {
		rm := NewMsg(name, a.cfg.Self, "reply", "对 "+m.ID+" 的回复", txt, m.ID)
		if _, err := a.mb.Put(rm); err == nil {
			fmt.Printf("回复已归档到邮箱: %s\n", rm.ID)
		}
	}
	return nil
}

func cmdStatus(a *App, pos []string, flags map[string]string) error {
	inbox, err := a.mb.Inbox(a.cfg.Self, true, "")
	if err != nil {
		return err
	}
	sent, _ := a.mb.Sent(a.cfg.Self)
	fmt.Printf("relay 状态\n")
	fmt.Println(strings.Repeat("-", 60))
	fmt.Printf("身份        : %s\n", a.cfg.Self)
	fmt.Printf("邮箱目录    : %s\n", a.cfg.Mailbox)
	fmt.Printf("未读消息    : %d 条\n", len(inbox))
	fmt.Printf("已发消息    : %d 条\n", len(sent))
	fmt.Printf("对端数量    : %d\n", len(a.cfg.Peers))
	if a.bridgeEnabled() {
		fmt.Printf("ai-bridge   : %s\n", a.cfg.Bridge.Root)
		for name, l := range a.cfg.Bridge.Links {
			fmt.Printf("  桥接 %-8s 写 %-10s 读 %-12s (我 %s- / 对方 %s-)\n",
				name, l.WriteDir, l.ReadDir, l.MyPrefix, l.TheirPrefix)
		}
	}
	oc, err := a.client()
	if err != nil {
		fmt.Printf("opencode    : 不可用 (%v)\n", err)
		return nil
	}
	url, _, _ := a.cfg.resolveService()
	fmt.Printf("opencode    : %s 已连接\n", url)
	for _, k := range a.peerNames() {
		p := a.cfg.Peers[k]
		if p.Session == "" {
			fmt.Printf("  %-10s 未绑定会话\n", k)
			continue
		}
		s, err := oc.Session(p.Session)
		if err != nil {
			fmt.Printf("  %-10s %s 不可达\n", k, p.Session)
			continue
		}
		st := "空闲"
		if s.Time.Updated > s.Time.Idle && s.Time.Idle > 0 {
			st = "运行中"
		}
		fmt.Printf("  %-10s %s  %s  最后活动 %s\n", k, s.ID, st, fmtMS(s.Time.Updated))
	}
	if len(inbox) > 0 {
		fmt.Println("\n未读消息:")
		for _, m := range inbox {
			fmt.Printf("  %s  %s  %s\n", m.ID, m.From, clip(strings.ReplaceAll(m.Body, "\n", " "), 50))
		}
	}
	return nil
}
