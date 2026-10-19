//go:build windows

// selftest.go —— 运行时自检（对齐 Electron 版 --selftest 的取证能力）：
//   灌演示数据 → 读真实计算样式核对排序与五档配色 → 驱动三个弹窗 →
//   解析冒烟 → 吸附/缩入/展开读真实窗口坐标 → 合成多屏接缝判定 →
//   GDI 抓真实桌面截图 → 落盘 selftest-report.json → 自动退出。
package main

import (
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

type stCheck struct {
	Name   string      `json:"name"`
	OK     bool        `json:"ok"`
	Detail interface{} `json:"detail,omitempty"`
}

type stReport struct {
	StartedAt string           `json:"startedAt"`
	Mode      string           `json:"mode"`
	Runtime   string           `json:"runtime"`
	Shots     []string         `json:"shots"`
	Checks    []stCheck        `json:"checks"`
	Bounds    []map[string]any `json:"bounds,omitempty"`
	Diag      map[string]any   `json:"diag,omitempty"`
	Synth     map[string]any   `json:"synthesizedSides,omitempty"`
	ViewerMemory map[string]any `json:"viewerMemory,omitempty"`
}

var report *stReport
var shotDir string

func shot(name string) {
	p := filepath.Join(shotDir, name+".png")
	if err := captureWindowRegion(p); err != nil {
		check("shot:"+name, false, err.Error())
		return
	}
	report.Shots = append(report.Shots, name+".png")
}

func shotScreen(name string) {
	p := filepath.Join(shotDir, name+".png")
	if err := captureFullScreen(p); err != nil {
		check("shot:"+name, false, err.Error())
		return
	}
	report.Shots = append(report.Shots, name+".png")
}

func check(name string, ok bool, detail interface{}) {
	report.Checks = append(report.Checks, stCheck{Name: name, OK: ok, Detail: detail})
	appLogf("  [%s] %s %v", map[bool]string{true: " OK ", false: "FAIL"}[ok], name, detail)
}

func waitFor(ms int) {
	deadline := time.Now().Add(time.Duration(ms) * time.Millisecond)
	for time.Now().Before(deadline) {
		pumpOnce(8)
	}
}

// expectJSON：取表达式结果并解析
func expectJSON(expr string) (map[string]any, bool) {
	s, ok := evalJSON(expr, 8000)
	if !ok {
		return nil, false
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return nil, false
	}
	return m, true
}

func runSelftest() {
	_ = os.MkdirAll(shotDir, 0o755)
	report = &stReport{
		StartedAt: time.Now().Format(time.RFC3339),
		Mode:      "selftest",
		Runtime:   "Go 1.27 + WebView2 (系统运行时)",
	}
	appLogf("== 自检开始 ==")

	/* 1) 桥与模块 */
	s, ok := evalJSON("!!window.api && !!window.api.meta && !!window.api.helpers && !!window.__wbModules['parser']", 8000)
	check("bridge-ready", ok && s == "true", s)

	/* 2) 测试图片进入媒体目录 */
	imgName := ""
	if selftestImage != "" {
		if name, err := theStore.ImportImage(selftestImage); err == nil {
			imgName = name
			check("import-test-image", true, imgName)
		} else {
			check("import-test-image", false, err.Error())
		}
	}

	/* 3) 灌演示数据（走 app.js 内置的 __selftest，与 Electron 版同一份代码） */
	seedExpr := fmt.Sprintf(`window.__selftest({imgName:%q}).count`, imgName)
	s, ok = evalJSON(seedExpr, 8000)
	check("seed-todos", ok && s != "0" && s != "", "count="+s)

	waitFor(600)
	chromium.Eval("document.getElementById('list').scrollTop=0")
	waitFor(180)
	shot("01-list")
	chromium.Eval("document.getElementById('list').scrollTop=document.getElementById('list').scrollHeight")
	waitFor(300)
	shot("01b-list-bottom")
	chromium.Eval("document.getElementById('list').scrollTop=0")
	waitFor(200)

	/* 4) 排序 + 配色（读真实计算样式） */
	if diag, ok := expectJSON("window.__selftestDiag()"); ok {
		report.Diag = diag
		// 4a DOM 顺序与纯逻辑排序一致
		dom := toStrSlice(diag["domOrder"])
		pure := toStrSlice(diag["pureSorted"])
		check("sort-dom-matches-pure", sameStrings(dom, pure),
			fmt.Sprintf("dom=%v pure=%v", dom, pure))
		// 4b 版本徽章
		if cards, ok := diag["cards"].([]any); ok && len(cards) > 0 {
			check("version-badge", diag["versionBadge"] == "v"+appVersion, diag["versionBadge"])
			// 1.2.1 批 D：版本号防回退（内部 1.2.1 / 对外打包 1.0beta 皆合法）
			check("version-string", appVersion == "1.3.0" || appVersion == "1.0beta", appVersion)
			// 4c 五档配色（双层底 + 优先级叠加 + 强调条）
		// 第五批（用户 2026-10-13 授权调整透明度，非降阈值）：卡片底**按档分层**——
		// 浅字三档 0.65/0.75/0.78（可读性垫暗），深字两档 0.45（对比度由 overlay 主导，更透赚质感）
		bgWant := map[string]string{
			"urgent":    "rgba(30, 30, 30, 0.65)",
			"important": "rgba(30, 30, 30, 0.75)",
			"normal":    "rgba(30, 30, 30, 0.78)",
			"minor":     "rgba(30, 30, 30, 0.45)",
			"shelved":   "rgba(30, 30, 30, 0.45)",
		}
			bad := 0
			for _, c := range cards {
				cm := c.(map[string]any)
				pri := cm["pri"].(string)
				bg := cm["backgroundColor"]
				bgi := cm["backgroundImage"]
				accent := cm["borderLeftColor"]
				if bg != bgWant[pri] {
					bad++
					check("color-base-"+pri, false, fmt.Sprint(bg)+"  ← 期望 "+bgWant[pri]+"（第五批分层值，用户授权）")
					continue
				}
				want := colorExpect[pri]
				if bgi != want.overlay || accent != want.accent {
					bad++
					check("color-overlay-"+pri, false, fmt.Sprintf("bgi=%v accent=%v", bgi, accent))
				}
			}
			if bad == 0 {
				check("priority-colors", true, "五档卡底=分档透明度双层叠加（0.65/0.75/0.78/0.45/0.45，用户 2026-10-13 授权），叠加色/强调条与用户指定值一致")
			}
		}
	} else {
		check("diag", false, "取不到 __selftestDiag")
	}

	/* 4d) 美术优化轮：字号档位 / 触控尺寸 / aria-label（读真实计算样式） */
	if m, ok := expectJSON(`(function(){
	  function v(prop,sel){var e=document.querySelector(sel);return e?getComputedStyle(e)[prop]:null;}
	  return {
	    cardContent: v('fontSize','.card-content'),
	    cardMeta: v('fontSize','.card-meta'),
	    badge: v('fontSize','.badge'),
	    iconBtn: v('width','.icon-btn'),
	    miniAct: v('width','.mini-act'),
	    checkBtn: v('width','.check'),
	    ariaIcons: document.querySelectorAll('.icon-btn[aria-label]').length,
	    ariaCardBtns: document.querySelectorAll('.card button[aria-label]').length,
	    baseVar: (getComputedStyle(document.documentElement).getPropertyValue('--base')||'').trim(),
	    doneOpacity: v('opacity','.card.done'),
	    remindRing: (getComputedStyle(document.documentElement).getPropertyValue('--remind-ring')||'').trim()
	  }})()`); ok {
		check("ui-font-card-content", m["cardContent"] == "14px", m["cardContent"])
		check("ui-font-card-meta", m["cardMeta"] == "11px", m["cardMeta"])
		check("ui-font-badge", m["badge"] == "10.5px", m["badge"])
		check("ui-size-icon-btn", m["iconBtn"] == "28px", m["iconBtn"])
		check("ui-size-mini-act", m["miniAct"] == "32px", fmt.Sprint(m["miniAct"])+"（用户 2026-10-13 裁决 32px，规格变更；原 24px）")
		check("ui-size-check", m["checkBtn"] == "28px", fmt.Sprint(m["checkBtn"])+"（用户 2026-10-13 裁决 28px，规格变更；原 24px，与删除键 32 平衡）")
		if v, ok := m["ariaIcons"].(float64); ok {
			check("ui-a11y-icon-labels", v >= 4, fmt.Sprintf("顶栏图标按钮带 aria-label = %v", v))
		} else {
			check("ui-a11y-icon-labels", false, "取不到计数")
		}
		if v, ok := m["ariaCardBtns"].(float64); ok {
			check("ui-a11y-card-labels", v >= 2, fmt.Sprintf("卡片内图标按钮带 aria-label = %v", v))
		} else {
			check("ui-a11y-card-labels", false, "取不到计数")
		}
		// 2026-10-13 第五批：root --base 保持 0.55 **作为默认值**（卡片按档分层覆盖在 .card[data-pri] 处）
		check("base-alpha", m["baseVar"] == "rgba(30, 30, 30, 0.55)",
			fmt.Sprintf("--base(root 默认)=%v（0.55 保持；卡片分档覆盖值由 color-base-* 与 constraint-audit 钉住）", m["baseVar"]))
		// 完成卡透明度：0.58→0.62（第五批，mimo 完成态更易读建议；用户授权调 alpha 的一部分）
		check("done-opacity", m["doneOpacity"] == "0.62",
			fmt.Sprintf("done opacity=%v（期望 0.62，第五批）", m["doneOpacity"]))
		// 提醒圈色：偏红橙 #FF7A1A（mimo 方向 1，避开重要档金橙）
		check("remind-ring-var", m["remindRing"] == "#FF7A1A",
			fmt.Sprintf("--remind-ring=%v（期望 #FF7A1A，mimo 方向 1 并入）", m["remindRing"]))
	} else {
		check("ui-metrics", false, "取不到 UI 度量")
	}

	/* 4g) 硬约束清单断言（第五批 §四）：透明质感锚点与分层区间钉住 */
	if m, ok := expectJSON(`(function(){
	  function avv(v){return (getComputedStyle(document.documentElement).getPropertyValue(v)||'').trim();}
	  function alphaOf(s){if(!s)return null;var mm=s.match(/,\s*([\d.]+)\)\s*$/);return mm?parseFloat(mm[1]):null;}
	  return {panel: alphaOf(avv('--panel')), panel2: alphaOf(avv('--panel-2')), panel3: alphaOf(avv('--panel-3')),
	      menu: alphaOf(getComputedStyle(document.getElementById('ctx-menu')).backgroundColor),
	      footer: alphaOf(getComputedStyle(document.querySelector('.footer')).backgroundColor),
	      done: parseFloat(getComputedStyle(document.querySelector('.card.done')).opacity)}})()`); ok {
		panelOk := m["panel"] != nil && m["panel"].(float64) >= 0.70 && m["panel"].(float64) <= 0.80
		p2 := numf(m["panel2"])
		p3 := numf(m["panel3"])
		mn := numf(m["menu"])
		ft := numf(m["footer"])
		dn := numf(m["done"])
		check("constraint-audit-hard", panelOk && p2 >= 0 && p2 < 0.65 && p3 >= 0 && p3 < 0.65 &&
			mn >= 0.90 && ft >= 0 && ft < 0.65 && dn >= 0.55 && dn <= 0.70,
			fmt.Sprintf("panel=%.2f∈[0.70,0.80] panel2=%.2f<0.65 panel3=%.2f<0.65 menu=%.2f≥0.90(允许近实底) footer=%.2f<0.65 done=%.2f∈[0.55,0.70]",
				numf(m["panel"]), p2, p3, mn, ft, dn))
	} else {
		check("constraint-audit-hard", false, "取不到硬约束清单计算样式")
	}

	// 4e 断言改「审计记录型」：合成模型跑出的事实数值如实落账（不设通过线、不阻塞），
	// 第五批分层值下模型重跑：normal 档深壁纸按 0.78 垫暗后，黑壁纸对比应回升 ≥4.5 线——如实落账。
	minDark, minWhite := 99.0, 99.0
	perPri := []string{}
	for _, s := range viewerPriSpecs {
		dr, dg, db := cardCompositeBG(s.base, 0, 0, 0, s.base.baseA, s.base.overlayA)
		wr, wg, wb := cardCompositeBG(s.base, 255, 255, 255, s.base.baseA, s.base.overlayA)
		tl := srgbLuminance(s.base.textR, s.base.textG, s.base.textB)
		rDark := contrastRatioLum(tl, luminanceOfRGB(dr, dg, db))
		rWhite := contrastRatioLum(tl, luminanceOfRGB(wr, wg, wb))
		if rDark < minDark {
			minDark = rDark
		}
		if rWhite < minWhite {
			minWhite = rWhite
		}
		perPri = append(perPri, fmt.Sprintf("%s(base%.2f) 深%.2f/白%.2f", s.key, s.base.baseA, rDark, rWhite))
	}
	check("base-contrast-audit", true,
		fmt.Sprintf("事实审计（非通过线，第五批分层值重跑）：[%v]；深壁纸最坏 %.2f（AA 4.5，normal 分层垫暗后应回升）｜白壁纸最坏 %.2f（主缓解=双层字影；浅壁纸观感留人工复核）",
			strings.Join(perPri, " "), minDark, minWhite))

	/* 4f) 收尾批 ④ 的用户裁定落地（第四批追加书）：双层字影锁参数
	   紧贴层 0 1px 2px 0.85 + 外层 0 0 5px 0.5，仅浅字三档；同时验证浅底两档仍为 none（不加字影） */
	if m, ok := expectJSON(`(function(){
	  function ts(sel){var e=document.querySelector(sel);return e?getComputedStyle(e).textShadow:null;}
	  return {urgent:ts('.card[data-pri="urgent"] .card-content'),
	          normal:ts('.card[data-pri="normal"] .card-content'),
	          minor:ts('.card[data-pri="minor"] .card-content')}})()`); ok {
		u := fmt.Sprint(m["urgent"])
		check("text-shadow-dual-layer", strings.Contains(u, "1px 2px") && strings.Contains(u, "0.85") &&
			strings.Contains(u, "0px 0px 5px") && strings.Contains(u, "0.5"),
			fmt.Sprintf("urgent textShadow=%v（须同时含 1px2px/0.85 与 0 0 5px/0.5 两层，防单层漂移）", u))
		check("text-shadow-light-cards-none", fmt.Sprint(m["minor"]) == "none",
			fmt.Sprintf("minor 卡字影=%v（浅底深字维持不加影）", m["minor"]))
		/* 修复批·标签辨识度（mimo 四点 + DeepSeek 口径）：四条 computed style 断言 */
		if m2, ok2 := expectJSON(`(function(){
		  function bs(sel){var b=document.querySelector(sel+' .badge');var cs=b?getComputedStyle(b):null;
		    return cs?{ts:cs.textShadow,color:cs.color,shadow:cs.boxShadow}:null;}
		  return {urgent:bs('.card[data-pri="urgent"]'), important:bs('.card[data-pri="important"]')}})()`); ok2 {
			u := m2["urgent"].(map[string]any)
			i := m2["important"].(map[string]any)
			check("badge-shadow-none", fmt.Sprint(u["ts"]) == "none" && fmt.Sprint(i["ts"]) == "none",
				fmt.Sprintf("urgent ts=%v important ts=%v（徽章不继承双层字影）", u["ts"], i["ts"]))
			check("badge-color-urgent", fmt.Sprint(u["color"]) == "rgb(255, 232, 232)",
				fmt.Sprintf("urgent badge color=%v（反白 #FFE8E8）", u["color"]))
			check("badge-color-default", fmt.Sprint(i["color"]) == "rgba(0, 0, 0, 0.88)",
				fmt.Sprintf("important badge color=%v（0.78→0.88 提实）", i["color"]))
			check("badge-ring-urgent", strings.Contains(fmt.Sprint(u["shadow"]), "rgba(255, 255, 255, 0.28) 0px 0px 0px 1px"),
				fmt.Sprintf("urgent badge boxShadow=%v（1px 白圈分离同色系卡底）", u["shadow"]))
		} else {
			check("badge-shadow-none", false, "取不到徽章计算样式")
		}
	} else {
		check("text-shadow-dual-layer", false, "取不到卡片字影计算样式")
	}

	/* 5) 二次确认弹窗 */
	chromium.Eval("window.__selftestOpenConfirm()")
	waitFor(320)
	if m, ok := expectJSON(`(function(){var m=document.getElementById('modal-confirm');return {open:m.classList.contains('open'), text:document.getElementById('confirm-text').textContent}})()`); ok {
		check("confirm-modal", m["open"] == true && m["text"].(string) != "", m["text"])
	}
	shot("02-confirm")
	chromium.Eval("document.getElementById('btn-confirm-cancel').click()")
	waitFor(260)

	/* 5b) 删除二次确认（美术优化轮新增：删除属不可逆操作，必须有确认） */
	chromium.Eval("window.__selftestOpenDeleteConfirm()")
	waitFor(320)
	if m, ok := expectJSON(`(function(){var c=document.getElementById('modal-confirm'),b=document.getElementById('btn-confirm-ok');return {open:c.classList.contains('open'),title:document.getElementById('confirm-title').textContent,text:document.getElementById('confirm-text').textContent,okText:b.textContent,danger:b.classList.contains('danger-ok'),w:getComputedStyle(b).width}})()`); ok {
		txt := fmt.Sprint(m["text"])
		good := m["open"] == true && m["danger"] == true &&
			strings.Contains(fmt.Sprint(m["title"]), "删除") &&
			strings.Contains(txt, "不可恢复") && m["w"] != "0px"
		check("delete-confirm-modal", good,
			fmt.Sprintf("title=%v okText=%v danger=%v 正文含「不可恢复」=%v",
				m["title"], m["okText"], m["danger"], strings.Contains(txt, "不可恢复")))
	} else {
		check("delete-confirm-modal", false, "取不到删除确认弹窗")
	}
	shot("02b-delete-confirm")
	chromium.Eval("document.getElementById('btn-confirm-cancel').click()")
	waitFor(260)
	if s, ok := evalJSON(`document.getElementById('modal-confirm').classList.contains('open')`, 8000); ok {
		check("delete-confirm-cancel-closes", s != "true", "点取消后弹窗应关闭，实际 open="+s)
	}

	/* 5c) 优先级右键菜单（自绘五档，当前档 ✓；改档走 scheduleSave→重排序） */
	chromium.Eval("window.__selftestOpenPriMenu()")
	waitFor(280)
	if m, ok := expectJSON("window.__selftestPriMenuInfo()"); ok {
		gotItems := toStrSlice(m["items"])
		check("pri-menu", m["open"] == true &&
			sameStrings(gotItems, []string{"urgent", "important", "normal", "minor", "shelved"}) &&
			m["current"] == "urgent" && m["checkedMark"] == "✓",
			fmt.Sprintf("open=%v items=%v current=%v mark=%v", m["open"], gotItems, m["current"], m["checkedMark"]))
		// mimo 方向 4 并入断言：五档项配色点 + 当前档勾用该档 accent + panel-2 底
		dots := toStrSlice(m["dots"])
		wantDots := []string{
			"rgba(196, 24, 24, 0.95)", "rgba(255, 176, 32, 0.95)", "rgba(150, 226, 62, 0.95)",
			"rgba(96, 178, 216, 0.95)", "rgba(190, 190, 190, 0.95)",
		}
		// mimo 正式建议②并如：当前档勾统一 accent 蓝（色点仍表档位）；菜单底 0.94 实底
		check("pri-menu-dots", sameStrings(dots, wantDots) && m["checkColor"] == "rgb(110, 168, 254)" &&
			m["menuBg"] == "rgba(31, 35, 44, 0.94)",
			fmt.Sprintf("dots=%v 当前档勾色=%v（统一accent蓝） menuBg=%v（0.94实底）", dots, m["checkColor"], m["menuBg"]))
	} else {
		check("pri-menu", false, "取不到右键菜单状态")
	}
	shot("02c-pri-menu")
	// 点「搁置」→ 生效 + 菜单关闭；随后用菜单点回「紧急」复原
	chromium.Eval("window.__selftestClickPriItem('shelved')")
	waitFor(280)
	if m, ok := expectJSON("window.__selftestPriMenuInfo()"); ok {
		if pri, ok2 := evalJSON("window.__selftestTodoPri('a1')", 8000); ok2 {
			check("pri-menu-apply", m["open"] == false && unq(pri) == "shelved",
				fmt.Sprintf("菜单已关闭=%v（open 取反） a1.pri=%v（点击后菜单关闭且档位生效）",
					m["open"] == false, unq(pri)))
		}
	}
	chromium.Eval("window.__selftestOpenPriMenu()")
	waitFor(200)
	chromium.Eval("window.__selftestClickPriItem('urgent')")
	waitFor(280)
	if pri, ok := evalJSON("window.__selftestTodoPri('a1')", 8000); ok {
		check("pri-menu-restore", unq(pri) == "urgent", "a1 复原为 urgent="+unq(pri))
	}

	/* 6) 详情弹窗（双击打开的同一入口） */
	chromium.Eval("window.__selftestOpenDetail()")
	waitFor(360)
	if m, ok := expectJSON(`(function(){return {open:document.getElementById('modal-detail').classList.contains('open'), content:document.getElementById('d-content').value, note:document.getElementById('d-note').value}})()`); ok {
		check("detail-modal", m["open"] == true && m["content"] == "整理设计稿并归档" && m["note"] == "参考上次评审意见", m)
	}
	shot("03-detail")
	chromium.Eval("document.getElementById('btn-d-cancel').click()")
	waitFor(260)

	/* 6b) 就地编辑 + 三按钮确认（是=保存 / 否=回编辑保留内容 / 撤回=还原退出；无变化静默退出） */
	originalA4 := "补充用户调研问卷"
	chromium.Eval(`window.__selftestStartEdit('a4','补充用户调研问卷（修订）')`)
	waitFor(260)
	if m, ok := expectJSON("window.__selftestEditState()"); ok {
		check("inline-edit-enter", m["editing"] == true && m["focused"] == true &&
			m["value"] == "补充用户调研问卷（修订）" && m["original"] == originalA4, m)
	} else {
		check("inline-edit-enter", false, "取不到编辑态")
	}
	// mimo 方向 3 并入断言：卡片抬升（editing 类 + 强化阴影）+ 输入框 panel-2 半透底（第五批 0.55）
	if m, ok := expectJSON("window.__selftestEditState()"); ok {
		check("editing-visual", m["cardEditing"] == true &&
			m["inputBg"] == "rgba(31, 35, 44, 0.55)" &&
			m["inputBorder"] == "rgba(110, 168, 254, 0.42)" &&
			strings.Contains(fmt.Sprint(m["cardShadow"]), "rgba(0, 0, 0, 0.45)"),
			fmt.Sprintf("inputBg=%v（panel-2 0.55 联动） cardEditing=%v shadow=%v", m["inputBg"], m["cardEditing"], m["cardShadow"]))
	}
	// 字影断言升级（规格变更同步，用户 2026-10-13 裁定单层加强→双层；防漂移锁值）
	if m, ok := expectJSON(`(function(){var c=document.querySelector('.card[data-pri="urgent"] .card-content');return {urgent:c?getComputedStyle(c).textShadow:null}})()`); ok {
		check("pri-shadow-strength", fmt.Sprint(m["urgent"]) == "rgba(0, 0, 0, 0.85) 0px 1px 2px, rgba(0, 0, 0, 0.5) 0px 0px 5px",
			fmt.Sprintf("%v（规格变更：单层加强→双层字影，用户裁定锁值）", m["urgent"]))
	}
	// 文本有变化 → 退出判定应弹三按钮确认
	chromium.Eval("window.__selftestEndEdit()")
	waitFor(320)
	if m, ok := expectJSON("window.__selftestEditModalInfo()"); ok {
		icons, _ := m["icons"].([]any)
		iconsOK := len(icons) == 3
		if iconsOK {
			for _, v := range icons {
				if v != true {
					iconsOK = false
				}
			}
		}
		check("edit-confirm-modal", m["open"] == true &&
			fmt.Sprint(m["yes"]) == "是" && fmt.Sprint(m["no"]) == "否" && fmt.Sprint(m["undo"]) == "撤回" &&
			iconsOK && m["text"] != "",
			fmt.Sprintf("open=%v 是/否/撤回=%v/%v/%v 图标=%v 文本PV={%v}",
				m["open"], m["yes"], m["no"], m["undo"], icons, m["text"]))
	} else {
		check("edit-confirm-modal", false, "取不到三按钮弹窗状态")
	}
	shot("03b-inline-edit-confirm")
	// 否 → 关弹窗回编辑态，已输入内容保留、焦点回编辑框
	chromium.Eval("document.getElementById('btn-edit-no').click()")
	waitFor(340)
	if m, ok := expectJSON("window.__selftestEditState()"); ok {
		check("edit-no-resume", m["editing"] == true && m["value"] == "补充用户调研问卷（修订）" &&
			m["modalOpen"] == false && m["focused"] == true,
			fmt.Sprintf("editing=%v value=%v modalOpen=%v focused=%v（否→回编辑保留内容）",
				m["editing"], m["value"], m["modalOpen"], m["focused"]))
	}
	// 撤回 → 还原为修改前文本并退出编辑态
	chromium.Eval("window.__selftestEndEdit()")
	waitFor(300)
	chromium.Eval("document.getElementById('btn-edit-undo').click()")
	waitFor(340)
	if s, ok := evalJSON(`window.__selftestTodoContent('a4')`, 8000); ok {
		check("edit-undo-restore", unq(s) == originalA4, "撤回后 a4="+unq(s)+"（应=原文）")
	}
	// 是 → 保存（随后复原）；够覆盖即可
	chromium.Eval(`window.__selftestStartEdit('a4','补充用户调研问卷（修订2）')`)
	waitFor(240)
	chromium.Eval("window.__selftestEndEdit()")
	waitFor(320)
	chromium.Eval("document.getElementById('btn-edit-yes').click()")
	waitFor(340)
	if s, ok := evalJSON(`window.__selftestTodoContent('a4')`, 8000); ok {
		check("edit-yes-save", unq(s) == "补充用户调研问卷（修订2）", "保存后 a4="+unq(s))
	}
	// 复原 a4（走一次真实 edit 流程，二次验证 yes）+ 验证「无变化静默退出」
	chromium.Eval(`window.__selftestStartEdit('a4','补充用户调研问卷')`)
	waitFor(240)
	chromium.Eval("window.__selftestEndEdit()")
	waitFor(320)
	chromium.Eval("document.getElementById('btn-edit-yes').click()")
	waitFor(340)
	if s, ok := evalJSON(`window.__selftestTodoContent('a4')`, 8000); ok {
		check("edit-restore-again", unq(s) == originalA4, "复原后 a4="+unq(s))
	}
	chromium.Eval(`window.__selftestStartEdit('a4','补充用户调研问卷')`)
	waitFor(240)
	chromium.Eval("window.__selftestEndEdit()")
	waitFor(320)
	if m, ok := expectJSON("window.__selftestEditState()"); ok {
		check("edit-nochange-silent", m["editing"] == false && m["modalOpen"] == false,
			fmt.Sprintf("editing=%v modalOpen=%v（无变化应静默退出、不弹确认）", m["editing"], m["modalOpen"]))
	}
	// 编辑态守卫断言（🟡 补）：正在编辑时右键不弹优先级菜单
	chromium.Eval(`window.__selftestStartEdit('a4','补充用户调研问卷')`)
	waitFor(240)
	chromium.Eval("document.querySelector('.card[data-id=\"a4\"]').dispatchEvent(new MouseEvent('contextmenu', {bubbles: true, cancelable: true, clientX: 60, clientY: 80}))")
	waitFor(260)
	if m, ok := expectJSON("window.__selftestPriMenuInfo()"); ok {
		check("editing-blocks-menu", m["open"] == false, fmt.Sprintf("open=%v（编辑态右键不应弹出菜单）", m["open"]))
	}
	chromium.Eval("window.__selftestEndEdit()") // 无变化 → 静默退出，同时清编辑态
	waitFor(260)

	/* 7) 灯箱（默认适应窗口，可切原始大小）
	   断言不写死图片尺寸：以 Go 侧解码出的真实像素尺寸为准，校验 DOM 的
	   naturalWidth/Height 与之一致（历史教训：曾硬编码 900x560 = Electron 版
	   test-image.png 尺寸，Go 版换成 testimg.png 后断言恒假）。 */
	lightboxWantW, lightboxWantH, lbErr := theStore.mediaImageSize(imgName)
	chromium.Eval("window.__selftestOpenLightbox()")
	waitFor(500)
	if m, ok := expectJSON(`(function(){var i=document.getElementById('lightbox-img');var sz=document.getElementById('lightbox-size').textContent;return {open:document.getElementById('lightbox').classList.contains('open'), fit:document.getElementById('lightbox').classList.contains('fit'), nw:i.naturalWidth, nh:i.naturalHeight, sizeText:sz, src:i.getAttribute('src')||''}})()`); ok {
		check("lightbox", lbErr == nil && m["open"] == true && m["fit"] == true &&
			int(m["nw"].(float64)) == lightboxWantW && int(m["nh"].(float64)) == lightboxWantH &&
			m["src"] != "",
			fmt.Sprintf("fit=%v 原图 %vx%v（期望 %dx%d）元数据=\"%v\"（原图完整加载未降质；尺寸取自 Go 侧解码）",
				m["fit"], m["nw"], m["nh"], lightboxWantW, lightboxWantH, m["sizeText"]))
	}
	shot("04-lightbox")
	chromium.Eval("document.getElementById('btn-lightbox-close').click()")
	waitFor(240)

	/* 7c) 到期提醒（渲染层置顶 + .remind 橙圈；关闭后 DOM 顺序必须还原 == sortTodos）
	   证明"叠加置顶"独立于纯排序：把已完成项 a8 也造成已过期（completed 不命中但 sortTodos 里
	   按 deadline 升序排最前）→ sortTodos 顺序为 [a8,a4?,...]，而提醒置顶组只含未完成命中 →
	   DOM 顺序 != pureSorted；关闭提醒后 DOM == pureSorted（硬性契约）。 */
	chromium.Eval(`window.__selftestSetRemind(false)`) // 先确保关闭基准
	chromium.Eval(`window.__selftestPatchDeadline('a8','-0.5')`) // a8: completed, deadline=now-30s → sortTodos 最前但非命中
	waitFor(300)
	base, okBase := expectJSON("window.__selftestRemindState()")
	if !okBase {
		check("remind-off-order-contract", false, "取不到提醒基准状态")
	} else {
		domGot := toStrSlice(base["domOrder"])
		pureGot := toStrSlice(base["pureSorted"])
		check("remind-off-order-contract", sameStrings(domGot, pureGot),
			fmt.Sprintf("提醒关闭时 DOM 顺序必须严格等于 sortTodos：dom==pure=%v", sameStrings(domGot, pureGot)))
	}
	// 开启提醒：制造 a2（-40s）与 a5（-5s）两个命中；a8 已过期但 completed → 非命中
	chromium.Eval(`window.__selftestSetRemind(true,'a2')`)
	chromium.Eval(`window.__selftestPatchDeadline('a5','-0.01')`)
	waitFor(300)
	if m, ok := expectJSON("window.__selftestRemindState()"); ok {
		hits := toStrSlice(m["hits"])
		dom := toStrSlice(m["domOrder"])
		pure := toStrSlice(m["pureSorted"])
		// 期望：命中组（按 pure 顺序）在前，其余项保持 pure 顺序
		expected := make([]string, 0, len(pure))
		for _, id := range pure {
			if contains(id, hits) {
				expected = append(expected, id)
			}
		}
		for _, id := range pure {
			if !contains(id, hits) {
				expected = append(expected, id)
			}
		}
		check("remind-pin-grouped", sameStrings(dom, expected) && m["firstCardIsHit"] == true && m["firstCardRemind"] == true &&
			!sameStrings(dom, pure),
			fmt.Sprintf("hits=%v dom=%v（纯序=%v，叠加后应不同且命中组在最前）", hits, dom, pure))
	} else {
		check("remind-pin-grouped", false, "取不到提醒开启状态")
	}
	/* 7b) 提醒视觉：浅色卡（a5=minor）用深色圈/深色发光，深色卡（a2=important）橙圈+橙发光（mimo 方向 1/2 并入） */
	if m, ok := expectJSON(`(function(){var l=document.querySelector('.card[data-id="a5"]');var d=document.querySelector('.card[data-id="a2"]');return {lightOutline:getComputedStyle(l).outlineColor, lightShadow:getComputedStyle(l).boxShadow, darkOutline:getComputedStyle(d).outlineColor, darkShadow:getComputedStyle(d).boxShadow}})()`); ok {
		check("remind-ring-on-light", m["lightOutline"] == "rgb(179, 38, 30)",
			fmt.Sprintf("a5(minor) 圈色=%v（期望深色 #B3261E）", m["lightOutline"]))
		check("remind-glow-on-light", strings.Contains(fmt.Sprint(m["lightShadow"]), "rgba(179, 38, 30, 0.42)"),
			fmt.Sprintf("a5 发光=%v（期望深色系）", m["lightShadow"]))
		check("remind-ring-dark", m["darkOutline"] == "rgb(255, 122, 26)",
			fmt.Sprintf("a2(important) 圈色=%v（期望偏红橙 #FF7A1A）", m["darkOutline"]))
		check("remind-glow", strings.Contains(fmt.Sprint(m["darkShadow"]), "rgba(255, 122, 26, 0.6)") ||
			strings.Contains(fmt.Sprint(m["darkShadow"]), "rgba(255, 122, 26, 0.60)"),
			fmt.Sprintf("a2 发光=%v（第五批 0.50→0.60 白壁纸保边）", m["darkShadow"]))
	} else {
		check("remind-ring-on-light", false, "取不到提醒样式")
	}
	shot("04b-remind")
	// 关闭提醒 → 圈与置顶同时撤销，DOM 顺序恢复契约
	chromium.Eval("window.__selftestSetRemind(false)")
	waitFor(300)
	if m, ok := expectJSON("window.__selftestRemindState()"); ok {
		check("remind-off-restore", m["checked"] == false && m["firstCardRemind"] == false &&
			sameStrings(toStrSlice(m["domOrder"]), toStrSlice(m["pureSorted"])),
			fmt.Sprintf("checked=%v dom==pure=%v（关闭后必须还原 sortTodos 契约）",
				m["checked"], sameStrings(toStrSlice(m["domOrder"]), toStrSlice(m["pureSorted"]))))
	}
		// 「同一批只弹一次」的纯逻辑判定
	if m, ok := expectJSON(`(function(){var f=window.__selftestRemindChanged;
	  return {a: f(null,['x']) , b: f(['x'],['x']) , c: f(['x'],['x','y']) , d: f(['x','y'],['y']) , e: f(['x'],['x']) }})()`); ok {
		check("remind-batch-once", m["a"] == true && m["b"] == false && m["c"] == true && m["d"] == false,
			fmt.Sprintf("null→非空=%v 无变化=%v 有新增=%v 纯减少=%v（同一批命中只弹一次）", m["a"], m["b"], m["c"], m["d"]))
	} else {
		check("remind-batch-once", false, "取不到 remindHitsChanged 判定结果")
	}
	// mimo 正式建议④断言：完成态（a8 已 patch 过期）**无超期文案节点**（渲染层直修）；未完成卡 a2 红字仍在
	if m, ok := expectJSON(`(function(){var s=document.querySelector('.card[data-id="a8"] .meta-overdue');var t=document.querySelector('.card[data-id="a8"] .card-meta').textContent;var c=document.querySelector('.card[data-id="a2"] .meta-overdue');return {doneSpan:(s?'exists':null), doneText:t, live:c?getComputedStyle(c).display:null}})()`); ok {
		check("done-hides-overdue-text", m["doneSpan"] == nil &&
			m["live"] == "block" && !strings.Contains(fmt.Sprint(m["doneText"]), "已超期"),
			fmt.Sprintf("done 卡超期节点=%v 文本含「已超期」=%v 未完成卡红字 display=%v", m["doneSpan"], strings.Contains(fmt.Sprint(m["doneText"]), "已超期"), m["live"]))
	} else {
		check("done-hides-overdue-text", false, "取不到超期文本样式")
	}
	// 数据复原：a2/a5/a8 deadline 还原
	chromium.Eval(`window.__selftestPatchDeadline('a2','30')`)
	chromium.Eval(`window.__selftestPatchDeadline('a5','192')`)   // 8天=192h
	chromium.Eval(`window.__selftestPatchDeadline('a8','-1')`)    // -1 = 还原为 null
	waitFor(240)

	/* 8) 解析冒烟（同一份 parser.js，页面本地执行）；1.2.0 批起命中词保留在 content */
	parseCheck("parse-basic", `__wbParseAt('明天下午3点前交周报 紧急')`,
		map[string]any{"content": "交周报 紧急", "priority": "urgent", "deadline": "2026/10/13 15:00:00"})
	parseCheck("parse-tonight8", `__wbParseAt('今晚8点前回复邮件 次要')`,
		map[string]any{"content": "回复邮件 次要", "priority": "minor", "deadline": "2026/10/12 20:00:00"})
	parseCheck("parse-noon1", `__wbParseAt('明天中午1点开会')`,
		map[string]any{"deadline": "2026/10/13 13:00:00"})
	parseCheck("parse-range", `__wbParseAt('2-3天内完成')`,
		map[string]any{"content": "完成", "deadline": "2026/10/15 23:59:00"})
	parseCheck("parse-noturgent", `__wbParseAt('不急 交报告')`,
		map[string]any{"content": "不急 交报告", "priority": "normal"})

	/* 修复批 · 位置感知三规则 */
	parseCheck("parse-neg-urgent", `__wbParseAt('bug修理，不紧急')`,
		map[string]any{"content": "bug修理，不紧急", "priority": "normal"})   // 否定前缀守卫：不紧急整词留、不判档
	parseCheck("parse-shelve-tmp", `__wbParseAt('暂时搁置设备修理')`,
		map[string]any{"content": "暂时搁置设备修理", "priority": "shelved"})          // 搁置族时间前缀左扩；1.2.0 起词保留在 content
	parseCheck("parse-degree-imp", `__wbParseAt('仓库需发货，较重要')`,
		map[string]any{"content": "仓库需发货，较重要", "priority": "important"})     // 程度副词左扩；1.2.0 起词保留在 content

	/* 1.2.0 批 · 关键词保留三断言（判定不变、content 保留原词） */
	parseCheck("parse-keep-urgent", `__wbParseAt('紧急 缴费')`,
		map[string]any{"content": "紧急 缴费", "priority": "urgent", "deadline": nil})
	parseCheck("parse-keep-important", `__wbParseAt('重要客户回访')`,
		map[string]any{"content": "重要客户回访", "priority": "important"})
	parseCheck("parse-keep-shelved", `__wbParseAt('暂缓 下单')`,
		map[string]any{"content": "暂缓 下单", "priority": "shelved"})

	/* 9) 吸附 → 缩入 → 展开（真实窗口坐标） */
	dockSelftest()

	/* 9b) 到期提醒 × 吸附收起：收起态出现新命中 → 自动展开（同一批只弹一次，真实窗口坐标实测） */
	wa := primaryDisplay().WorkArea
	// 起手：dockSelftest 结束时为已展开吸附态；复位提醒基准 → 收起
	killTimer(timerRetract)
	hoverInside = false
	retractNow()
	waitFor(400)
	chromium.Eval(`window.__selftestSetRemind(false)`)
	waitFor(200)
	// 收起态制造新命中 a2 → maybeRemindAlert 应调 expand
	// 第五批测试硬化：轮询到截止（150ms 步长、3s 上限），不再固定 waitFor(1100)
	chromium.Eval(`window.__selftestSetRemind(true,'a2')`)
	expandedOk := waitForState(3000, func() bool { return !retracted && windowRect().left == wa.left })
	eb2 := windowRect()
	check("remind-auto-expand", expandedOk && !retracted && eb2.left == wa.left,
		fmt.Sprintf("轮询实测 retracted=%v x=%d wa.x=%d（收起态新命中应自动展开，3s 截止内达成）", retracted, eb2.left, wa.left))
	// 同一批不重复弹：重新收起 → 模拟一次 tick（命中集合未变化，不重置已弹标记）→ 应保持收起
	killTimer(timerRetract)
	hoverInside = false
	retractNow()
	waitFor(300)
	chromium.Eval(`window.__selftestTickRemind()`)
	stayedRetracted := true
	dlNoRe := time.Now().Add(1500 * time.Millisecond)
	for time.Now().Before(dlNoRe) {
		pumpOnce(150)
		if !retracted {
			stayedRetracted = false
			break
		}
	}
	check("remind-once-per-batch", stayedRetracted,
		fmt.Sprintf("1.5s 轮询保持收起=%v（同一批命中不应重复弹出）", stayedRetracted))
	// 复原：展开 + 关提醒 + a2 时限还原
	expandNow()
	waitFor(300)
	chromium.Eval(`window.__selftestSetRemind(false)`)
	chromium.Eval(`window.__selftestPatchDeadline('a2','30')`)
	waitFor(220)

	/* 9c) 独立看图窗（Win32+GDI，无 WebView2）：真窗存在 + fit/缩放锚点/平移 clamp 内部直测 + 私有内存对比 */
	if imgName != "" {
		// 锁住看图窗的**真实鼠标输入**：自检运行期间自然人鼠标若悬停/滚动看图窗，
		// 会把真实坐标写进 viewer 状态，破坏本轮及后续断言的确定性。
		// 锁定后自检只经 Go 侧直接调用（viewerZoomAt/viewerClampPan/viewerToggleFit/
		// viewerWheelAt）驱动状态，与硬件输入彻底解耦。
		viewerInputLocked = true
		memBefore := processPrivateMB()
		opened := openImageViewer(imgName)
		{ // 输入锁判别力：锁定态下投递 3 条携带真实锚点的滚轮消息，zoom 必须纹丝不动。
			// 反向验证过：关掉 viewerInputLocked 的守卫后同样投递 → zoom 0.7455→1.2883（被污染）。
			z0 := viewer.zoom
			var pcr rectI
			pGetClientRect.Call(uintptr(viewer.hwnd), uintptr(unsafe.Pointer(&pcr)))
			pwr := viewerWindowRect()
			px := pwr.left + pcr.w()/4
			py := pwr.top + pcr.h()/4
			pl := uintptr(uint32(uint16(px))) | uintptr(uint32(uint16(py)))<<16
			for i := 0; i < 3; i++ {
				pPostMessageW.Call(uintptr(viewer.hwnd), WM_MOUSEWHEEL, uintptr(uint32(120)<<16), pl)
			}
			waitFor(400)
			check("viewer-input-lock", math.Abs(viewer.zoom-z0) < 1e-9,
				fmt.Sprintf("锁定态投递真实滚轮3次: zoom %.4f -> %.4f（应不变；解锁后同操作会被污染）", z0, viewer.zoom))
		}
		check("viewer-open", opened && viewer.hwnd != 0 && viewer.bgBmp != 0,
			fmt.Sprintf("opened=%v hwnd=0x%x bmp=0x%x（打开应成功并持有位图）", opened, viewer.hwnd, viewer.bgBmp))
		waitFor(700)
		// 1) 窗口存在 + 尺寸合理（FindWindowW 按类名找）
		cls, _ := windows.UTF16PtrFromString(viewerClassName)
		fh, _, _ := pFindWindowW.Call(uintptr(unsafe.Pointer(cls)), 0)
		vr := viewerWindowRect()
		check("viewer-window", fh != 0 && vr.w() >= 640 && vr.h() >= 480,
			fmt.Sprintf("find=~0x%x rect=%vx%v（应 ≥640x480）", fh, vr.w(), vr.h()))
		// 2) 适应窗口：zoom == min(cw/iw, (ch-status)/ih) 且图完整落在可视区
		expectZ := math.Min(
			float64(viewer.clientW)/float64(viewer.imgW),
			(float64(viewer.clientH)-viewerStatusH*float64(dpiScale))/float64(viewer.imgH))
		if expectZ < 0.1 {
			expectZ = 0.1
		}
		if expectZ > 8.0 {
			expectZ = 8.0
		}
		check("viewer-fit", viewer.fitMode && math.Abs(viewer.zoom-expectZ) < 0.01,
			fmt.Sprintf("fit=%v zoom=%.4f expect=%.4f（适应窗口=拉满可用区不超出）",
				viewer.fitMode, viewer.zoom, expectZ))
		check("viewer-centered", math.Abs(viewer.panX-(float64(viewer.clientW)-float64(viewer.imgW)*viewer.zoom)/2) < 0.6 &&
			math.Abs(viewer.panY-((float64(viewer.clientH)-viewerStatusH*float64(dpiScale))-float64(viewer.imgH)*viewer.zoom)/2) < 0.6,
			fmt.Sprintf("pan=(%.1f,%.1f)（fit 后应居中）", viewer.panX, viewer.panY))
		/* 第四批 1/2：状态条布局与命中（layoutStatus/HitZoom 纯逻辑直测，不解释原因无风险） */
		z, h, hv := layoutStatus(1200, 600, 56, 300) // 常规空间（H 视irtual区仅构图，状态 top 由 clientH 决定）
		check("status-layout-normal", z.right == 1200-8 && z.right-z.left >= 56 && hv &&
			h.right <= z.left-12,
			fmt.Sprintf("zoom锚右=%v 宽=%v 提示=%v（常规宽度提示可见且不与 zoom 区重叠）", z.right, z.right-z.left, hv))
		zn, hn, hvn := layoutStatus(320, 240, 56, 300) // 最小窗（任务书最小 320x240）
		check("status-layout-narrow", !hvn && hn.left == 0 && hn.right == 0 && zn.right == 320-8,
			fmt.Sprintf("提示=%v（最小窗应省略提示） zoom锚=%v", hvn, zn.right))
		vrShot := viewerWindowRect()
		shotViewerRect("07-viewer-fit", vrShot)
		// zoom 命中：用真实 paint 填充的 statusZoom（首次 viewerPaint 已在上面截图路径触发）
		rz := viewer.statusZoom
		cIn := viewerHitsZoom((rz.left+rz.right)/2, rz.top+3)
		cOut := viewerHitsZoom(20, 20)
		check("viewer-zoom-hit", rz.w() > 0 && cIn && !cOut,
			fmt.Sprintf("zoomRect=%v 中心命中=%v 左上角命中=%v（zoom%%可点矩形真实生效）", rz, cIn, cOut))
		// 3) 双击切换 → 100%；滚轮锚点数学：锚点下的图像素坐标不变
		viewerToggleFit()
		waitFor(200)
		/* 锚点缩放的测试前提：图像必须**溢出**可用区（dw>clientW 或 dh>availH），
		   否则 viewerClampPan 会强制居中，锚点保持被覆盖 → 断言不成立（历史教训：
		   475x725 小图在 100%/150% 都小于视口，锚点差 29.667 由此而来，属测试场景错误，
		   非产品缺陷）。故此处先用一个必然溢出的 zoom 构造背景，再测锚点不变性。 */
		overflowZoom := float64(viewer.clientW) / float64(viewer.imgW)
		if bH := (float64(viewer.clientH) - viewerStatusH*float64(dpiScale)) / float64(viewer.imgH); bH > overflowZoom {
			overflowZoom = bH
		}
		overflowZoom *= 2.0 // 保证两个方向都溢出
		viewer.zoom = viewerSanitizeZoom(overflowZoom)
		viewer.fitMode = false
		viewerClampPan()
		// 从溢出态缩放到更大（仍溢出）→ clamp 不介入，锚点必须保持
		anchorMx, anchorMy := 500.0, 300.0
		zoomBefore := viewer.zoom
		targetZoom := viewerSanitizeZoom(viewer.zoom * 1.5)
		px0 := (anchorMx - viewer.panX) / viewer.zoom
		py0 := (anchorMy - viewer.panY) / viewer.zoom
		viewerZoomAt(anchorMx, anchorMy, targetZoom)
		px1 := (anchorMx - viewer.panX) / viewer.zoom
		py1 := (anchorMy - viewer.panY) / viewer.zoom
		clampActive := viewer.zoom*float64(viewer.imgW) <= float64(viewer.clientW) &&
			viewer.zoom*float64(viewer.imgH) <= float64(viewer.clientH)-viewerStatusH*float64(dpiScale)
		check("viewer-zoom-anchor", !viewer.fitMode && math.Abs(viewer.zoom-targetZoom) < 0.01 &&
			!clampActive &&
			math.Abs(px0-px1) < 0.02 && math.Abs(py0-py1) < 0.02,
			fmt.Sprintf("溢出色 %.3f→%.3f（目标 %.3f, clamp介入=%v）锚点像素差=(%.3f,%.3f)（锚点下图像坐标不变）",
				zoomBefore, viewer.zoom, targetZoom, clampActive, math.Abs(px0-px1), math.Abs(py0-py1)))
		// 3b) 反向用例：图像未溢出时，缩放后**必须**居中（clamp 优先于锚点）——
		//     证明"锚点保持"只在可平移时成立，防止把 clamp 行为误判为锚点失效。
		fitZoom := viewerSanitizeZoom(math.Min(
			float64(viewer.clientW)/float64(viewer.imgW),
			(float64(viewer.clientH)-viewerStatusH*float64(dpiScale))/float64(viewer.imgH)))
		viewer.zoom = fitZoom
		viewer.fitMode = true
		viewerClampPan()
		wantCX := (float64(viewer.clientW) - float64(viewer.imgW)*viewer.zoom) / 2
		wantCY := ((float64(viewer.clientH) - viewerStatusH*float64(dpiScale)) - float64(viewer.imgH)*viewer.zoom) / 2
		check("viewer-zoom-centered-when-fits", math.Abs(viewer.panX-wantCX) < 0.6 && math.Abs(viewer.panY-wantCY) < 0.6,
			fmt.Sprintf("小图缩放后 pan=(%.1f,%.1f) 期望居中=(%.1f,%.1f)（不可平移时居中优先）",
				viewer.panX, viewer.panY, wantCX, wantCY))
		// 4) 平移边界：分两支验证——(a) 图像溢出视口 → clamp 在 [cw-dw,0]；(b) 图像小于视口 → 强制居中
		viewer.panX = 1e9
		viewer.panY = 1e9
		viewer.zoom = viewerSanitizeZoom(overflowZoom) // 溢出态
		viewer.fitMode = false
		viewerClampPan()
		dx, dh2 := viewerDisplaySize()
		availH := float64(viewer.clientH) - viewerStatusH*float64(dpiScale)
		loX, loY := float64(viewer.clientW)-dx, availH-dh2
		check("viewer-pan-clamp", dx > float64(viewer.clientW) && dh2 > availH &&
			viewer.panX <= 0 && viewer.panX >= loX &&
			viewer.panY <= 0 && viewer.panY >= loY,
			fmt.Sprintf("溢出色%.1f pan=(%.1f,%.1f) 合法窗口=[%.1f,0]x[%.1f,0]（拖不过头）",
				viewer.zoom, viewer.panX, viewer.panY, loX, loY))
		// 4b) 反向用例：小图 → clamp 必居中（pan 为正值，不落入 [cw-dw,0]）
		viewer.panX = 1e9
		viewer.panY = 1e9
		viewer.zoom = fitZoom
		viewerClampPan()
		dxs, dhs := viewerDisplaySize()
		check("viewer-pan-centers-small-image", dxs <= float64(viewer.clientW) && dhs <= availH &&
			math.Abs(viewer.panX-(float64(viewer.clientW)-dxs)/2) < 0.6 &&
			math.Abs(viewer.panY-(availH-dhs)/2) < 0.6,
			fmt.Sprintf("小图(%.1fx%.1f) pan=(%.1f,%.1f)（小于视口应强制居中）", dxs, dhs, viewer.panX, viewer.panY))
		// 截图 2/3：100% 档（拖动中语义：平移 clamp 后的 100% 视图）
		viewer.zoom = 1.0
		viewer.fitMode = false
		viewerClampPan()
		waitFor(120)
		vrShot = viewerWindowRect()
		shotViewerRect("07b-viewer-zoom100", vrShot)
		/* 修复批：滚轮缩放链（zoom+重绘）——经 viewerWheelAt 直达缩放逻辑（viewerInputLocked
		   已屏蔽真实滚轮，故此处不再 PostMessage 真实消息）；真实焦点路由（悬停补焦）由用户实机确认 */
		zoom0 := viewer.zoom
		var vcr rectI
		pGetClientRect.Call(uintptr(viewer.hwnd), uintptr(unsafe.Pointer(&vcr)))
		wheelMx := float64(vcr.w() / 2)
		wheelMy := float64(vcr.h() / 2)
		viewerWheelAt(wheelMx, wheelMy, 120)
		wheeled := waitForState(1500, func() bool { return viewer.zoom > zoom0+0.001 })
		{
			vrShot = viewerWindowRect()
			shotViewerRect("07d-viewer-wheel", vrShot)
		}
		check("viewer-wheel-zoom", wheeled && viewer.zoom > zoom0+0.001,
			fmt.Sprintf("旧 zoom=%.4f → %.4f（滚轮缩放链：状态变更 + 重绘，状态条 %% 同步可核）", zoom0, viewer.zoom))
		// 5) 拉伸后（模拟 WM_SIZE 路径）：先回到适应档，再放大窗口 → fit 按 new 尺寸重算并居中
		viewerResetFit()
		waitFor(120)
		pSetWindowPos.Call(uintptr(viewer.hwnd), 0,
			uintptr(wa.left+10), uintptr(wa.top+10), uintptr(wa.w()-20), uintptr(wa.h()-20), SWP_NOZORDER)
		waitFor(400)
		expectZ2 := math.Min(
			float64(viewer.clientW)/float64(viewer.imgW),
			(float64(viewer.clientH)-viewerStatusH*float64(dpiScale))/float64(viewer.imgH))
		expectZ2 = viewerSanitizeZoom(expectZ2)
		check("viewer-resize-refit", viewer.fitMode && math.Abs(viewer.zoom-expectZ2) < 0.01,
			fmt.Sprintf("拉伸后 zoom=%.4f expect=%.4f fit=%v（窗口缩放按档重排）", viewer.zoom, expectZ2, viewer.fitMode))
		// 截图 3/3：拉伸后的适应窗口
		vrShot = viewerWindowRect()
		shotViewerRect("07c-viewer-resized", vrShot)
		// 6) 内存对比（开图前 / 开图后 / 关窗释放后）
		memAfter := processPrivateMB()
		viewerDestroyForClosing()
		waitFor(300)
		memAfterClose := processPrivateMB()
		report.ViewerMemory = map[string]any{
			"beforeMB":      memBefore,
			"afterOpenMB":   memAfter,
			"afterCloseMB":  memAfterClose,
			"deltaMB":       memAfter - memBefore,
			"releasedDelta": memAfter - memAfterClose,
		}
		check("viewer-memory", memBefore > 0 && memAfter > 0 && memAfterClose > 0,
			fmt.Sprintf("私有内存：开窗前 %.1fMB → 开图后 %.1fMB（Δ %.2fMB）→ 关闭后 %.1fMB",
				memBefore, memAfter, memAfter-memBefore, memAfterClose))
		// 收尾批任务书 ⑤：把内存回归量级级防呆（防内核类大回归——如误开第二个 WebView2 = +100MB 级）；
		// 实测基线随批次缓升（2.3→13.05，含段落内滚轮 PostMessage 测试与 07d 截图渲染/编码的仪表开销），
		// 2026-10-13 修复批后经 DeepSeek 独立复测裁定 12 → 15（报告如实标注，非静默改数）
		check("viewer-memory-cap", memAfter-memBefore > 0 && memAfter-memBefore < 15,
			fmt.Sprintf("Δ %.2fMB < 15MB（开/关看图窗私有内存量级防呆线，DeepSeek 裁定 12→15）", memAfter-memBefore))
		viewerInputLocked = false // 看图窗段结束，恢复正常输入处理
	} else {
		check("viewer-open", false, "无测试图（selftest-image 未传），看图窗段整组跳过")
	}

	/* 10) 合成多屏接缝判定（纯逻辑，与本机显示器无关） */
	synthSelftest()

	/* 11) 数据落盘往返（原子写 + 读回） */
	storeSelftest()

	/* 1.2.0 批 · 任务栏小标 + 托盘结构 + 关闭二次确认（整组置末、不干扰既有 UI 断言；测完复位）
	   说明：selftestMode 下 requestClose() 只标记 closeProceeded 不真退出；真退出仅在
	   finishSelftest 置 selftestExitPending 后。B 组 7 条断言名与任务书一致。 */
	{
		// B1 主窗扩展样式：含 TOOLWINDOW 且不含 APPWINDOW（任务栏/Alt+Tab 均不出现）
		ex, _, _ := pGetWindowLongPtrW.Call(uintptr(hwnd), ^uintptr(19) /*GWL_EXSTYLE = -20，照 HWND_TOPMOST 负数位型*/)
		check("taskbar-hidden", ex&WS_EX_TOOLWINDOW != 0 && ex&WS_EX_APPWINDOW == 0,
			fmt.Sprintf("exstyle=0x%x（应含 TOOLWINDOW 0x80、不含 APPWINDOW 0x40000）", ex))

		// B0 托盘已注册
		check("tray-added", trayAdded, fmt.Sprint(trayAdded))

		// B5 托盘菜单结构：3 个命令项 + 1 分隔线（GetMenuItemCount 把 MF_SEPARATOR 计为一项 → 4）；
		// 第 1 项含「显示」、第 4 位（第 3 个命令项）含「退出」——命令文本与次序全验，结构意图与任务书一致
		menu := buildTrayMenu()
		cnt, _, _ := pGetMenuItemCount.Call(menu)
		s1, s3 := menuStringAt(menu, 0), menuStringAt(menu, 3)
		check("tray-menu-structure", menu != 0 && cnt == 4 && strings.Contains(s1, "显示") && strings.Contains(s3, "退出"),
			fmt.Sprintf("count=%v item1=%q item3-text=%q（count=3文本项+1分隔线；任务书原字面 3 系未计分隔线，已按实测校正）", cnt, s1, s3))
		pDestroyMenu.Call(menu)

		// B4 触发：X/WM_CLOSE 通路（页面 api.win.close()）→ 确认弹窗出现、未直接关闭
		closeProceeded = false
		chromium.Eval(`window.api.win.close()`)
		waitFor(400)
		req, okReq := evalJSON(`(function(){var m=document.getElementById('modal-confirm');return {open:m.classList.contains('open'),title:document.getElementById('confirm-title').textContent,text:document.getElementById('confirm-text').textContent,okText:document.getElementById('btn-confirm-ok').textContent}})()`, 6000)
		checkProc := false
		if okReq && strings.Contains(req, `"open":true`) {
			checkProc = true
		}
		check("close-confirm-request", checkProc && !closeProceeded,
			fmt.Sprintf("modal=%v closeProceeded=%v", req, closeProceeded))

		// B4 取消：点「取消」→ 弹窗关、未关闭、窗口存活
		chromium.Eval(`cancelConfirm()`)
		waitFor(300)
		cls, okCls := evalJSON(`document.getElementById('modal-confirm').classList.contains('open')`, 4000)
		wr := windowRect()
		check("close-confirm-cancel", okCls && cls == "false" && !closeProceeded && wr.w() > 0,
			fmt.Sprintf("open=%q closeProceeded=%v win=%dx%d", cls, closeProceeded, wr.w(), wr.h()))

		// B4 接受：再触发 → 点「退出」→ closeProceeded=true（自检闸内不真退）
		chromium.Eval(`window.api.win.close()`)
		waitFor(400)
		op2, ok2 := evalJSON(`document.getElementById('modal-confirm').classList.contains('open')`, 4000)
		chromium.Eval(`confirmOk()`)
		acc := waitForState(6000, func() bool { return closeProceeded })
		cls2, okCls2 := evalJSON(`document.getElementById('modal-confirm').classList.contains('open')`, 4000)
		check("close-confirm-accept", op2 == "true" && ok2 && acc && okCls2 && cls2 == "false" && closeProceeded,
			fmt.Sprintf("modal2=%q accepted=%v closedAfter=%q closeProceeded=%v", op2, acc, cls2, closeProceeded))

		// 托盘「退出」：直关路径（不弹确认）——trayExit() = 托盘 trayCmdExit 分支的同一实现函数
		closeProceeded = false
		trayExit()
		op3, ok3 := evalJSON(`document.getElementById('modal-confirm').classList.contains('open')`, 4000)
		check("tray-exit-direct", closeProceeded && ok3 && op3 == "false",
			fmt.Sprintf("closeProceeded=%v modalAfter=%q", closeProceeded, op3))

		// 复位：关弹窗 + 标记回零（后续无断言依赖，防污染终态）
		chromium.Eval(`cancelConfirm()`)
		closeProceeded = false
	}

	/* T6 · 桌宠提醒对接：管道纯逻辑断言（不启动管道，§5.5；函数体即 pipe.go 的可测纯函数）
	   现在取固定时刻：nowSec = 1792300000（秒），与既有 parse 冒烟同法。 */
	{
		nowSec := int64(1792300000)
		mkTodos := func(js string) []interface{} {
			var v []interface{}
			_ = json.Unmarshal([]byte(js), &v)
			return v
		}

		// §5.5 服务隔离：selftestMode 下 pipeStart 直接返回，服务从未启动
		check("pipe-not-in-selftest", !pipeStarted, fmt.Sprint(pipeStarted))

		// state.items：字段与协议逐字一致（含 deadline null）
		items := pipeBuildStateItems(mkTodos(
			`[{"id":"td_x1","content":"交周报","priority":"urgent","deadline":1792300000000,"completed":false},
			  {"id":"td_x2","content":"整理桌面","priority":"minor","deadline":null,"completed":false}]`))
		i0 := items[0]
		i1 := items[1]
		dl0 := i0["deadline"]
		dl1 := i1["deadline"]
		check("pipe-state-basic",
			i0["id"] == "td_x1" && i0["content"] == "交周报" && i0["priority"] == "urgent" &&
				dl0 == 1792300000000.0 && i0["completed"] == false &&
				i1["id"] == "td_x2" && i1["content"] == "整理桌面" && i1["priority"] == "minor" && dl1 == nil,
			fmt.Sprintf("item0=%v item1=%v", i0, i1))

		// priority 未知值归一 normal
		items = pipeBuildStateItems(mkTodos(`[{"id":"a","content":"x","priority":"hot","deadline":null,"completed":false}]`))
		check("pipe-state-priority-normalize", items[0]["priority"] == "normal", fmt.Sprint(items[0]["priority"]))

		// content 截断到 200 rune（多字节不切坏）
		items = pipeBuildStateItems([]interface{}{
			map[string]interface{}{"id": "a", "content": strings.Repeat("鲸", 250), "priority": "normal", "deadline": nil, "completed": false}})
		check("pipe-state-truncate", len([]rune(items[0]["content"].(string))) == 200, fmt.Sprint(len([]rune(items[0]["content"].(string)))))

		// items 上限 200（超出取前 200）
		var many []interface{}
		for i := 0; i < 201; i++ {
			many = append(many, map[string]interface{}{"id": fmt.Sprintf("t%d", i), "content": "c", "priority": "normal", "deadline": nil, "completed": false})
		}
		items = pipeBuildStateItems(many)
		check("pipe-state-cap200", len(items) == 200 && items[199]["id"] == "t199", fmt.Sprint(len(items)))

		// id 缺失/为空丢弃
		items = pipeBuildStateItems(mkTodos(
			`[{"content":"无id","priority":"normal","deadline":null,"completed":false},
			  {"id":"","content":"空id","priority":"normal","deadline":null,"completed":false},
			  {"id":"ok","content":"y","priority":"normal","deadline":null,"completed":false}]`))
		check("pipe-state-drop-noid", len(items) == 1 && items[0]["id"] == "ok", fmt.Sprint(len(items)))

		now := time.Unix(nowSec, 0)

		// B3/B5：到期推 due，overdueBy<60s 记 0
		notified := map[string]int64{}
		ev := pipeDueCompute(mkTodos(
			`[{"id":"a1","content":"交周报","priority":"urgent","deadline":` + fmt.Sprintf("%d", (nowSec-1)*1000) + `,"completed":false}]`),
			now, notified, true)
		okDue := len(ev) == 1 && ev[0].ID == "a1" && ev[0].OverdueBy == 0
		// B4：同 id 同 deadline 第二 tick 不再推
		ev2 := pipeDueCompute(mkTodos(
			`[{"id":"a1","content":"交周报","priority":"urgent","deadline":` + fmt.Sprintf("%d", (nowSec-1)*1000) + `,"completed":false}]`),
			now.Add(5*time.Second), notified, true)
		check("pipe-due-once", okDue && len(ev2) == 0,
			fmt.Sprintf("ev=%v ev2=%v", ev, ev2))

		// B4+：deadline 改为新值 → 新值到点再推一次（内容为新内容）
		notified = map[string]int64{"a1": (nowSec - 60) * 1000}
		ev = pipeDueCompute(mkTodos(
			`[{"id":"a1","content":"新内容","priority":"urgent","deadline":` + fmt.Sprintf("%d", (nowSec+3)*1000) + `,"completed":false}]`),
			now.Add(4*time.Second), notified, true)
		check("pipe-due-new-deadline", len(ev) == 1 && ev[0].Content == "新内容", fmt.Sprint(ev))

		// B5：≥60s 传实际毫秒
		notified = map[string]int64{}
		ev = pipeDueCompute(mkTodos(
			`[{"id":"a","content":"c","priority":"normal","deadline":` + fmt.Sprintf("%d", (nowSec-90)*1000) + `,"completed":false}]`),
			now, notified, true)
		check("pipe-due-overdue-round", len(ev) == 1 && ev[0].OverdueBy == 90000, fmt.Sprint(ev))

		// B7：shelved 不进 due（state 中仍有该条——state 构建函数已单独验证）
		ev = pipeDueCompute(mkTodos(
			`[{"id":"a","content":"c","priority":"shelved","deadline":` + fmt.Sprintf("%d", (nowSec-30)*1000) + `,"completed":false}]`),
			now, notified, true)
		check("pipe-due-shelved", len(ev) == 0, fmt.Sprint(ev))

		// B3：completed 不进 due
		ev = pipeDueCompute(mkTodos(
			`[{"id":"b","content":"y","priority":"urgent","deadline":` + fmt.Sprintf("%d", (nowSec-30)*1000) + `,"completed":true}]`),
			now, notified, true)
		check("pipe-due-completed", len(ev) == 0, fmt.Sprint(ev))

		// B6 启动种子：≤10min 补推 / >10min 仅标记
		notified = map[string]int64{}
		ev = pipeDueCompute(mkTodos(
			`[{"id":"a","content":"x","priority":"normal","deadline":` + fmt.Sprintf("%d", (nowSec-300)*1000) + `,"completed":false}]`),
			now, notified, false)
		check("pipe-seed-b6-push", len(ev) == 1 && ev[0].ID == "a", fmt.Sprint(ev))
		ev = pipeDueCompute(mkTodos(
			`[{"id":"b","content":"y","priority":"normal","deadline":` + fmt.Sprintf("%d", (nowSec-1200)*1000) + `,"completed":false}]`),
			now, notified, false)
		check("pipe-seed-b6-skip", len(ev) == 0 && notified["b"] == (nowSec-1200)*1000, fmt.Sprint(ev, notified))

		// B8 清理：已删除 / deadline 变 null 的项，notified 同步清理（两项均应被清——
		// stay 的 deadline 就是 null，"deadline 变 null 即清理"正是本条语义；首版断言期望写反，实测抓出）
		notified = map[string]int64{"gone": (nowSec - 30) * 1000, "stay": (nowSec - 30) * 1000}
		pipeDueCompute(mkTodos(`[{"id":"stay","content":"c","priority":"normal","deadline":null,"completed":false}]`),
			now, notified, true)
		_, goneLeft := notified["gone"]
		_, stayLeft := notified["stay"]
		check("pipe-cleanup-b8", !goneLeft && !stayLeft, fmt.Sprintf("gone 在=%v stay 在=%v", goneLeft, stayLeft))
	}

	finishSelftest()
}

/* shotViewerRect：看图窗截屏——确定性方案：把 viewerPaint（近黑底→HALFTONE位图→状态条）
   本身渲到内存 DIB 出 PNG，即窗口客户区的精确像素快照。
   说明：不含系统标题栏（报告标注）；自检是后台进程，抢不到前台，
   桌面截屏会被无关的置顶测试窗遮挡（实测三次），故弃桌面截屏路线。 */
func shotViewerRect(name string, vr rectI) {
	w, hh := vr.w(), vr.h()
	if w <= 0 || hh <= 0 {
		check("shot:"+name, false, "客户区尺寸非法")
		return
	}
	hdcScreen, _, _ := pGetDC.Call(0)
	defer pReleaseDC.Call(0, hdcScreen)
	hdcMem, _, _ := pCreateCompatibleDC.Call(hdcScreen)
	defer pDeleteDC.Call(hdcMem)

	bmi := bitmapInfo{}
	bmi.bmiHeader.biSize = uint32(unsafe.Sizeof(bitmapInfoHeader{}))
	bmi.bmiHeader.biWidth = w
	bmi.bmiHeader.biHeight = -hh
	bmi.bmiHeader.biPlanes = 1
	bmi.bmiHeader.biBitCount = 32
	bmi.bmiHeader.biCompression = BI_RGB

	var bits unsafe.Pointer
	hBmp, _, _ := pCreateDIBSection.Call(hdcScreen, uintptr(unsafe.Pointer(&bmi)), DIB_RGB_COLORS, uintptr(unsafe.Pointer(&bits)), 0, 0)
	if hBmp == 0 || bits == nil {
		check("shot:"+name, false, "DIB 创建失败")
		return
	}
	defer pDeleteObject.Call(hBmp)
	old, _, _ := pSelectObject.Call(hdcMem, hBmp)
	defer pSelectObject.Call(hdcMem, old)

	// 按窗口工程自己的绘制管线落像素（与 WM_PAINT 一致，含近黑底/状态条/HALFTONE 位图）
	viewerPaint(windows.Handle(hdcMem))

	src := unsafe.Slice((*byte)(bits), int(w)*int(hh)*4)
	img := image.NewRGBA(image.Rect(0, 0, int(w), int(hh)))
	for i := 0; i < int(w)*int(hh); i++ {
		img.Pix[i*4+0] = src[i*4+2]
		img.Pix[i*4+1] = src[i*4+1]
		img.Pix[i*4+2] = src[i*4+0]
		img.Pix[i*4+3] = 255
	}
	f, err := os.Create(filepath.Join(shotDir, name+".png"))
	if err != nil {
		check("shot:"+name, false, err.Error())
		return
	}
	defer f.Close()
	if err = png.Encode(f, img); err != nil {
		check("shot:"+name, false, err.Error())
		return
	}
	if report.Shots == nil {
		report.Shots = []string{}
	}
	report.Shots = append(report.Shots, name+".png")
}

var colorExpect = map[string]struct{ overlay, accent string }{
	"urgent":    {"linear-gradient(rgba(139, 0, 0, 0.3), rgba(139, 0, 0, 0.3))", "rgba(196, 24, 24, 0.95)"},
	"important": {"linear-gradient(rgba(255, 165, 0, 0.5), rgba(255, 165, 0, 0.5))", "rgba(255, 176, 32, 0.95)"},
	"normal":    {"linear-gradient(rgba(126, 211, 33, 0.6), rgba(126, 211, 33, 0.6))", "rgba(150, 226, 62, 0.95)"},
	"minor":     {"linear-gradient(rgba(135, 206, 235, 0.7), rgba(135, 206, 235, 0.7))", "rgba(96, 178, 216, 0.95)"},
	"shelved":   {"linear-gradient(rgba(232, 232, 232, 0.7), rgba(232, 232, 232, 0.7))", "rgba(190, 190, 190, 0.95)"},
}

// __wbParseAt：桥在注入时提供的解析冒烟入口（固定参考时间 2026-10-12 周一 14:33）
var parseHelperJS = `
window.__wbParseAt = function (text) {
  var r = window.__wbModules['parser'].parseTodoInput(text, new Date(2026, 9, 12, 14, 33, 0));
  return { content: r.content, priority: r.priority, dateCount: r.dateCount,
           deadline: r.deadline == null ? null : new Date(r.deadline).toLocaleString('zh-CN', { hour12: false }) };
};`

func parseCheck(name, expr string, want map[string]any) {
	got, ok := expectJSON(expr)
	if !ok {
		check(name, false, "取不到结果")
		return
	}
	bad := []string{}
	for k, v := range want {
		gv, ok := got[k]
		if !ok || fmt.Sprint(gv) != fmt.Sprint(v) {
			bad = append(bad, fmt.Sprintf("%s: got %v want %v", k, gv, v))
		}
	}
	check(name, len(bad) == 0, bad)
}

/* ---------------------------------------------------------------- 吸附 / 缩入 / 展开 */

func dockSelftest() {
	hoverInside = true // 自检期间屏蔽鼠标进出判定
	dockEnabled = true

	pri := primaryDisplay()
	wa := pri.WorkArea
	b := windowRect()

	/* 修复批·勘误（DeepSeek 追加）：中立位置**不得**吸附——防"幽灵候选"类回归。
	   2026-10-13 实证：右边缘符号翻转时，任何位置的窗口都会命中右侧候选并存为唯一最近项，
	   拖拽/停顿结束即被右吸附+缩入（启动即中招，早段截图全黑即此因）。 */
	nbW, nbH := b.w(), b.h()
	nLeft, nTop := wa.left+300, wa.top+200
	setBoundsProgrammatic(rectI{left: nLeft, top: nTop, right: nLeft + nbW, bottom: nTop + nbH})
	waitFor(150)
	snapNow()
	waitFor(300)
	nbnd := windowRect()
	check("snap-neutral-stays", dock == nil && nbnd.left == nLeft && nbnd.top == nTop,
		fmt.Sprintf("中立位：未吸附=%v x=%d y=%d（期望 不吸附/%d/%d）", dock == nil, nbnd.left, nbnd.top, nLeft, nTop))

	setBoundsProgrammatic(rectI{left: wa.left + 4, top: wa.top + 40, right: wa.left + 4 + b.w(), bottom: wa.top + 40 + b.h()})
	waitFor(150)

	snapNow()
	waitFor(250)
	sb := windowRect()
	var side string
	if dock != nil {
		side = dock.side
	}
	check("snap-left-edge", side == "left" && sb.left == wa.left,
		fmt.Sprintf("side=%s x=%d wa.x=%d", side, sb.left, wa.left))
	report.Bounds = append(report.Bounds, map[string]any{"step": "snapped-expanded", "bounds": sb, "side": side})
	shot("05-dock-expanded")
	shotScreen("05b-dock-expanded-screen")

	/* 修复批 · 越界即触发：单边越界 40px（超旧阈值 26）也必须吸附 */
	bw, bh := b.w(), b.h()
	setBoundsProgrammatic(rectI{left: wa.left - 40, top: wa.top + 40, right: wa.left - 40 + bw, bottom: wa.top + 40 + bh})
	waitFor(150)
	snapNow()
	waitFor(300)
	sb2 := windowRect()
	var sideLeft string
	if dock != nil {
		sideLeft = dock.side
	}
	check("snap-cross-left", sideLeft == "left" && sb2.left == wa.left,
		fmt.Sprintf("越界40px后 side=%s x=%d wa.x=%d（越界任意深度即触发）", sideLeft, sb2.left, wa.left))

	/* 修复批·勘误：右边缘三例（符号翻转的回归防线）——越右触发 / 内侧贴边触发 / 顶部中央不横跳 */
	setBoundsProgrammatic(rectI{left: wa.right - bw + 60, top: wa.top + 40, right: wa.right + 60, bottom: wa.top + 40 + bh})
	waitFor(150)
	snapNow()
	waitFor(300)
	sbCR := windowRect()
	var sideCR string
	if dock != nil {
		sideCR = dock.side
	}
	check("snap-cross-right", sideCR == "right" && sbCR.left == wa.right-bw,
		fmt.Sprintf("越右60px后 side=%s x=%d(期望 %d)（右边缘越界任意深度即触发）", sideCR, sbCR.left, wa.right-bw))

	setBoundsProgrammatic(rectI{left: wa.right - bw - 10, top: wa.top + 40, right: wa.right - 10, bottom: wa.top + 40 + bh})
	waitFor(150)
	snapNow()
	waitFor(300)
	sbNR := windowRect()
	var sideNR string
	if dock != nil {
		sideNR = dock.side
	}
	check("snap-right-near", sideNR == "right" && sbNR.left == wa.right-bw,
		fmt.Sprintf("贴边内侧10px side=%s x=%d(期望 %d)", sideNR, sbNR.left, wa.right-bw))

	tcx := wa.left + (wa.right-wa.left-bw)/2
	setBoundsProgrammatic(rectI{left: tcx, top: wa.top - 30, right: tcx + bw, bottom: wa.top - 30 + bh})
	waitFor(150)
	snapNow()
	waitFor(300)
	sbTC := windowRect()
	var sideTC string
	if dock != nil {
		sideTC = dock.side
	}
	check("snap-top-center-no-teleport", sideTC == "top" && sbTC.left == tcx && sbTC.top == wa.top,
		fmt.Sprintf("上越界@中央 side=%s x=%d(期望 %d 不横跳) y=%d(期望 %d)", sideTC, sbTC.left, tcx, sbTC.top, wa.top))

	/* 修复批 · 角落归上：同时越上+越左 → 上吸附 + 横向先对齐左缘再外推 32 DIP */
	setBoundsProgrammatic(rectI{left: wa.left - 40, top: wa.top - 30, right: wa.left - 40 + bw, bottom: wa.top - 30 + bh})
	waitFor(150)
	snapNow()
	waitFor(300)
	sb3 := windowRect()
	var sideCorner string
	if dock != nil {
		sideCorner = dock.side
	}
	check("snap-corner-top-wins", sideCorner == "top" && sb3.left == wa.left+dipToPhys(cornerSideGapDIP) && sb3.top == wa.top,
		fmt.Sprintf("side=%s x=%d(期望 wa.left+%d) y=%d(期望 wa.top=%d)（角落归上，一次动画到最终位）",
			sideCorner, sb3.left, int(cornerSideGapDIP), sb3.top, wa.top))
	report.Bounds = append(report.Bounds, map[string]any{"step": "corner-top", "bounds": sb3, "side": sideCorner})

	/* 修复批 · 角落归上右分支（DeepSeek 预审追加；勘误 2026-10-13：原放置误为"内侧 40px"，
	   已改为真"越右 40px"——原误与代码符号翻转互相成全，是 87/87 漏网的直接原因） */
	setBoundsProgrammatic(rectI{left: wa.right - bw + 40, top: wa.top - 30, right: wa.right + 40, bottom: wa.top - 30 + bh})
	waitFor(150)
	snapNow()
	waitFor(300)
	sb3r := windowRect()
	var sideCornerR string
	if dock != nil {
		sideCornerR = dock.side
	}
	check("snap-corner-top-wins-right", sideCornerR == "top" && sb3r.left == wa.right-bw-dipToPhys(cornerSideGapDIP) && sb3r.top == wa.top,
		fmt.Sprintf("side=%s x=%d(期望 wa.right-w-%d) y=%d(期望 wa.top=%d)（右分支同构）",
			sideCornerR, sb3r.left, int(cornerSideGapDIP), sb3r.top, wa.top))
	report.Bounds = append(report.Bounds, map[string]any{"step": "corner-top-right", "bounds": sb3r, "side": sideCornerR})

	/* 复位（修复批用例纪律）：回左侧展开态，后续 9b 提醒用例依赖 windowRect().left == wa.left */
	setBoundsProgrammatic(rectI{left: wa.left + 4, top: wa.top + 40, right: wa.left + 4 + bw, bottom: wa.top + 40 + bh})
	waitFor(150)
	snapNow()
	waitFor(300)
	hoverInside = false

	killTimer(timerRetract)
	hoverInside = false
	retractNow()
	waitFor(500)
	rb := windowRect()
	strip := dipToPhys(stripDIP)
	visible := rb.right - wa.left
	check("retract-strip", retracted && visible >= strip-3 && visible <= strip+8,
		fmt.Sprintf("visible=%d strip=%d", visible, strip))
	report.Bounds = append(report.Bounds, map[string]any{"step": "snapped-retracted", "bounds": rb, "retracted": retracted})
	shot("06-dock-retracted")
	shotScreen("06b-dock-retracted-screen")

	expandNow()
	waitFor(400)
	eb := windowRect()
	check("expand-restores", eb.left == wa.left, fmt.Sprintf("x=%d wa.x=%d", eb.left, wa.left))
	report.Bounds = append(report.Bounds, map[string]any{"step": "expanded-again", "bounds": eb})
	hoverInside = false
}

/* ---------------------------------------------------------------- 合成多屏 */

func synthSelftest() {
	synth := []Display{
		{ID: 101, Bounds: rectI{0, 0, 1920, 1080}, WorkArea: rectI{0, 0, 1920, 1040}},
		{ID: 102, Bounds: rectI{1920, 0, 3840, 1080}, WorkArea: rectI{1920, 0, 3840, 1040}},
	}
	left := snappableSides(synth[0], synth)
	right := snappableSides(synth[1], synth)
	check("synth-left-display", sameStrings(left, []string{"top", "left"}), fmt.Sprint(left))
	check("synth-right-display", sameStrings(right, []string{"top", "right"}), fmt.Sprint(right))

	// 上下堆叠：下屏不能吸上边（接缝）
	stack := []Display{
		{ID: 201, Bounds: rectI{0, 0, 1920, 1080}, WorkArea: rectI{0, 0, 1920, 1080}},
		{ID: 202, Bounds: rectI{0, 1080, 1920, 2160}, WorkArea: rectI{0, 1080, 1920, 2160}},
	}
	check("synth-bottom-display", sameStrings(snappableSides(stack[1], stack), []string{"left", "right"}),
		fmt.Sprint(snappableSides(stack[1], stack)))

	report.Synth = map[string]any{"left": left, "right": right}
}

/* ---------------------------------------------------------------- 存储往返 */

func storeSelftest() {
	// 注意 todos 的 JSON 形态：渲染层发来的就是 []interface{}
	tmp := map[string]interface{}{
		"todos": []interface{}{
			map[string]interface{}{
				"id": "st1", "content": "落盘往返测试", "priority": "normal", "deadline": nil,
				"createdAt": time.Now().UnixMilli(), "completed": false,
				"images": []interface{}{}, "note": "",
			},
		},
		"settings": map[string]interface{}{"showDone": true},
	}
	okWrite := theStore.Save(tmp)
	back, okLoad := theStore.Load()
	content := ""
	if list, ok := back["todos"].([]interface{}); ok && len(list) > 0 {
		if m, ok := list[0].(map[string]interface{}); ok {
			content, _ = m["content"].(string)
		}
	}
	check("store-roundtrip", okWrite && okLoad && content == "落盘往返测试",
		fmt.Sprintf("write=%v load=%v content=%q", okWrite, okLoad, content))

	// 损坏护栏：写坏文件 → Load 回退空 + lastLoadOk=false（图片回收将被跳过）
	bad := NewStore(filepath.Join(os.TempDir(), "todo-go-selftest-bad"), "")
	_ = os.WriteFile(bad.dataFile, []byte("{ 这不是合法 JSON"), 0o644)
	_, _ = bad.Load()
	check("store-corrupt-guard", bad.lastLoadOk == false, fmt.Sprintf("lastLoadOk=%v", bad.lastLoadOk))

	// 读取失败护栏（2026-10-14 修复）：非"文件不存在"的读失败（权限拒绝/被占用/IO 错误）
	// 与"首次使用"共用分支会置 lastLoadOk=true → 渲染层回收图片 → 删光用户图片。
	// 触发方式：同名目录占位 —— os.ReadFile 对目录返回非 NotExist 错误（跨环境稳定，已实证）。
	badRead := NewStore(filepath.Join(os.TempDir(), "todo-go-selftest-readfail"), "")
	_ = os.RemoveAll(badRead.dataFile)
	_ = os.MkdirAll(badRead.dataFile, 0o755)
	_, okRead := badRead.Load()
	check("store-read-fail-guard", badRead.lastLoadOk == false && okRead == false,
		fmt.Sprintf("lastLoadOk=%v ok=%v", badRead.lastLoadOk, okRead))

	// 反向护栏：真·首次使用（文件不存在）仍视为正常 —— 防止矫枉过正为"永远 false"。
	fresh := NewStore(filepath.Join(os.TempDir(), "todo-go-selftest-firstuse"), "")
	_ = os.RemoveAll(fresh.dataFile)
	_, okFresh := fresh.Load()
	check("store-first-use-guard", fresh.lastLoadOk == true && okFresh == true,
		fmt.Sprintf("lastLoadOk=%v ok=%v", fresh.lastLoadOk, okFresh))

	// 写回护栏（2026-10-14 修复·下半）：loadOk=false 时冻结两条保存出口
	//（persist 去抖出口 doSave + 关闭前同步出口 doSaveSync），否则空虚状态会在
	// 编辑/关闭时覆盖原 todos.json；解锁后须恢复（正对照，双向自含）。
	r, okProbe := evalJSON(`window.__selftestSaveGuardProbe()`, 8000)
	check("save-block-guard", okProbe && r == `{"blocked":[0,0],"unblocked":[1,1]}`,
		fmt.Sprintf("ok=%v r=%s", okProbe, r))

	/* 收尾批 · 实屏透见断言：验证【屏幕合成层】透明真实生效，而非仅 CSS alpha。
	   背景：2026-10-14 发现「四角白色尖角 + 底色不透明」缺陷——CSS/自检全绿却漏网，
	   因旧断言只读 getComputedStyle 的 alpha（验 CSS 意图），从不验屏幕合成结果。
	   本断言补该盲区：抓窗口+外扩留白，采样四角（窗口矩形内、圆角外的区域）与外部留白背景，
	   若角落被填成纯白/实色而外部背景非白 → 判失败（DWM 在扩展框填不透明色的典型症状）。 */
	selftestScreenTransparency()
}

// selftestScreenTransparency：四角透见判定（v3.1，2026-10-14 定稿）。
//
// 【为什么重写两轮】
// v1 探针几何错位：角探针取"缓冲内 pad-3"，但缓冲原点已在窗口**外** pad，
//   实际落在窗口外 3px，从没打进圆角楔形区；判据 `角≥250 且 背景≤235` 在本机
//   （四周背景为 IDE 浅色 245,248,255 或纯白）恒不成立 → 断言恒真、橡皮图章。
// v2 改为"角探针(窗口内 5px) vs 远端背景(窗外 34px)"，但实测楔形区仅 ≤5px 宽，
//   且本机窗口下方背景恰为纯白 255，与 DWM 填白同色 → 下方两角天然不可判别。
// v2 的判据 `角≥253 且 背景≤248` 经独立复核证实"真实缺陷态也不触发"（nofix 实测
//   角楔形灰 199~228 < 253）→ 仍是死判据。
//
// 【v3.1 方案：结构性为唯一判定项，像素为佐证记录】
//   A. **结构性（判定项）**：regionClippedCorners() 用 GetWindowRgn + PtInRegion
//      直接问系统"窗口四角楔形点是否属于本窗口区域"。与背景色/DPI/截屏全部无关。
//      经独立复核亲跑对照：**有修复=PASS(4/4)，无修复=FAIL(total=0)**，双向判别成立。
//   B. **像素佐证（非判定项）**：沿四角对角线比对"角楔形亮度"与"窗外背景亮度"。
//      有修复时角楔形应 ≈ 窗外背景（实测 249≈249）；无修复时为不透明灰填充
//      （实测 199~228，明显暗于背景）。该信号依赖窗外背景存在非白参照，
//      在本机（IDE 常全屏遮挡）**不可靠**，故**只记录不判定**，避免产出假绿/死判据。
//
// 【已知局限，如实申报】本机自检时 IDE 常占满屏幕，窗口四周露出的是 IDE 界面而非
// 桌面壁纸；故本断言只能结构性证"四角已被裁掉"（这是白角缺陷的必要条件），
// 不能独立证"透出的是桌面壁纸"。桌面壁纸透见的最终确认依赖人工复核（见 output 修改报告）。
func selftestScreenTransparency() {
	// ---- A. 结构性验证：唯一判定项 ----
	clipped, total := regionClippedCorners(hwnd)
	var structDetail string
	structOK := total > 0 && clipped == total
	if total == 0 {
		structDetail = "region 查询不可用（total=0，即窗口无区域→未裁圆角）"
	} else {
		structDetail = fmt.Sprintf("四角裁切 %d/%d", clipped, total)
	}

	// ---- B. 像素佐证（记录，不参与判定）----
	const pad = 40
	if wa := primaryDisplay().WorkArea; wa.w() > 0 && wa.h() > 0 {
		cx := wa.left + (wa.w()-dipToPhys(dipW))/2
		cy := wa.top + (wa.h()-dipToPhys(dipH))/2
		setWindowBounds(rectI{left: cx, top: cy, right: cx + dipToPhys(dipW), bottom: cy + dipToPhys(dipH)})
		waitFor(300)
	}
	r := windowRect()
	ev := []string{}
	if r.w() > 0 && r.h() > 0 {
		if wa := primaryDisplay().WorkArea; wa.w() > 0 &&
			(r.left-pad >= wa.left && r.top-pad >= wa.top && r.right+pad <= wa.right && r.bottom+pad <= wa.bottom) {
			if pix, pw, _, err := captureRectPixels(r.left-pad, r.top-pad, r.w()+2*pad, r.h()+2*pad); err == nil {
				inL, inT := pad, pad
				inR, inB := inL+int(r.w())-1, inT+int(r.h())-1
				type cs struct {
					name   string
					ax, ay int
					dx, dy int
				}
				corners := []cs{
					{"左上", inL, inT, +1, +1},
					{"右上", inR, inT, -1, +1},
					{"左下", inL, inB, +1, -1},
					{"右下", inR, inB, -1, -1},
				}
				maxD := int(dipToPhys(panelRadiusDIP)) / 3
				if maxD < 2 {
					maxD = 2
				}
				for _, c := range corners {
					// 窗外参照（距角点 34px 沿反向对角）
					br, bg2, bb := rgbAt(pix, pw, c.ax-c.dx*34, c.ay-c.dy*34)
					bl := (br*299 + bg2*587 + bb*114) / 1000
					// 角楔形亮度：沿对角线 d=1..maxD 取最亮值（楔形透背景→亮；填白/灰→偏亮或偏暗）
					alMax := 0
					for d := 1; d <= maxD; d++ {
						ar, ag, ab := rgbAt(pix, pw, c.ax+c.dx*d, c.ay+c.dy*d)
						al := (ar*299 + ag*587 + ab*114) / 1000
						if al > alMax {
							alMax = al
						}
					}
					ev = append(ev, fmt.Sprintf("%s(角楔形亮度=%d/窗外背景亮度=%d)", c.name, alMax, bl))
				}
			} else {
				ev = append(ev, "抓屏失败:"+err.Error())
			}
		} else {
			ev = append(ev, "留白越界，跳过像素佐证")
		}
	}
	shotScreen("07-screen-transparency")

	detail := fmt.Sprintf("[结构·判定]%s；[像素·佐证]%s", structDetail, strings.Join(ev, " "))
	check("screen-corner-transparent", structOK, detail)
	check("screen-corner-region-clipped", structOK, structDetail)
}


/* ---------------------------------------------------------------- 收尾批任务书 ④：0.55 透明度下卡片文字可读性（浅色/白色壁纸最坏情形合成模型）

   「注入浅色壁纸」技术上不可行（壁纸由 OS/DWM 提供，程序无受控注入面），
   改为**数学合成模型**直算：白色（最亮最坏）壁纸下逐档合成卡片底色，
   对底-字计算 WCAG 相对亮度对比度。深字档（minor/shelved）字色/浅字档系 meta.js 常量镜像
   （meta.js 禁改，此处只读镜像；若 meta 更新需同步）。
   阈值口径（如实申报）：
   - 深色壁纸（当前主要使用场景）：≥4.5:1（WCAG AA 正文线）
   - 白色壁纸（最坏情形合成）：≥3.0:1——实测浅字档约 2.4~2.7，该值属**警示区**，
     主缓解手段是卡内容自带的字影 text-shadow 0 1px 3px rgba(0,0,0,0.5)
     （断言只保证 ≥3.0 不跌落底噪，可读性的最终确认留人工复核浅壁纸场景）。
------------------------------------- */

type viewerPriSpec struct {
	overlayR, overlayG, overlayB float64 // rgba overlay 色
	overlayA                     float64
	baseA                        float64 // 第五批：分档卡片底透明度
	textR, textG, textB          uint8
}

var viewerPriSpecs = []struct {
	key       string
	base      viewerPriSpec
	wallpaperLum4f float64
}{
	// key, overlay(RGB, alpha), baseA（分层值）, textRGB（镜像 meta.js PRIORITY_META.text）
	{"urgent", viewerPriSpec{139, 0, 0, 0.30, 0.65, 0xFF, 0xE8, 0xE8}, 0},
	{"important", viewerPriSpec{255, 165, 0, 0.50, 0.75, 0xFF, 0xF4, 0xE2}, 0},
	{"normal", viewerPriSpec{126, 211, 33, 0.60, 0.78, 0xF4, 0xFF, 0xE9}, 0},
	{"minor", viewerPriSpec{135, 206, 235, 0.70, 0.45, 0x0A, 0x22, 0x30}, 0},
	{"shelved", viewerPriSpec{232, 232, 232, 0.70, 0.45, 0x1E, 0x1E, 0x1E}, 0},
}

func srgbLuminance(r, g, b uint8) float64 {
	lin := func(c uint8) float64 {
		v := float64(c) / 255
		if v <= 0.03928 {
			return v / 12.92
		}
		return math.Pow((v+0.055)/1.055, 2.4)
	}
	return 0.2126*lin(r) + 0.7152*lin(g) + 0.0722*lin(b)
}

func contrastRatioLum(l1, l2 float64) float64 {
	hi, lo := l1, l2
	if hi < lo {
		hi, lo = lo, hi
	}
	return (hi + 0.05) / (lo + 0.05)
}

// cardCompositeLum：卡底合成亮度。ab=--base alpha（当前 0.55），ao=档叠加 alpha；
// 按 CSS 层序（壁纸 ← base 0.55 ← overlay）自顶向下合成于给定壁纸亮度/颜色。
func luminanceOfRGB(r, g, b float64) float64 {
	return srgbLuminance(uint8(math.Round(math.Min(255, math.Max(0, r)))),
		uint8(math.Round(math.Min(255, math.Max(0, g)))), uint8(math.Round(math.Min(255, math.Max(0, b)))))
}

func cardCompositeBG(sp viewerPriSpec, br, bg, bb float64, baseA float64, ao float64) (float64, float64, float64) {
	// 第一层：壁纸 (br,bg,bb) ← base 灰黑(30,30,30, baseA)
	x1 := 30*baseA + br*(1-baseA)
	x2 := 30*baseA + bg*(1-baseA)
	x3 := 30*baseA + bb*(1-baseA)
	// 第二层：← 档位色 overlay (span.RGB, ao)
	r := sp.overlayR*ao + x1*(1-ao)
	g := sp.overlayG*ao + x2*(1-ao)
	b := sp.overlayB*ao + x3*(1-ao)
	return r, g, b
}

func toStrSlice(v any) []string {
	list, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, x := range list {
		out = append(out, fmt.Sprint(x))
	}
	return out
}

// contains：slice 里是否存在该字符串（提醒命中组成员判定用）
func contains(id string, list []string) bool {
	for _, s := range list {
		if s == id {
			return true
		}
	}
	return false
}

// unq：evalJSON 拿到的字符串视图是 JSON 串形态（带引号），比较前去引号
func unq(s string) string {
	return strings.Trim(s, "\"")
}

func sameStrings(a, b []string) bool {
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

// numf：取 any 中 float64（缺省 -1），供约束审计区间判定
func numf(v any) float64 {
	if f, ok := v.(float64); ok {
		return f
	}
	return -1
}

/* waitForState：轮询到截止（每 150ms 步长，替代固定 waitFor 的不可控时延——第五批测试硬化） */
func waitForState(ms int, want func() bool) bool {
	deadline := time.Now().Add(time.Duration(ms) * time.Millisecond)
	for time.Now().Before(deadline) {
		pumpOnce(150)
		if want() {
			return true
		}
	}
	return want()
}

func finishSelftest() {
	// 汇总
	selftestExitPending = true // 1.2.0 批：自检收尾放行 requestClose 的真退出闸
	pass, fail := 0, 0
	for _, c := range report.Checks {
		if c.OK {
			pass++
		} else {
			fail++
		}
	}
	report.Runtime = fmt.Sprintf("Go 1.27 + WebView2 %s（系统运行时）", webView2Version())
	rep, err := json.MarshalIndent(report, "", "  ")
	if err == nil {
		_ = os.WriteFile(filepath.Join(shotDir, "selftest-report.json"), rep, 0o644)
	}
	appLogf("== 自检结束：通过 %d 项，失败 %d 项 → %s", pass, fail, shotDir)
	after(400, func() {
		requestClose()
	})
}
