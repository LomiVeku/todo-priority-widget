package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// BridgeLink 描述一个 ai-bridge 对端的目录与编号约定。
// 核心防冲突原则：一个目录只有一方写。
type BridgeLink struct {
	WriteDir    string `json:"writeDir"`    // 我方发出的消息写到这里（如 to-mimo）
	ReadDir     string `json:"readDir"`     // 对方发来的消息在这里（如 to-deepseek）
	TheirPrefix string `json:"theirPrefix"` // 对方的 ID 前缀，如 M
	MyPrefix    string `json:"myPrefix"`    // 我方的 ID 前缀，如 D
}

// BridgeConfig 是 ai-bridge 互操作层的配置。
type BridgeConfig struct {
	Enabled bool                  `json:"enabled"`
	Root    string                `json:"root"`
	Links   map[string]BridgeLink `json:"links"`
}

// bridgeEnabled 判断互操作层是否可用。
func (a *App) bridgeEnabled() bool {
	return a.cfg.Bridge.Enabled && a.cfg.Bridge.Root != "" && len(a.cfg.Bridge.Links) > 0
}

// parseTS 解析多种时间格式，统一出口。
// mimo 的 bridge.ps1 用 Get-Date -Format o，会带 7 位小数秒，故需 RFC3339Nano。
func parseTS(s string) time.Time {
	s = strings.TrimSpace(s)
	for _, f := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.999999999-07:00"} {
		if t, err := time.Parse(f, s); err == nil {
			return t
		}
	}
	return time.Now()
}

// parseBridgeDoc 解析 YAML 头 + 正文，返回头部字段与正文。
func parseBridgeDoc(data []byte) (map[string]string, string) {
	s := strings.TrimPrefix(string(data), "\ufeff")
	head := map[string]string{}
	if !strings.HasPrefix(s, "---") {
		return head, strings.TrimSpace(s)
	}
	nl := strings.Index(s, "\n")
	if nl < 0 {
		return head, ""
	}
	rest := s[nl+1:]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return head, strings.TrimSpace(rest)
	}
	for _, line := range strings.Split(rest[:end], "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k := strings.Index(line, ":")
		if k <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:k])
		val := strings.Trim(strings.TrimSpace(line[k+1:]), `"'`)
		head[key] = val
	}
	return head, strings.TrimSpace(rest[end+4:])
}

// slugify 生成与 ai-bridge 约定一致的文件名片段：保留中英文与数字，其余转连字符，截断 24 字符。
func slugify(s string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(s) {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r >= 0x4e00 && r <= 0x9fff:
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	for strings.Contains(out, "--") {
		out = strings.ReplaceAll(out, "--", "-")
	}
	if rs := []rune(out); len(rs) > 24 {
		out = string(rs[:24])
	}
	return out
}

// nextBridgeID 扫描给定目录，按前缀取 max+1。
// 与 mimo 的 Get-NextSeq 行为一致：扫双方的收件目录，保证不撞号。
func nextBridgeID(dirs []string, prefix string) string {
	pat := prefix + "-"
	max := 0
	for _, d := range dirs {
		ents, err := os.ReadDir(d)
		if err != nil {
			continue
		}
		for _, e := range ents {
			n := e.Name()
			if !strings.HasPrefix(n, pat) || !strings.HasSuffix(strings.ToLower(n), ".md") {
				continue
			}
			rest := n[len(pat):]
			cut := strings.IndexAny(rest, "-.")
			if cut <= 0 {
				continue
			}
			if v, err := strconv.Atoi(rest[:cut]); err == nil && v > max {
				max = v
			}
		}
	}
	return fmt.Sprintf("%s-%04d", prefix, max+1)
}

// resolveRef 把 relay 内部消息 ID 转换成对端能理解的引用 ID。
// 若该消息来自桥（带 BridgeID），用桥 ID；否则原样返回。
func (a *App) resolveRef(ref string) string {
	if ref == "" {
		return ""
	}
	list, err := a.mb.List()
	if err == nil {
		for _, m := range list {
			if m.ID == ref || strings.HasPrefix(m.ID, ref) {
				if m.BridgeID != "" {
					return m.BridgeID
				}
				return m.ID
			}
		}
	}
	return ref
}

// bridgeWrite 把一条 relay 消息按 ai-bridge 格式镜像写入对端收件目录。
// 一律带 UTF-8 BOM，与 mimo 的 bridge.ps1 现状对齐，避免其读端乱码。
func (a *App) bridgeWrite(peerName string, link BridgeLink, m *Msg) (string, error) {
	root := a.cfg.Bridge.Root
	dir := filepath.Join(root, link.WriteDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	dirs := []string{filepath.Join(root, link.WriteDir), filepath.Join(root, link.ReadDir)}
	id := nextBridgeID(dirs, link.MyPrefix)

	slug := slugify(m.Subject)
	if slug == "" {
		slug = "message"
	}
	p := filepath.Join(dir, fmt.Sprintf("%s-%s.md", id, slug))

	ts := parseTS(m.Created)
	var b strings.Builder
	b.WriteString("---\n")
	fmt.Fprintf(&b, "id: %s\n", id)
	fmt.Fprintf(&b, "from: %s\n", a.cfg.Self)
	fmt.Fprintf(&b, "to: %s\n", peerName)
	fmt.Fprintf(&b, "ts: %s\n", ts.Format(time.RFC3339))
	if r := a.resolveRef(m.Ref); r != "" {
		fmt.Fprintf(&b, "reply_to: %s\n", r)
	}
	fmt.Fprintf(&b, "subject: %s\n", m.Subject)
	// 以下两个为可选扩展字段：对端不认识时忽略即可
	fmt.Fprintf(&b, "relay_id: %s\n", m.ID)
	fmt.Fprintf(&b, "kind: %s\n", m.Kind)
	b.WriteString("---\n\n")
	b.WriteString(strings.TrimSpace(m.Body))
	b.WriteString("\n")

	data := append([]byte{0xef, 0xbb, 0xbf}, []byte(b.String())...)
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, p); err != nil {
		os.Remove(tmp)
		return "", err
	}
	return p, nil
}

// bridgeImport 扫描对端写入的目录，把未导入过的消息并入 relay 邮箱。
// 按 ai-bridge 协议，读消息的一方负责写 ack/<ID>.ok，因此这里顺带回执。
// 导入是幂等的：以桥 ID 去重，重复调用不会产生副本。
func (a *App) bridgeImport(peerName string, link BridgeLink) (int, error) {
	root := a.cfg.Bridge.Root
	dir := filepath.Join(root, link.ReadDir)
	ents, err := os.ReadDir(dir)
	if err != nil {
		return 0, nil
	}
	existing, err := a.mb.List()
	if err != nil {
		return 0, err
	}
	have := map[string]bool{}
	for _, m := range existing {
		if m.BridgeID != "" {
			have[m.BridgeID] = true
		}
	}

	ackDir := filepath.Join(root, "ack")
	_ = os.MkdirAll(ackDir, 0o755)

	n := 0
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".md") {
			continue
		}
		p := filepath.Join(dir, e.Name())
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		head, body := parseBridgeDoc(data)
		bid := head["id"]
		if bid == "" || have[bid] {
			continue
		}
		kind := head["kind"]
		if kind == "" {
			kind = "note"
		}
		from := head["from"]
		if from == "" {
			from = peerName
		}
		m := NewMsg(from, a.cfg.Self, kind, head["subject"], body, head["reply_to"])
		m.Created = parseTS(head["ts"]).Format(time.RFC3339)
		m.BridgeID = bid
		m.File = e.Name()
		if _, err := a.mb.Put(m); err != nil {
			continue
		}
		// 替对端回执：我方已读
		ack := filepath.Join(ackDir, bid+".ok")
		if _, err := os.Stat(ack); err != nil {
			_ = os.WriteFile(ack, []byte(time.Now().Format(time.RFC3339)+" acked by "+a.cfg.Self+" (relay)\n"), 0o644)
		}
		have[bid] = true
		n++
	}
	return n, nil
}

// bridgeAck 在 relay 侧标记已读时，同步写 ai-bridge 的 ack 文件。
func (a *App) bridgeAck(m *Msg) {
	if !a.bridgeEnabled() || m.BridgeID == "" {
		return
	}
	ackDir := filepath.Join(a.cfg.Bridge.Root, "ack")
	_ = os.MkdirAll(ackDir, 0o755)
	p := filepath.Join(ackDir, m.BridgeID+".ok")
	if _, err := os.Stat(p); err == nil {
		return
	}
	_ = os.WriteFile(p, []byte(time.Now().Format(time.RFC3339)+" acked by "+a.cfg.Self+" (relay)\n"), 0o644)
}

// bridgeImportAll 对所有已配置的桥对端执行导入，返回总条数。
func (a *App) bridgeImportAll() int {
	if !a.bridgeEnabled() {
		return 0
	}
	total := 0
	for name, link := range a.cfg.Bridge.Links {
		n, _ := a.bridgeImport(name, link)
		total += n
	}
	return total
}

// bridgeWriteFor 若该对端配置了桥，则镜像写出，返回落盘路径（未配置返回空）。
func (a *App) bridgeWriteFor(peerName string, m *Msg) string {
	if !a.bridgeEnabled() {
		return ""
	}
	link, ok := a.cfg.Bridge.Links[peerName]
	if !ok {
		return ""
	}
	p, err := a.bridgeWrite(peerName, link, m)
	if err != nil {
		return ""
	}
	return p
}
