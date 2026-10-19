//go:build windows

// bridge.go —— 前后端桥：替代 Electron 的 preload.js。
//
// 注入方式：chromium.Init(script)（= AddScriptToExecuteOnDocumentCreated，先于页面所有脚本执行，
// app.js 顶层就要用 window.api，因此必须走这个时机）。
//
// meta.js / parser.js 是 CommonJS 模块，浏览器里没有 module/require——
// Go 把**原文件源码**逐字节包进函数体注入（不做任何字符串转义/eval），
// 由一个 8 行的 __wbDefine 垫片提供 module.exports / require 语义。
// 因此 assets 里的 meta.js / parser.js 与 Electron 版**逐字节一致**。
//
// 上行：页面 window.chrome.webview.postMessage(JSON) → chromium.MessageCallback
// 下行：chromium.Eval —— 数据一律 base64 + TextDecoder 解码，规避转义问题。
package main

import (
	_ "embed"
)

//go:embed assets/src/shared/meta.js
var metaSrc []byte

//go:embed assets/src/main/parser.js
var parserSrc []byte

//go:embed assets/app.js
var rendererAppJS []byte // 页面通过 <script src="app.js"> 加载，这里 embed 仅为 SHA256 校验用

//go:embed assets/index.html
var indexHTML []byte

//go:embed assets/styles.css
var stylesCSS []byte

//go:embed assets/icon.ico
var iconIco []byte

// bridgeJS：window.api 的实现。约定：
//   上行  { t:'req',    id, type, payload }   请求-应答（Promise）
//   上行  { t:'event',  type, payload }       单向事件
//   上行  { t:'eval-reply', id, data }        自检取数回执
//   下行  __wbResolve(id, ok, data)           应答
//   下行  __wbPush(t, base64JSON)             事件推送
const bridgeJS = `
(function () {
  'use strict';
  if (window.api) return;

  var WB = { seq: 1, pending: {}, handlers: {} };

  function send(msg) { window.chrome.webview.postMessage(JSON.stringify(msg)); }

  function request(type, payload) {
    return new Promise(function (resolve, reject) {
      var id = WB.seq++;
      WB.pending[id] = { resolve: resolve, reject: reject };
      send({ t: 'req', id: id, type: type, payload: payload === undefined ? null : payload });
      setTimeout(function () {
        if (WB.pending[id]) { delete WB.pending[id]; reject(new Error('bridge timeout: ' + type)); }
      }, 20000);
    });
  }

  window.__wbResolve = function (id, ok, data) {
    var p = WB.pending[id];
    if (!p) return;
    delete WB.pending[id];
    if (ok) p.resolve(data); else p.reject(new Error(String(data && data.error || data)));
  };

  window.__wbEvalReply = function (id, data) { send({ t: 'eval-reply', id: id, data: data }); };

  window.__wbPush = function (t, b64) {
    var json = new TextDecoder('utf-8').decode(
      Uint8Array.from(atob(b64), function (c) { return c.charCodeAt(0); }));
    var data = JSON.parse(json);
    var hs = WB.handlers[t] || [];
    for (var i = 0; i < hs.length; i++) { try { hs[i](data); } catch (e) {} }
  };

  var meta = window.__wbModules['meta'];
  var parser = window.__wbModules['parser'];

  window.api = {
    meta: {
      PRIORITY_META: meta.PRIORITY_META,
      PRIORITY_ORDER: meta.PRIORITY_ORDER,
      CARD_BASE_CSS: meta.CARD_BASE_CSS,
      PANEL_CSS: meta.PANEL_CSS,
      DEFAULT_PRIORITY: meta.DEFAULT_PRIORITY
    },
    helpers: {
      sortTodos: meta.sortTodos,
      formatDateTime: meta.formatDateTime,
      describeRemain: meta.describeRemain,
      isOverdue: meta.isOverdue,
      normalizePriority: meta.normalizePriority,
      priorityRank: meta.priorityRank
    },
    load: function () { return request('data:load'); },
    save: function (payload) { return request('data:save', payload); },
    saveSync: function (payload) { return request('data:save-sync', payload); },
    // 解析器就是同一份 parser.js，在页面本地执行（与 Electron 行为一致）
    parse: function (text) { return Promise.resolve(parser.parseTodoInput(text)); },
    appInfo: function () { return request('app:info'); },

    addImagePaths: function (paths) { return request('image:add-paths', paths); },
    addImageDataUrls: function (list) { return request('image:add-dataurls', list); },
    removeImage: function (name) { return request('image:remove', name); },
    gcImages: function (refs) { return request('image:gc', refs); },
    // 独立看图窗（Win32+GDI，无 WebView2；第二批任务书）：返回 true/false
    viewImage: function (name) { return request('image:view', name); },
    // WebView2 拖进来的 File 拿不到真实路径 → 一律走 dataURL 导入（app.js 自带该回退分支）
    getPathForFile: function () { return null; },

    win: {
      minimize: function () { send({ t: 'event', type: 'win:minimize' }); },
      close: function () { send({ t: 'event', type: 'win:close' }); },
      closeConfirmed: function () { send({ t: 'event', type: 'win:close-confirmed' }); }, // 1.2.0 批：确认弹窗「退出」后回传
      toggleTop: function () { return request('win:toggle-top'); },
      setDockEnabled: function (v) { return request('win:set-dock', !!v); },
      undock: function () { send({ t: 'event', type: 'win:undock' }); return Promise.resolve(true); },
      expand: function () { send({ t: 'event', type: 'win:expand' }); return Promise.resolve(true); },
      retract: function () { send({ t: 'event', type: 'win:retract' }); return Promise.resolve(true); },
      setWidth: function (w) { return request('win:set-width', w); },
      hover: function (inside) { send({ t: 'event', type: 'win:hover', payload: !!inside }); return Promise.resolve(true); },
      openDataDir: function () { send({ t: 'event', type: 'app:open-data-dir' }); return Promise.resolve(true); },
      getState: function () { return request('win:get-state'); }
    },

    onWinState: function (cb) { (WB.handlers['win:state'] = WB.handlers['win:state'] || []).push(cb); },
    onDockState: function (cb) { (WB.handlers['win:dock'] = WB.handlers['win:dock'] || []).push(cb); },
    onDisplays: function (cb) { (WB.handlers['win:displays'] = WB.handlers['win:displays'] || []).push(cb); },
    // 1.2.0 批：关闭二次确认（X / WM_CLOSE / Alt+F4 → Go 推 win:close-request；托盘「退出」不走此路）
    onCloseRequest: function (cb) { (WB.handlers['win:close-request'] = WB.handlers['win:close-request'] || []).push(cb); }
  };

  /* ---- 标题栏拖动（替代 CSS -webkit-app-region: drag） ----
     Electron 版可拖区 = .titlebar（按钮/输入框除外）。 */
  document.addEventListener('mousedown', function (e) {
    if (e.button !== 0) return;
    var t = e.target;
    if (!t || !t.closest) return;
    if (t.closest('button, input, select, textarea, a')) return;
    if (t.closest('.modal') || t.closest('.lightbox')) return;
    var bar = t.closest('.titlebar');
    if (!bar) return;
    e.preventDefault();
    send({ t: 'event', type: 'win:drag-start' });
  }, true);
})();
`

// buildBootScript：把 meta.js / parser.js 原样包进函数体 + 桥实现，组成一份文档创建脚本。
func buildBootScript() string {
	s := "window.__wbModules = {};\n"
	s += "function __wbDefine(name, fn){ var m = { exports: {} }; fn(m, m.exports, function(n){ var k = n.split('/').pop(); return window.__wbModules[k]; }); window.__wbModules[name] = m.exports; }\n"
	s += "__wbDefine('meta', function(module, exports, require) {\n" + string(metaSrc) + "\n});\n"
	s += "__wbDefine('parser', function(module, exports, require) {\n" + string(parserSrc) + "\n});\n"
	s += bridgeJS
	// 自检用：固定参考时间的解析冒烟入口
	s += "\nwindow.__wbParseAt = function (text) {\n" +
		"  var r = window.__wbModules['parser'].parseTodoInput(text, new Date(2026, 9, 12, 14, 33, 0));\n" +
		"  return { content: r.content, priority: r.priority, dateCount: r.dateCount,\n" +
		"           deadline: r.deadline == null ? null : new Date(r.deadline).toLocaleString('zh-CN', { hour12: false }) };\n" +
		"};\n"
	return s
}
