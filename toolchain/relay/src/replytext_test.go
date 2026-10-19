package main

import (
	"os"
	"testing"
	"time"
)

// 集成测试：验证「列表轻量条目 → 按 ID 取详情」这条链路能真正取到回复正文。
// opencode 2.0.27 起列表接口不再返回 content，若不做详情补齐，ReplyText() 永远为空。
//
// 启用方式（需要本机 opencode 服务在跑）：
//
//	RELAY_IT=1 go test -run TestReplyTextFromIdleSession -v
func TestReplyTextFromIdleSession(t *testing.T) {
	if os.Getenv("RELAY_IT") == "" {
		t.Skip("集成测试：设 RELAY_IT=1 启用")
	}
	const (
		cfgPath = "D:/待办事项优先级提示窗/toolchain/relay/relay.json"
		sid     = "ses_ecceafc2fffeexfhywD33fg3gp" // GLM 会话：含 assistant 历史，验证取正文
	)
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	oc, err := newOClient(cfg)
	if err != nil {
		t.Fatalf("newOClient: %v", err)
	}

	// since=0：把该会话所有 assistant 消息都当候选回复；空闲会话会立即返回
	res, err := oc.WaitDone(sid, 0, 15*time.Second)
	if err != nil {
		t.Fatalf("WaitDone: %v", err)
	}
	txt := res.ReplyText()
	t.Logf("poll=%d replies=%d idleAt=%d 正文长度=%d", res.PollCount, len(res.Replies), res.IdleAt, len(txt))
	if txt == "" {
		t.Fatalf("❌ 取不到回复正文（详情补齐未生效）：replies=%d", len(res.Replies))
	}
	if len(txt) < 120 {
		t.Logf("提示：正文偏短（%d 字节），请人工确认内容", len(txt))
	}
	head := txt
	if len(head) > 100 {
		head = head[:100]
	}
	t.Logf("✅ 正文开头：%s", head)
}
