//go:build windows

// store_test.go —— 持久化护栏单测（对应 app/test/run-tests.js 的持久化组）
package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestStore(t *testing.T, name string) *Store {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	return NewStore(dir, "")
}

func TestSaveLoadRoundtrip(t *testing.T) {
	s := newTestStore(t, "a")
	ok := s.Save(map[string]interface{}{
		"todos":    []interface{}{map[string]interface{}{"id": "x", "content": "测试"}},
		"settings": map[string]interface{}{"width": 400},
	})
	if !ok {
		t.Fatal("Save 应成功")
	}
	back, _ := s.Load()
	todos := back["todos"].([]interface{})
	if len(todos) != 1 {
		t.Fatalf("应读回 1 条: %v", todos)
	}
	first := todos[0].(map[string]interface{})
	if first["content"] != "测试" {
		t.Fatalf("内容不符: %v", first)
	}
}

// A2：写入必须是「临时文件 + rename」——落盘后不得残留 .tmp
func TestAtomicWriteNoResidue(t *testing.T) {
	s := newTestStore(t, "b")
	s.Save(map[string]interface{}{"todos": []interface{}{}, "settings": map[string]interface{}{}})
	if _, err := os.Stat(s.tmpFile); !os.IsNotExist(err) {
		t.Fatalf("临时文件应已被 rename 消费: %v", err)
	}
	if _, err := os.Stat(s.dataFile); err != nil {
		t.Fatalf("数据文件应存在: %v", err)
	}
}

// A3：窗口几何独立 window.json，与 todos.json 互不覆盖
func TestWindowFileIndependent(t *testing.T) {
	s := newTestStore(t, "c")
	s.Save(map[string]interface{}{"todos": []interface{}{map[string]interface{}{"id": "x", "content": "A"}}, "settings": map[string]interface{}{}})
	s.SaveWindowState(map[string]interface{}{"width": 400, "height": 600, "x": 10, "y": 20})
	s.Save(map[string]interface{}{"todos": []interface{}{map[string]interface{}{"id": "x", "content": "B"}}, "settings": map[string]interface{}{}})

	ws := s.LoadWindowState()
	if ws["width"].(float64) != 400 || ws["x"].(float64) != 10 {
		t.Fatalf("window.json 被破坏: %v", ws)
	}
	back, _ := s.Load()
	first := back["todos"].([]interface{})[0].(map[string]interface{})
	if first["content"] != "B" {
		t.Fatalf("todos 内容不符: %v", first)
	}
	if !strings.HasSuffix(filepath.ToSlash(s.windowFile), "window.json") {
		t.Fatalf("窗口几何应为独立 window.json: %s", s.windowFile)
	}
}

// A4 护栏 1：只清理超龄孤儿，新文件必须保留
func TestGcOnlyOldOrphans(t *testing.T) {
	s := newTestStore(t, "d")
	src := filepath.Join(t.TempDir(), "src.png")
	_ = os.WriteFile(src, []byte{0x89, 0x50, 0x4e, 0x47}, 0o644)
	fresh, err := s.ImportImage(src)
	if err != nil {
		t.Fatal(err)
	}
	// 制造一个"超龄孤儿"：直接写一个旧时间戳文件
	old := s.mediaPath("old_1.png")
	_ = os.WriteFile(old, []byte{1}, 0o644)
	past := time.Now().Add(-8 * 24 * time.Hour)
	_ = os.Chtimes(old, past, past)

	r := s.GcImages([]string{fresh}, 7*24*time.Hour)
	if r.Removed != 1 {
		t.Fatalf("应清理 1 个超龄孤儿: %+v", r)
	}
	if _, err := os.Stat(s.mediaPath(fresh)); err != nil {
		t.Fatalf("被引用的新文件必须保留")
	}
}

// A4 护栏 2：数据损坏 → lastLoadOk=false（调用方据此跳过回收）
func TestCorruptDataSkipsGc(t *testing.T) {
	s := newTestStore(t, "e")
	_ = os.WriteFile(s.dataFile, []byte("{ 这不是合法 JSON"), 0o644)
	back, _ := s.Load()
	if s.lastLoadOk {
		t.Fatal("损坏时 lastLoadOk 应为 false")
	}
	if back["loadError"] == nil {
		t.Fatal("应带回 loadError")
	}
	// 好文件 → true
	s2 := newTestStore(t, "e2")
	s2.Save(map[string]interface{}{"todos": []interface{}{}, "settings": map[string]interface{}{}})
	s2.Load()
	if !s2.lastLoadOk {
		t.Fatal("正常文件 lastLoadOk 应为 true")
	}
}

// A5 护栏 3（2026-10-14 修复）：非 ErrNotExist 的读失败（权限拒绝 / 被占用 / IO 错误）
// 不得与"首次使用"同分支 —— 否则 lastLoadOk=true → 渲染层回收图片 → 删光用户图片。
// 触发方式：同名目录占位 —— os.ReadFile 对目录返回非 NotExist 错误（已实证，跨环境稳定）。
func TestReadErrorSkipsGc(t *testing.T) {
	s := newTestStore(t, "f")
	_ = os.RemoveAll(s.dataFile)
	_ = os.MkdirAll(s.dataFile, 0o755)
	back, ok := s.Load()
	if ok || s.lastLoadOk {
		t.Fatalf("读失败时不得视为成功: ok=%v lastLoadOk=%v", ok, s.lastLoadOk)
	}
	if back["loadError"] == nil {
		t.Fatal("应带回 loadError")
	}
	// 反向：真·首次使用（文件不存在）仍应放行回收（防矫枉过正为"永远 false"）
	s2 := newTestStore(t, "f2")
	back2, ok2 := s2.Load()
	if !ok2 || !s2.lastLoadOk {
		t.Fatalf("首次使用不应视为失败: ok=%v lastLoadOk=%v back=%v", ok2, s2.lastLoadOk, back2)
	}
}

// 便携模式：exe 同级有 data 目录时切数据根
func TestPortableMode(t *testing.T) {
	root := t.TempDir()
	_ = os.MkdirAll(filepath.Join(root, "data"), 0o755)
	s := NewStore(filepath.Join(root, "unused"), root)
	if !s.portable || s.baseDir != filepath.Join(root, "data") {
		t.Fatalf("便携模式未生效: %+v", s.Info())
	}
}
