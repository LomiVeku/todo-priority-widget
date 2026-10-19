package main

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Msg 是邮箱中的一条消息。它是跨会话通信的持久化载体：
// 即使对端会话尚未建立或已关闭，消息依然留在磁盘上，可追溯、可重放。
type Msg struct {
	ID      string `json:"id"`
	From    string `json:"from"`
	To      string `json:"to"`
	Kind    string `json:"kind"`              // task | reply | note
	Subject string `json:"subject,omitempty"` // 一句话摘要
	Body    string `json:"body"`
	Ref     string `json:"ref,omitempty"` // 关联的上一条消息 ID
	Created string `json:"created"`       // RFC3339
	Unread  bool   `json:"unread"`        // 对收件方而言是否未读
	Pushed  bool   `json:"pushed"`        // 是否已注入对端 opencode 会话
	Note    string `json:"note,omitempty"`
	// BridgeID 为 ai-bridge 侧的原始 ID（如 M-0002）；为空表示非桥来源。
	// 用于幂等导入与跨桥引用。
	BridgeID string `json:"bridgeId,omitempty"`
	// File 记录桥侧原始文件名，便于回溯。
	File string `json:"file,omitempty"`
}

// Mailbox 是以目录为载体的消息队列。
type Mailbox struct{ Dir string }

// NewMailbox 打开（必要时创建）邮箱目录。
func NewMailbox(dir string) (*Mailbox, error) {
	if dir == "" {
		return nil, fmt.Errorf("邮箱目录未配置")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("创建邮箱目录失败: %w", err)
	}
	return &Mailbox{Dir: dir}, nil
}

func randHex(n int) string {
	b := make([]byte, (n+1)/2)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%0*x", n, time.Now().UnixNano()&0xffffff)
	}
	return fmt.Sprintf("%x", b)[:n]
}

// NewMsg 构造一条待发送消息。
func NewMsg(from, to, kind, subject, body, ref string) *Msg {
	now := time.Now()
	return &Msg{
		ID:      fmt.Sprintf("m%d%s", now.UnixMilli(), randHex(4)),
		From:    from,
		To:      to,
		Kind:    kind,
		Subject: subject,
		Body:    body,
		Ref:     ref,
		Created: now.Format(time.RFC3339),
		Unread:  true,
	}
}

func (mb *Mailbox) pathFor(m *Msg) string {
	t, err := time.Parse(time.RFC3339, m.Created)
	if err != nil {
		t = time.Now()
	}
	name := fmt.Sprintf("%013d-%s-to-%s-%s.json", t.UnixMilli(), safe(m.From), safe(m.To), m.ID)
	return filepath.Join(mb.Dir, name)
}

// safe 将名字中不适宜作为文件名的字符替换掉。
func safe(s string) string {
	if s == "" {
		return "unknown"
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return b.String()
}

// Put 原子写入一条消息，返回落盘路径。
func (mb *Mailbox) Put(m *Msg) (string, error) {
	if m.ID == "" {
		m.ID = NewMsg(m.From, m.To, m.Kind, m.Subject, m.Body, m.Ref).ID
	}
	if m.Created == "" {
		m.Created = time.Now().Format(time.RFC3339)
	}
	p := mb.pathFor(m)
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return "", err
	}
	data = append(data, '\n')
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return "", fmt.Errorf("写入邮箱失败: %w", err)
	}
	if err := os.Rename(tmp, p); err != nil {
		os.Remove(tmp)
		return "", fmt.Errorf("落盘邮箱消息失败: %w", err)
	}
	return p, nil
}

// List 读取全部消息，按时间升序（文件名前缀为补零毫秒时间戳，字典序即时间序）。
func (mb *Mailbox) List() ([]*Msg, error) {
	ents, err := os.ReadDir(mb.Dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var files []string
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		files = append(files, e.Name())
	}
	sort.Strings(files)
	out := make([]*Msg, 0, len(files))
	for _, f := range files {
		data, err := os.ReadFile(filepath.Join(mb.Dir, f))
		if err != nil {
			continue
		}
		var m Msg
		if err := json.Unmarshal(data, &m); err != nil {
			continue
		}
		out = append(out, &m)
	}
	return out, nil
}

// Get 按 ID 或 ID 前缀查找消息，返回消息与其文件路径。
func (mb *Mailbox) Get(idOrPrefix string) (*Msg, string, error) {
	list, err := mb.List()
	if err != nil {
		return nil, "", err
	}
	var hit *Msg
	for _, m := range list {
		if m.ID == idOrPrefix || strings.HasPrefix(m.ID, idOrPrefix) {
			if hit != nil {
				return nil, "", fmt.Errorf("ID 前缀 %q 匹配到多条消息，请提供更长的前缀", idOrPrefix)
			}
			hit = m
		}
	}
	if hit == nil {
		return nil, "", fmt.Errorf("未找到消息 %q", idOrPrefix)
	}
	return hit, mb.pathFor(hit), nil
}

// Update 就地更新一条消息。
func (mb *Mailbox) Update(m *Msg) error {
	p := mb.pathFor(m)
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// MarkRead 将指定 ID（或其前缀）的消息标记为已读，返回实际更新条数。
func (mb *Mailbox) MarkRead(ids []string) (int, error) {
	list, err := mb.List()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, m := range list {
		if !m.Unread {
			continue
		}
		for _, want := range ids {
			if m.ID == want || strings.HasPrefix(m.ID, want) {
				m.Unread = false
				if err := mb.Update(m); err != nil {
					return n, err
				}
				n++
				break
			}
		}
	}
	return n, nil
}

// Inbox 返回发往 me 的消息；unreadOnly 为真时只返回未读。
func (mb *Mailbox) Inbox(me string, unreadOnly bool, from string) ([]*Msg, error) {
	list, err := mb.List()
	if err != nil {
		return nil, err
	}
	var out []*Msg
	for _, m := range list {
		if !strings.EqualFold(m.To, me) {
			continue
		}
		if unreadOnly && !m.Unread {
			continue
		}
		if from != "" && !strings.EqualFold(m.From, from) {
			continue
		}
		out = append(out, m)
	}
	return out, nil
}

// Sent 返回 me 发出的消息。
func (mb *Mailbox) Sent(me string) ([]*Msg, error) {
	list, err := mb.List()
	if err != nil {
		return nil, err
	}
	var out []*Msg
	for _, m := range list {
		if strings.EqualFold(m.From, me) {
			out = append(out, m)
		}
	}
	return out, nil
}
