//go:build windows

// snap_test.go —— 吸附几何纯逻辑单测（snap.js 语义的 Go 实现对照）
package main

import "testing"

func mkD(id uintptr, x, y, w, h int32, ww, wh int32) Display {
	return Display{
		ID:       id,
		Bounds:   rectI{x, y, x + w, y + h},
		WorkArea: rectI{x, y, x + ww, y + wh},
	}
}

// 与 app/test/run-tests.js 的多屏吸附组逐条对应（坐标换算成物理像素语义）
func TestSideBySideSeam(t *testing.T) {
	d1 := mkD(1, 0, 0, 1920, 1080, 1920, 1040)
	d2 := mkD(2, 1920, 0, 1920, 1080, 1920, 1040)
	all := []Display{d1, d2}

	s1 := snappableSides(d1, all)
	if has(s1, "right") {
		t.Fatalf("左屏不应吸附右边（接缝）: %v", s1)
	}
	if !has(s1, "top") || !has(s1, "left") {
		t.Fatalf("左屏应可吸附 top/left: %v", s1)
	}
	s2 := snappableSides(d2, all)
	if has(s2, "left") {
		t.Fatalf("右屏不应吸附左边（接缝）: %v", s2)
	}
	if !has(s2, "top") || !has(s2, "right") {
		t.Fatalf("右屏应可吸附 top/right: %v", s2)
	}
}

func TestStackedSeam(t *testing.T) {
	top := mkD(1, 0, 0, 1920, 1080, 1920, 1080)
	bottom := mkD(2, 0, 1080, 1920, 1080, 1920, 1080)
	all := []Display{top, bottom}

	if has(snappableSides(bottom, all), "top") {
		t.Fatalf("下屏不应吸附上边（接缝）")
	}
	if !sameSet(snappableSides(bottom, all), []string{"top", "left", "right"}) &&
		!has(snappableSides(bottom, all), "left") {
		t.Fatalf("下屏应可吸附左右: %v", snappableSides(bottom, all))
	}
	if !sameSet(snappableSides(top, all), []string{"top", "left", "right"}) {
		t.Fatalf("上屏三面可吸附: %v", snappableSides(top, all))
	}
}

func TestThreeRowMiddle(t *testing.T) {
	m1 := mkD(1, 0, 0, 1920, 1080, 1920, 1080)
	m2 := mkD(2, 1920, 0, 1920, 1080, 1920, 1080)
	m3 := mkD(3, 3840, 0, 1920, 1080, 1920, 1080)
	all := []Display{m1, m2, m3}
	got := snappableSides(m2, all)
	if !sameSet(got, []string{"top"}) {
		t.Fatalf("中间屏只能吸附上边: %v", got)
	}
}

func TestBottomNeverSnaps(t *testing.T) {
	if sameSet(snapSides, []string{"top", "left", "right"}) == false {
		t.Fatalf("SNAP_SIDES 必须恰为 top/left/right: %v", snapSides)
	}
}

func TestComputeSnapSeamReject(t *testing.T) {
	d1 := mkD(1, 0, 0, 1920, 1080, 1920, 1040)
	d2 := mkD(2, 1920, 0, 1920, 1080, 1920, 1040)
	all := []Display{d1, d2}
	// 窗口贴到左屏右缘（接缝）→ 不得吸附
	win := rectI{left: 1920 - 475 - 4, top: 200, right: 1920 - 4, bottom: 200 + 700}
	if hit := computeSnap(win, all, 32); hit != nil && hit.side == "right" {
		t.Fatalf("不应吸附到右侧接缝: %+v", hit)
	}
	// 贴左屏左缘 → 吸附 left 且 x=0
	win2 := rectI{left: 7, top: 200, right: 7 + 475, bottom: 200 + 700}
	hit := computeSnap(win2, all, 32)
	if hit == nil || hit.side != "left" {
		t.Fatalf("应吸附 left: %+v", hit)
	}
	if hit.bounds.left != 0 {
		t.Fatalf("吸附目标 x 应为 0: %+v", hit.bounds)
	}
}

func TestRetractedBoundsStrip(t *testing.T) {
	expanded := rectI{left: 0, top: 40, right: 0 + 475, bottom: 40 + 700}
	r := retractedBounds("left", expanded, 13)
	if r.left != -(475 - 13) {
		t.Fatalf("left 缩入 x: %d", r.left)
	}
	if r.right != 13 {
		t.Fatalf("left 缩入后屏内可见宽度应=strip: %d", r.right)
	}
	r2 := retractedBounds("top", rectI{left: 500, top: 0, right: 500 + 475, bottom: 700}, 13)
	if r2.bottom != 13 {
		t.Fatalf("top 缩入后屏内可见高度应=strip: %d", r2.bottom)
	}
}

func TestHandleSide(t *testing.T) {
	if handleSideOf("left") != "right" || handleSideOf("right") != "left" || handleSideOf("top") != "bottom" {
		t.Fatalf("把手方向错误")
	}
}

func TestValidateDock(t *testing.T) {
	d1 := mkD(1, 0, 0, 1920, 1080, 1920, 1040)
	d2 := mkD(2, 1920, 0, 1920, 1080, 1920, 1040)
	// 屏被移除
	if v := validateDock("left", 2, []Display{d1}); v.valid {
		t.Fatalf("屏被移除应失效")
	}
	// 接缝性质变化
	if v := validateDock("right", 1, []Display{d1}); !v.valid {
		t.Fatalf("单屏时右缘应可吸附")
	}
	if v := validateDock("right", 1, []Display{d1, d2}); v.valid || v.reason != "edge-now-shared" {
		t.Fatalf("插入新屏后右缘变接缝应失效: %+v", v)
	}
}

func has(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func sameSet(a, b []string) bool {
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
