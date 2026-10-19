//go:build windows

package main

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"
)

// ---- 测试辅助 ----

func mkTodos(t *testing.T, js string) []interface{} {
	t.Helper()
	var v []interface{}
	if err := json.Unmarshal([]byte(js), &v); err != nil {
		t.Fatalf("测试 JSON 非法: %v", err)
	}
	return v
}

func decodeItems(t *testing.T, items []map[string]interface{}) []map[string]interface{} {
	t.Helper()
	// 经 JSON 往返，验证序列化后的真实形态（与协议传输一致）
	b, err := json.Marshal(items)
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]interface{}
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// ---- state.items 构建规则（§3.2） ----

func TestStateBasicFields(t *testing.T) {
	items := pipeBuildStateItems(mkTodos(t,
		`[{"id":"td_x1","content":"交周报","priority":"urgent","deadline":1792300000000,"completed":false},
		  {"id":"td_x2","content":"整理桌面","priority":"minor","deadline":null,"completed":false}]`))
	if len(items) != 2 {
		t.Fatalf("条目数 = %d，应 2", len(items))
	}
	got := decodeItems(t, items)
	if got[0]["id"] != "td_x1" || got[0]["content"] != "交周报" ||
		got[0]["priority"] != "urgent" || got[0]["deadline"] != 1792300000000.0 || got[0]["completed"] != false {
		t.Fatalf("item0 与协议逐字不一致: %v", got[0])
	}
	if got[1]["deadline"] != nil {
		t.Fatalf("无 deadline 应序列化为 null，实际 %v", got[1]["deadline"])
	}
}

func TestStatePriorityNormalize(t *testing.T) {
	items := pipeBuildStateItems(mkTodos(t,
		`[{"id":"a","content":"x","priority":"hot","deadline":null,"completed":false},
		  {"id":"b","content":"y","priority":"shelved","deadline":null,"completed":false}]`))
	if items[0]["priority"] != "normal" {
		t.Fatalf("未知值应归一 normal，实际 %v", items[0]["priority"])
	}
	if items[1]["priority"] != "shelved" {
		t.Fatalf("合法值不得改写，实际 %v", items[1]["priority"])
	}
}

func TestStateContentTruncate200Runes(t *testing.T) {
	long := strings.Repeat("鲸", 250) // 多字节 rune，验证不切坏
	items := pipeBuildStateItems(mkTodos(t,
		`[{"id":"a","content":"","priority":"normal","deadline":null,"completed":false}]`))
	_ = items
	items = pipeBuildStateItems([]interface{}{
		map[string]interface{}{"id": "a", "content": long, "priority": "normal", "deadline": nil, "completed": false},
	})
	c := items[0]["content"].(string)
	r := []rune(c)
	if len(r) != 200 {
		t.Fatalf("截断后 rune 数 = %d，应 200", len(r))
	}
	if c != strings.Repeat("鲸", 200) {
		t.Fatalf("截断内容损坏（出现替换符/切坏多字节）")
	}
}

func TestStateCap200(t *testing.T) {
	var todos []interface{}
	for i := 0; i < 201; i++ {
		todos = append(todos, map[string]interface{}{
			"id": "t" + itoa(int64(i)), "content": "c", "priority": "normal", "deadline": nil, "completed": false,
		})
	}
	items := pipeBuildStateItems(todos)
	if len(items) != 200 {
		t.Fatalf("条目数 = %d，应 200（超出取前 200）", len(items))
	}
	if items[199]["id"] != "t199" {
		t.Fatalf("取前 200：最后一条应为 t199，实际 %v", items[199]["id"])
	}
}

func TestStateDropEmptyID(t *testing.T) {
	items := pipeBuildStateItems(mkTodos(t,
		`[{"content":"无 id","priority":"normal","deadline":null,"completed":false},
		  {"id":"","content":"空 id","priority":"normal","deadline":null,"completed":false},
		  {"id":"ok","content":"y","priority":"normal","deadline":null,"completed":false}]`))
	if len(items) != 1 || items[0]["id"] != "ok" {
		t.Fatalf("id 缺失/为空应丢弃，实际 %d 条 %v", len(items), items)
	}
}

// ---- due 状态机（B3-B8） ----

const baseSec = 1792300000 // 固定"现在"（秒）

func dueOnce(t *testing.T, todosJS string, nowSec int64, notified map[string]int64, seeded bool) []pipeDue {
	t.Helper()
	return pipeDueCompute(mkTodos(t, todosJS), time.Unix(nowSec, 0), notified, seeded)
}

func TestDueBasicAndOnce(t *testing.T) {
	notified := map[string]int64{}
	js := `[{"id":"a1","content":"交周报","priority":"urgent","deadline":` +
		itoa((baseSec-1)*1000) + `,"completed":false}]`
	ev := dueOnce(t, js, baseSec, notified, true)
	if len(ev) != 1 || ev[0].ID != "a1" || ev[0].OverdueBy != 0 {
		t.Fatalf("到期应推一次 due（overdueBy<60s 记 0），实际 %v", ev)
	}
	// B4：同一 id 同一 deadline 第二个 tick 不再推
	ev = dueOnce(t, js, baseSec+5, notified, true)
	if len(ev) != 0 {
		t.Fatalf("同 id 同 deadline 不应重复推，实际 %v", ev)
	}
}

func TestDueNewDeadline(t *testing.T) {
	notified := map[string]int64{"a1": (baseSec - 60) * 1000} // 旧 deadline 已提醒
	// deadline 改为新值（未来 3 秒）→ 未到期不推
	js := `[{"id":"a1","content":"新内容","priority":"urgent","deadline":` + itoa((baseSec+3)*1000) + `,"completed":false}]`
	if ev := dueOnce(t, js, baseSec, notified, true); len(ev) != 0 {
		t.Fatalf("新 deadline 未到点不应推 due，实际 %v", ev)
	}
	// 到点 → 再推一次，内容为新内容
	ev := dueOnce(t, js, baseSec+4, notified, true)
	if len(ev) != 1 || ev[0].Content != "新内容" {
		t.Fatalf("新 deadline 到点应推一次（新内容），实际 %v", ev)
	}
	// 之后不重复
	if ev := dueOnce(t, js, baseSec+9, notified, true); len(ev) != 0 {
		t.Fatalf("B4 违反：重复推 %v", ev)
	}
}

func TestDueOverdueRoundAndFloor(t *testing.T) {
	// ≥60s 传实际毫秒
	ev := dueOnce(t, `[{"id":"a","content":"c","priority":"normal","deadline":`+itoa((baseSec-90)*1000)+`,"completed":false}]`,
		baseSec, map[string]int64{}, true)
	if len(ev) != 1 || ev[0].OverdueBy != 90000 {
		t.Fatalf("≥60s 应传实际毫秒 90000，实际 %v", ev)
	}
	// <60s 一律 0
	ev = dueOnce(t, `[{"id":"b","content":"c","priority":"normal","deadline":`+itoa((baseSec-30)*1000)+`,"completed":false}]`,
		baseSec, map[string]int64{}, true)
	if len(ev) != 1 || ev[0].OverdueBy != 0 {
		t.Fatalf("<60s 应传 0，实际 %v", ev)
	}
}

func TestDueShelvedAndCompletedSkip(t *testing.T) {
	js := `[{"id":"a","content":"x","priority":"shelved","deadline":` + itoa((baseSec-30)*1000) + `,"completed":false},
	        {"id":"b","content":"y","priority":"urgent","deadline":` + itoa((baseSec-30)*1000) + `,"completed":true}]`
	if ev := dueOnce(t, js, baseSec, map[string]int64{}, true); len(ev) != 0 {
		t.Fatalf("shelved / completed 均不产生 due，实际 %v", ev)
	}
}

func TestSeedB6(t *testing.T) {
	// 首次快照：超期 5 分钟（≤10min）→ 补推一次
	notified := map[string]int64{}
	js := `[{"id":"a","content":"x","priority":"normal","deadline":` + itoa((baseSec-300)*1000) + `,"completed":false}]`
	if ev := dueOnce(t, js, baseSec, notified, false); len(ev) != 1 {
		t.Fatalf("种子应补推 ≤10min 的超期项，实际 %v", ev)
	}
	if notified["a"] != (baseSec-300)*1000 {
		t.Fatalf("种子应标记 notified，实际 %v", notified)
	}
	// 超期 20 分钟（>10min）→ 不补推，仅标记
	notified2 := map[string]int64{}
	js2 := `[{"id":"b","content":"y","priority":"normal","deadline":` + itoa((baseSec-1200)*1000) + `,"completed":false}]`
	if ev := dueOnce(t, js2, baseSec, notified2, false); len(ev) != 0 {
		t.Fatalf(">10min 积压不应补推，实际 %v", ev)
	}
	if notified2["b"] != (baseSec-1200)*1000 {
		t.Fatalf(">10min 积压应被标记为已提醒，实际 %v", notified2)
	}
}

func TestNotifiedCleanupB8(t *testing.T) {
	notified := map[string]int64{"gone": (baseSec - 30) * 1000, "stay": (baseSec - 30) * 1000}
	js := `[{"id":"stay","content":"c","priority":"normal","deadline":null,"completed":false}]`
	_ = dueOnce(t, js, baseSec, notified, true)
	if _, ok := notified["gone"]; ok {
		t.Fatalf("已删除项的 notified 未清理（B8）")
	}
	if _, ok := notified["stay"]; ok {
		t.Fatalf("deadline 变 null 的项 notified 未清理（B8）")
	}
}

// ---- 工具 ----

func itoa(ms int64) string { return strconv.FormatInt(ms, 10) }
