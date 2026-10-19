package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

// OClient 是 opencode 本地服务的 HTTP 客户端。
// 使用 Basic 认证（用户名任意，密码为服务密码），直连环形地址，毫秒级响应。
type OClient struct {
	base string
	pw   string
	hc   *http.Client
}

// ModelRef 描述会话使用的模型。
type ModelRef struct {
	ID         string `json:"id"`
	ProviderID string `json:"providerID"`
	Variant    string `json:"variant,omitempty"`
}

// SessionTime 是会话时间戳（毫秒）。
type SessionTime struct {
	Created int64 `json:"created"`
	Updated int64 `json:"updated"`
	Idle    int64 `json:"idle"`
}

// Session 是 opencode 会话的摘要信息。
type Session struct {
	ID        string      `json:"id"`
	ProjectID string      `json:"projectID"`
	Title     string      `json:"title"`
	Agent     string      `json:"agent"`
	Model     *ModelRef   `json:"model"`
	Outcome   string      `json:"outcome"`
	Time      SessionTime `json:"time"`
	Location  struct {
		Directory string `json:"directory"`
	} `json:"location"`
}

// Part 是消息内容片段。
type Part struct {
	Type string `json:"type"`
	Text string `json:"text"`
	Name string `json:"name"`
}

// Message 是会话中的一条消息。
type Message struct {
	ID        string `json:"id"`
	SessionID string `json:"sessionID"`
	Type      string `json:"type"`
	Content   []Part `json:"content"`
	Payload   struct {
		Text string `json:"text"`
	} `json:"payload"`
	Time struct {
		Created   int64 `json:"created"`
		Completed int64 `json:"completed"`
	} `json:"time"`
}

// Text 提取消息的可读文本。user 消息取 payload.text，assistant 消息拼接 text 片段。
func (m Message) Text() string {
	if m.Type == "user" && m.Payload.Text != "" {
		return m.Payload.Text
	}
	var parts []string
	for _, p := range m.Content {
		if p.Type == "text" && strings.TrimSpace(p.Text) != "" {
			parts = append(parts, p.Text)
		}
	}
	return strings.Join(parts, "\n")
}

type envelope struct {
	Data json.RawMessage `json:"data"`
}

func newOClient(c *Config) (*OClient, error) {
	url, pw, err := c.resolveService()
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(pw) == "" {
		return nil, fmt.Errorf("opencode 服务密码为空")
	}
	return &OClient{
		base: strings.TrimRight(url, "/"),
		pw:   pw,
		hc:   &http.Client{Timeout: 60 * time.Second},
	}, nil
}

func (o *OClient) do(method, path string, body any, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, o.base+path, rdr)
	if err != nil {
		return err
	}
	req.SetBasicAuth("opencode", o.pw)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := o.hc.Do(req)
	if err != nil {
		return fmt.Errorf("连接 opencode 服务失败(%s): %w", o.base, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d %s %s: %s", resp.StatusCode, method, path, truncate(string(data), 240))
	}
	if out == nil || len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	var env envelope
	if err := json.Unmarshal(data, &env); err == nil && len(env.Data) > 0 {
		return json.Unmarshal(env.Data, out)
	}
	return json.Unmarshal(data, out)
}

// Sessions 列出全部会话，按更新时间倒序。
func (o *OClient) Sessions() ([]Session, error) {
	var list []Session
	if err := o.do("GET", "/api/session", nil, &list); err != nil {
		return nil, err
	}
	// 服务返回的顺序不保证，这里显式按更新时间倒序
	for i := 1; i < len(list); i++ {
		for j := i; j > 0 && list[j].Time.Updated > list[j-1].Time.Updated; j-- {
			list[j], list[j-1] = list[j-1], list[j]
		}
	}
	return list, nil
}

// MessageDetail 获取单条消息详情（含 content 正文）。
// opencode 2.0.27 起列表接口只返回轻量条目，正文需按 ID 单独取。
func (o *OClient) MessageDetail(sessionID, msgID string) (*Message, error) {
	var m Message
	if err := o.do("GET", "/api/session/"+sessionID+"/message/"+msgID, nil, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// Session 获取单个会话详情。
func (o *OClient) Session(id string) (*Session, error) {
	var s Session
	if err := o.do("GET", "/api/session/"+id, nil, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// Messages 读取会话消息。limit<=0 时使用服务默认页大小；order 为 asc/desc。
// 注意：opencode 2.0.27 起该接口返回轻量条目（无 content），需要正文时用 MessageDetail。
func (o *OClient) Messages(id string, limit int, order, typ string) ([]Message, error) {
	q := []string{}
	if limit > 0 {
		q = append(q, fmt.Sprintf("limit=%d", limit))
	}
	if order != "" {
		q = append(q, "order="+order)
	}
	if typ != "" {
		q = append(q, "type="+typ)
	}
	path := "/api/session/" + id + "/message"
	if len(q) > 0 {
		path += "?" + strings.Join(q, "&")
	}
	var list []Message
	if err := o.do("GET", path, nil, &list); err != nil {
		return nil, err
	}
	return list, nil
}

// Prompt 向会话投递一条用户消息。
func (o *OClient) Prompt(id, text string) (*Message, error) {
	var m Message
	body := map[string]any{"text": text}
	if err := o.do("POST", "/api/session/"+id+"/prompt", body, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// CreateSession 创建会话。dir 为空时由服务决定工作目录。
func (o *OClient) CreateSession(id, title, dir, agent, model string) (*Session, error) {
	body := map[string]any{}
	if id != "" {
		body["id"] = id
	}
	if title != "" {
		body["title"] = title
	}
	if agent != "" {
		body["agent"] = agent
	}
	if model != "" {
		body["model"] = splitModelRef(model)
	}
	if dir != "" {
		body["location"] = map[string]any{"directory": dir}
	}
	var s Session
	if err := o.do("POST", "/api/session", body, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// DeleteSession 删除会话及其子会话。
func (o *OClient) DeleteSession(id string) error {
	return o.do("DELETE", "/api/session/"+id, nil, nil)
}

// Interrupt 请求打断会话当前生成。
func (o *OClient) Interrupt(id string) error {
	return o.do("POST", "/api/session/"+id+"/interrupt", map[string]any{}, nil)
}

// splitModelRef 将 "provider/id" 或 "id" 解析为模型引用。
func splitModelRef(s string) map[string]string {
	if i := strings.Index(s, "/"); i > 0 {
		return map[string]string{"providerID": s[:i], "id": s[i+1:]}
	}
	return map[string]string{"providerID": "opencode-go", "id": s}
}

// WaitResult 汇总一次等待的结果。
type WaitResult struct {
	TimedOut  bool
	Elapsed   time.Duration
	Replies   []Message // 期间产生的 assistant 消息（时间升序）
	IdleAt    int64
	PollCount int
}

// ReplyText 将期间的所有回复合并为可读文本。
func (w *WaitResult) ReplyText() string {
	var out []string
	for _, m := range w.Replies {
		if t := strings.TrimSpace(m.Text()); t != "" {
			out = append(out, t)
		}
	}
	return strings.Join(out, "\n\n")
}

// WaitDone 轮询等待会话处理完毕。
// sinceMS 为起始时间戳（毫秒）。完成判定基于会话的 idle 时间戳
// （idle >= updated 表示已停止生成，且 idle 晚于本次起点），
// 不依赖消息流里的 idle 记录——后者在长任务下会提前出现，导致漏收回复。
func (o *OClient) WaitDone(id string, sinceMS int64, timeout time.Duration) (*WaitResult, error) {
	start := time.Now()
	deadline := start.Add(timeout)
	res := &WaitResult{}
	seen := map[string]bool{}

	for {
		res.PollCount++
		list, err := o.Messages(id, 50, "desc", "")
		if err != nil {
			return res, err
		}
		var found []Message
		for i := len(list) - 1; i >= 0; i-- { // desc -> 按时间升序遍历
			m := list[i]
			if m.Time.Created <= sinceMS || m.Type != "assistant" {
				continue
			}
			if !seen[m.ID] {
				seen[m.ID] = true
				// 列表接口（opencode 2.0.27 起）只返回轻量条目 {id,time,type,outcome}，
				// 不含 content；正文必须按 ID 再取一次详情，否则 ReplyText() 永远为空。
				if len(m.Content) == 0 {
					if full, err := o.MessageDetail(id, m.ID); err == nil && full != nil {
						if len(full.Content) > 0 {
							m.Content = full.Content
						}
						if full.Payload.Text != "" && m.Payload.Text == "" {
							m.Payload.Text = full.Payload.Text
						}
					}
				}
				found = append(found, m)
			}
		}
		if len(found) > 0 {
			res.Replies = append(res.Replies, found...)
		}

		if s, err := o.Session(id); err == nil && s.Time.Idle > 0 {
			if s.Time.Idle >= s.Time.Updated && s.Time.Idle > sinceMS {
				res.IdleAt = s.Time.Idle
				res.Elapsed = time.Since(start)
				return res, nil
			}
		}

		if time.Now().After(deadline) {
			res.TimedOut = true
			res.Elapsed = time.Since(start)
			return res, nil
		}
		time.Sleep(1500 * time.Millisecond)
	}
}

// Busy 判断会话当前是否仍在生成（用于 --wait 之外的状态展示）。
func (o *OClient) Busy(id string) bool {
	s, err := o.Session(id)
	if err != nil {
		return false
	}
	return s.Time.Updated > s.Time.Idle && s.Time.Idle > 0
}

// filteredEnv 返回剔除会干扰子进程的环境变量后的环境副本。
// ELECTRON_RUN_AS_NODE 会使 opencode-cli.exe 退化为 Node，必须过滤。
func filteredEnv() []string {
	var out []string
	for _, kv := range os.Environ() {
		name := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			name = kv[:i]
		}
		switch strings.ToUpper(name) {
		case "ELECTRON_RUN_AS_NODE", "NODE_OPTIONS":
			continue
		}
		out = append(out, kv)
	}
	return out
}

// cliServiceStatus 通过 opencode-cli.exe 查询后台服务地址（直连不可用时的回退路径）。
func cliServiceStatus(c *Config) (string, error) {
	cli := ocCliPath(c)
	if cli == "" {
		return "", fmt.Errorf("未找到 opencode-cli.exe")
	}
	cmd := exec.Command(cli, "service", "status")
	cmd.Env = filteredEnv()
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("调用 opencode-cli 失败: %w", err)
	}
	s := strings.TrimSpace(string(out))
	if s == "" {
		return "", fmt.Errorf("opencode-cli 未返回服务地址")
	}
	return s, nil
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
