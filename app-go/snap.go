//go:build windows

// snap.go —— 边缘吸附 / 缩入的纯几何逻辑（app/src/main/snap.js 的逐行翻译）。
//
// 规则（来自用户要求）：
//  1. 仅"上 / 左 / 右"三面可吸附，底面不吸附；
//  2. 跨多显示器适配（各显示器 workArea）；
//  3. 多屏相交的侧边（两屏接缝）不吸附。
//
// 坐标单位：物理像素（Electron 版为 DIP，比例规则完全一致）。
package main

import "math"

var snapSides = []string{"top", "left", "right"}

// cornerSideGapDIP（修复批）：角落归上时横向外推的固定空隙（物理=DIP×dpi）。
// 取值 > snapThreshold(26)：保证外推后窗口左/右缘回到 workArea 内侧 32px，
// 下一次判定不再满足"越界"，不会出现侧/上吸附抖动。数值可调。
const cornerSideGapDIP = 32.0

const (
	sharedTol       = 6.0  // 判定"两屏相接"的容差（像素）
	sharedMinOverlap = 60.0 // 最小重叠长度
)

// isSharedEdge：该显示器的某条边是否为"多屏相交侧边"。
// 情形一：另一屏与本屏在该边处紧邻（接缝）；情形二：另一屏跨越了本屏的该边。
func isSharedEdge(self Display, others []Display, side string) bool {
	b := self.Bounds
	for _, o := range others {
		if o.ID == self.ID {
			continue
		}
		ob := o.Bounds
		vOverlap := math.Min(float64(b.bottom), float64(ob.bottom)) - math.Max(float64(b.top), float64(ob.top))
		hOverlap := math.Min(float64(b.right), float64(ob.right)) - math.Max(float64(b.left), float64(ob.left))
		switch side {
		case "left":
			touching := math.Abs(float64(ob.right-b.left)) <= sharedTol
			crossing := float64(ob.left) < float64(b.left)-sharedTol && float64(ob.right) > float64(b.left)+sharedTol
			if (touching || crossing) && vOverlap >= sharedMinOverlap {
				return true
			}
		case "right":
			touching := math.Abs(float64(ob.left)-float64(b.right)) <= sharedTol
			crossing := float64(ob.left) < float64(b.right)-sharedTol && float64(ob.right) > float64(b.right)+sharedTol
			if (touching || crossing) && vOverlap >= sharedMinOverlap {
				return true
			}
		case "top":
			touching := math.Abs(float64(ob.bottom)-float64(b.top)) <= sharedTol
			crossing := float64(ob.top) < float64(b.top)-sharedTol && float64(ob.bottom) > float64(b.top)+sharedTol
			if (touching || crossing) && hOverlap >= sharedMinOverlap {
				return true
			}
		}
	}
	return false
}

// snappableSides：该显示器上真正可吸附的边集合
func snappableSides(self Display, all []Display) []string {
	var others []Display
	for _, d := range all {
		if d.ID != self.ID {
			others = append(others, d)
		}
	}
	var out []string
	for _, s := range snapSides {
		if !isSharedEdge(self, others, s) {
			out = append(out, s)
		}
	}
	return out
}

func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// displayForBounds：窗口"落在"哪个显示器上（窗口中心所在屏，否则取距离最近）
func displayForBounds(b rectI, displays []Display) *Display {
	if len(displays) == 0 {
		return nil
	}
	cx := float64(b.left+b.right) / 2
	cy := float64(b.top+b.bottom) / 2
	for i := range displays {
		d := &displays[i]
		db := d.Bounds
		if cx >= float64(db.left) && cx < float64(db.right) && cy >= float64(db.top) && cy < float64(db.bottom) {
			return d
		}
	}
	var best *Display
	bestDist := math.Inf(1)
	for i := range displays {
		d := &displays[i]
		db := d.Bounds
		dx := math.Max(math.Max(float64(db.left)-cx, 0), cx-float64(db.right))
		dy := math.Max(math.Max(float64(db.top)-cy, 0), cy-float64(db.bottom))
		dist := dx*dx + dy*dy
		if dist < bestDist {
			bestDist = dist
			best = d
		}
	}
	return best
}

type snapTarget struct {
	side      string
	dist      float64
	displayID uintptr
	bounds    rectI
	// overrideBounds（修复批）：角落归上时 true —— bounds 为 computeSnap 的权威贴边位
	// （含横向外推 gap）；snapNow 应直接采用而不是 expandedBoundsFor 重算（否则 gap 丢失）。
	overrideBounds bool
}

// computeSnap：窗口停在可吸附边附近时返回吸附目标，否则返回 nil
//
// 修复批（用户 2026-10-13 裁决）：
//  1. 越界即触发 —— 三边按**有向距离**判定（d = 离贴边位还差多少，内侧为正、越界为负），
//     d <= threshold 即候选（越界任意深度都触发，不再要求"小心贴边"）。
//     符号约定：右边的"越界" = 窗口左缘超过 wa.right - w（此时 d 为负）。
//  2. 角落归上 —— 侧边候选与上边候选**同时越界（d<0）**且两边皆可吸附 → 最终上吸附：
//     横向先侧边对齐、再外推 cornerSideGapDIP；一次动画到最终位。
//  3. 其余组合（单边越界、贴边、双边仅"接近"未越界）维持原最近距离逻辑。
func computeSnap(win rectI, displays []Display, threshold int32) *snapTarget {
	disp := displayForBounds(win, displays)
	if disp == nil {
		return nil
	}
	wa := disp.WorkArea
	sides := snappableSides(*disp, displays)

	var best *snapTarget
	consider := func(c *snapTarget) {
		if best == nil || c.dist < best.dist {
			best = c
		}
	}

	b := win
	clampedTop := clampI(b.top, wa.top, wa.bottom-b.h())
	leftCand, rightCand, topCand := false, false, false
	var dLeft, dRight, dTop float64
	var leftBounds, rightBounds, topBounds rectI

	if containsStr(sides, "left") {
		d := float64(b.left - wa.left) // 有向：内侧为正，越界为负
		leftCand = d <= float64(threshold)
		if leftCand {
			dLeft = d
			leftBounds = rectI{left: wa.left, top: clampedTop, right: wa.left + b.w(), bottom: clampedTop + b.h()}
		}
	}
	if containsStr(sides, "right") {
		tx := wa.right - b.w()
		d := float64(tx - b.left) // 有向（勘误 2026-10-13）：内侧（左缘 < tx）为正、越过贴位（左缘 > tx）为负——须与左侧同构
		rightCand = d <= float64(threshold)
		if rightCand {
			dRight = d
			rightBounds = rectI{left: tx, top: clampedTop, right: tx + b.w(), bottom: clampedTop + b.h()}
		}
	}
	if containsStr(sides, "top") {
		d := float64(b.top - wa.top) // 有向：内侧为正，越界为负
		topCand = d <= float64(threshold)
		if topCand {
			dTop = d
			x := clampI(b.left, wa.left, wa.right-b.w())
			topBounds = rectI{left: x, top: wa.top, right: x + b.w(), bottom: wa.top + b.h()}
		}
	}

	// 角落归上：侧边越界（d<0）+ 上边越界（d<0）且两边均可吸附 → 上吸附 + 横向外推
	// （cornerSideGapDIP 为 DIP 值；此函数是纯几何层，由调用方保证 dipToPhys 已初始化——
	//   computeSnap 现网仅在 snapNow/dockSelftest 场景调用，彼时 dpiScale 已就绪）
	gap := float64(int32(math.Round(cornerSideGapDIP * dpiScale)))
	if topCand && dTop < 0 && ((leftCand && dLeft < 0) || (rightCand && dRight < 0)) {
		x := wa.left + int32(math.Round(gap))
		if rightCand && dRight < 0 && !(leftCand && dLeft < 0) {
			x = wa.right - b.w() - int32(math.Round(gap))
		}
		return &snapTarget{side: "top", dist: dTop, displayID: disp.ID,
			bounds: rectI{left: x, top: wa.top, right: x + b.w(), bottom: wa.top + b.h()}, overrideBounds: true}
	}

	if leftCand {
		consider(&snapTarget{side: "left", dist: math.Abs(dLeft), displayID: disp.ID, bounds: leftBounds})
	}
	if rightCand {
		consider(&snapTarget{side: "right", dist: math.Abs(dRight), displayID: disp.ID, bounds: rightBounds})
	}
	if topCand {
		consider(&snapTarget{side: "top", dist: math.Abs(dTop), displayID: disp.ID, bounds: topBounds})
	}
	return best
}

// retractedBounds：缩入侧边后的窗口位置，只留 strip 像素在屏内
//   left  → 向右露出窗口右缘；right → 向左露出窗口左缘；top → 向下露出窗口下缘
func retractedBounds(side string, snapped rectI, strip int32) rectI {
	w, h := snapped.w(), snapped.h()
	switch side {
	case "left":
		return rectI{left: snapped.left - w + strip, top: snapped.top, right: snapped.left - w + strip + w, bottom: snapped.top + h}
	case "right":
		return rectI{left: snapped.right - strip, top: snapped.top, right: snapped.right - strip + w, bottom: snapped.top + h}
	case "top":
		return rectI{left: snapped.left, top: snapped.top - h + strip, right: snapped.left + w, bottom: snapped.top - h + strip + h}
	}
	return snapped
}

// handleSideOf：把手位于窗口的哪一侧（缩入方向的反方向）
func handleSideOf(side string) string {
	switch side {
	case "left":
		return "right"
	case "right":
		return "left"
	case "top":
		return "bottom"
	}
	return ""
}

// validateDock：显示器变化后校验原吸附位置是否仍合法
type dockCheck struct {
	valid   bool
	reason  string
	display *Display
}

func validateDock(side string, displayID uintptr, displays []Display) dockCheck {
	for i := range displays {
		if displays[i].ID == displayID {
			d := &displays[i]
			if !containsStr(snappableSides(*d, displays), side) {
				return dockCheck{valid: false, reason: "edge-now-shared", display: d}
			}
			return dockCheck{valid: true, display: d}
		}
	}
	return dockCheck{valid: false, reason: "display-removed"}
}
